// Command ocellusai-mcp exposes YAML-described DevOps tools to AI agents over MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/config"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/anomaly"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/anomalymv"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/clusterevents"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/ensemble"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/join"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/jq"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/outliers"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/ssaproc"
	"github.com/ocellus-ai/ocellusai-mcp/internal/server"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/prometheus"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/rest"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/shell"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ocellusai-mcp:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("ocellusai-mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	validate := fs.Bool("validate", false, "load config and tool catalog, print the tools and exit")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// Logs always go to stderr: on the stdio transport stdout is the protocol channel.
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	workers, err := buildWorkers(cfg, logger)
	if err != nil {
		return err
	}
	processors, err := buildProcessors()
	if err != nil {
		return err
	}
	tools, err := catalog.Load(cfg.ToolsDir, workers, processors)
	if err != nil {
		return err
	}
	logger.Info("catalog loaded", "tools_dir", cfg.ToolsDir, "tools", len(tools), "workers", workers.Names(), "processors", processors.Names())

	if *validate {
		for _, t := range tools {
			fmt.Printf("%-32s worker=%-12s type=%-10s calls=%-12s process=%-12s file=%s\n", t.Spec.Name,
				strings.Join(t.WorkerNames(), ","), strings.Join(t.WorkerTypes(), ","), callNames(t), stepNames(t), t.File)
		}
		return nil
	}

	srv := server.New(tools, pipeline.New(logger), server.Options{
		Name:         "ocellusai-mcp",
		Version:      version,
		Instructions: "DevOps tools backed by Prometheus, HTTP APIs and cluster CLIs. Tool descriptions state what each returns and whether it changes anything.",
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Server.Transport {
	case config.TransportStdio:
		logger.Info("serving MCP over stdio", "version", version)
		if err := server.RunStdio(ctx, srv); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case config.TransportHTTP:
		logger.Info("serving MCP over streamable HTTP", "listen", cfg.Server.Listen, "endpoint", "/mcp", "version", version)
		if err := server.RunHTTP(ctx, cfg.Server.Listen, server.HTTPHandler(srv, logger)); err != nil {
			return err
		}
	}
	logger.Info("shutdown complete")
	return nil
}

// buildWorkers creates one worker per configured instance. The instance name
// is what tools reference (`worker: <name>`); the type selects the code.
func buildWorkers(cfg *config.Config, logger *slog.Logger) (worker.Registry, error) {
	reg := worker.Registry{}
	for _, name := range cfg.Workers.Names() {
		w, err := newWorker(cfg.Workers[name], logger)
		if err != nil {
			return nil, fmt.Errorf("workers.%s: %w", name, err)
		}
		if err := reg.Register(name, w); err != nil {
			return nil, err
		}
		logger.Info("worker registered", "name", name, "type", w.Type())
	}
	if len(reg) == 0 {
		return nil, errors.New("no workers configured: add at least one instance under workers (e.g. workers.prometheus.url)")
	}
	return reg, nil
}

// newWorker is the factory by type: a new worker type adds one case here.
func newWorker(inst *config.Worker, logger *slog.Logger) (worker.Worker, error) {
	switch inst.Type {
	case config.WorkerPrometheus:
		p := inst.Prometheus
		return prometheus.New(prometheus.Config{URL: p.URL, Timeout: p.Timeout, Headers: p.Headers, Logger: logger})
	case config.WorkerShell:
		s := inst.Shell
		return shell.New(shell.Config{Allowlist: s.Allowlist, Timeout: s.Timeout, MaxOutputBytes: s.MaxOutputBytes, Env: s.Env, Logger: logger})
	case config.WorkerRest:
		r := inst.Rest
		return rest.New(rest.Config{
			URL: r.URL, Timeout: r.Timeout, Headers: r.Headers, MaxResponseBytes: r.MaxResponseBytes,
			Methods: r.Methods, CAFile: r.CAFile, InsecureSkipVerify: r.InsecureSkipVerify, Logger: logger,
		})
	default:
		return nil, fmt.Errorf("unsupported worker type %q", inst.Type)
	}
}

// buildProcessors registers every processor. Processors have no config
// section: everything they need comes from the tool's `with`.
func buildProcessors() (processor.Registry, error) {
	reg := processor.Registry{}
	if err := reg.Register(anomaly.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(anomalymv.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(clusterevents.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(ensemble.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(jq.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(join.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(outliers.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(ssaproc.NewUni()); err != nil {
		return nil, err
	}
	if err := reg.Register(ssaproc.NewMulti()); err != nil {
		return nil, err
	}
	return reg, nil
}

// callNames renders the calls: chain for the -validate table ("-" for the
// single-call form).
func callNames(t *catalog.Tool) string {
	if !t.Named() {
		return "-"
	}
	names := make([]string, len(t.Calls))
	for i, c := range t.Calls {
		names[i] = c.Name
	}
	return strings.Join(names, ">")
}

// stepNames renders the process chain for the -validate table ("-" when empty).
func stepNames(t *catalog.Tool) string {
	if len(t.Steps) == 0 {
		return "-"
	}
	names := make([]string, len(t.Steps))
	for i, s := range t.Steps {
		names[i] = s.Fn
	}
	return strings.Join(names, ">")
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
