// Package server registers catalog tools in an MCP server and runs the
// stdio or Streamable HTTP transport.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
)

// Options configures the MCP server.
type Options struct {
	Name         string
	Version      string
	Instructions string
	Logger       *slog.Logger
}

// New builds an MCP server exposing every tool of the catalog.
func New(tools []*catalog.Tool, pl *pipeline.Pipeline, opts Options) *mcp.Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Name == "" {
		opts.Name = "ocellusai-mcp"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: opts.Name, Version: opts.Version}, &mcp.ServerOptions{
		Instructions: opts.Instructions,
	})
	for _, t := range tools {
		srv.AddTool(&mcp.Tool{
			Name:        t.Spec.Name,
			Description: t.Spec.Description,
			InputSchema: t.Schema,
		}, handler(t, pl, opts.Logger))
	}
	return srv
}

func handler(t *catalog.Tool, pl *pipeline.Pipeline, logger *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		log := logger.With(
			"request_id", requestID(),
			"tool", t.Spec.Name,
			"worker", strings.Join(t.WorkerNames(), ","),
			"worker_type", strings.Join(t.WorkerTypes(), ","),
			"session", sessionID(req),
		)
		res, err := pl.Run(ctx, t, req.Params.Arguments)
		durationMS := time.Since(start).Milliseconds()
		if err != nil {
			stage := "internal"
			var perr *pipeline.Error
			if errors.As(err, &perr) {
				stage = perr.Stage
			}
			log.Error("tool call failed", "status", "error", "stage", stage, "duration_ms", durationMS, "error", err)
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("tool %s failed at stage %s: %v", t.Spec.Name, stage, unwrapStage(err))}},
			}, nil
		}
		log.Info("tool call ok", "status", "ok", "duration_ms", durationMS)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: res.Text}},
			StructuredContent: structuredContent(res.Structured),
		}, nil
	}
}

// structuredContent adapts a pipeline result to the structuredContent field.
// Protocol versions before SEP-2106 (2025-06-18, 2025-11-25) require a JSON
// object, and strict clients such as Claude Code reject the whole result
// otherwise, so arrays and primitives are wrapped as {"result": v}. A nil
// result omits the field.
func structuredContent(v any) any {
	switch v.(type) {
	case nil:
		return nil
	case map[string]any:
		return v
	default:
		return map[string]any{"result": v}
	}
}

func unwrapStage(err error) error {
	var perr *pipeline.Error
	if errors.As(err, &perr) {
		return perr.Err
	}
	return err
}

func sessionID(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	return req.Session.ID()
}

func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// RunStdio serves a single session over stdin/stdout until ctx is cancelled.
func RunStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler serves Streamable HTTP at /mcp plus /healthz.
func HTTPHandler(srv *mcp.Server, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Logger:                       logger,
		PropagateRequestCancellation: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// RunHTTP serves handler on listen until ctx is cancelled, then shuts down gracefully.
func RunHTTP(ctx context.Context, listen string, handler http.Handler) error {
	hs := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}
