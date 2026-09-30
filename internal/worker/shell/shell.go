// Package shell implements the "shell" worker: it runs an allow-listed binary
// with rendered arguments (no shell involved) and turns stdout into JSON.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// Type is the worker type referenced from config (`type: ...`); a config
// entry whose name equals the type may omit it.
const Type = "shell"

// Parse modes supported in request.parse.
const (
	ParseJSON  = "json"
	ParseLines = "lines"
	ParseRaw   = "raw"
)

const stderrLimit = 64 << 10

// Config configures the worker.
type Config struct {
	Allowlist      []string
	Timeout        time.Duration
	MaxOutputBytes int
	Env            map[string]string
	Logger         *slog.Logger
}

// Worker executes allow-listed commands.
type Worker struct {
	allow   map[string]struct{}
	timeout time.Duration
	maxOut  int
	env     []string
	logger  *slog.Logger
}

// New creates a worker. The child environment is PATH plus cfg.Env only.
func New(cfg Config) (*Worker, error) {
	if len(cfg.Allowlist) == 0 {
		return nil, errors.New("shell: allowlist must not be empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 1 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Worker{
		allow:   make(map[string]struct{}, len(cfg.Allowlist)),
		timeout: cfg.Timeout,
		maxOut:  cfg.MaxOutputBytes,
		logger:  cfg.Logger,
	}
	for _, b := range cfg.Allowlist {
		w.allow[b] = struct{}{}
	}
	if _, ok := cfg.Env["PATH"]; !ok {
		w.env = append(w.env, "PATH="+os.Getenv("PATH"))
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.env = append(w.env, k+"="+cfg.Env[k])
	}
	return w, nil
}

// Type implements worker.Worker.
func (w *Worker) Type() string { return Type }

// Validate implements worker.Worker for the raw request section.
func (w *Worker) Validate(req map[string]any) error {
	if err := worker.CheckKeys(req, "command", "args", "stdin", "parse", "timeout"); err != nil {
		return err
	}
	cmd, err := worker.RequiredString(req, "command")
	if err != nil {
		return err
	}
	if err := w.checkAllowed(cmd); err != nil {
		return err
	}
	if _, err := args(req); err != nil {
		return err
	}
	if _, err := worker.String(req, "stdin"); err != nil {
		return err
	}
	if _, err := parseMode(req); err != nil {
		return err
	}
	if _, err := w.requestTimeout(req); err != nil {
		return err
	}
	return nil
}

func (w *Worker) checkAllowed(cmd string) error {
	if _, ok := w.allow[cmd]; !ok {
		return fmt.Errorf("request.command: %q is not in workers.shell.allowlist", cmd)
	}
	return nil
}

func args(req map[string]any) ([]string, error) {
	v, ok := req["args"]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("request.args: must be a list, got %T", v)
	}
	out := make([]string, 0, len(list))
	for i, a := range list {
		switch x := a.(type) {
		case string:
			out = append(out, x)
		case int, int64, float64, bool:
			out = append(out, fmt.Sprint(x))
		default:
			return nil, fmt.Errorf("request.args[%d]: must be a scalar, got %T", i, a)
		}
	}
	return out, nil
}

func parseMode(req map[string]any) (string, error) {
	mode, err := worker.String(req, "parse")
	if err != nil {
		return "", err
	}
	switch mode {
	case "":
		return ParseJSON, nil
	case ParseJSON, ParseLines, ParseRaw:
		return mode, nil
	default:
		return "", fmt.Errorf("request.parse: must be json, lines or raw, got %q", mode)
	}
}

func (w *Worker) requestTimeout(req map[string]any) (time.Duration, error) {
	s, err := worker.String(req, "timeout")
	if err != nil {
		return 0, err
	}
	if s == "" {
		return w.timeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("request.timeout: %w", err)
	}
	if d <= 0 {
		return 0, errors.New("request.timeout: must be positive")
	}
	return d, nil
}

// Execute implements worker.Worker.
func (w *Worker) Execute(ctx context.Context, req map[string]any) (any, error) {
	command, err := worker.RequiredString(req, "command")
	if err != nil {
		return nil, err
	}
	if err := w.checkAllowed(command); err != nil {
		return nil, err
	}
	argv, err := args(req)
	if err != nil {
		return nil, err
	}
	stdin, err := worker.String(req, "stdin")
	if err != nil {
		return nil, err
	}
	mode, err := parseMode(req)
	if err != nil {
		return nil, err
	}
	timeout, err := w.requestTimeout(req)
	if err != nil {
		return nil, err
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", command, err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Env = w.env
	cmd.WaitDelay = 2 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdout := &limitedBuffer{limit: w.maxOut}
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	w.logger.Debug("exec", "command", command, "args", argv)
	runErr := cmd.Run()
	errText := strings.TrimSpace(stderr.String())

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("command %s timed out after %s%s", command, timeout, suffix(errText))
	case ctx.Err() != nil:
		return nil, fmt.Errorf("command %s cancelled: %w", command, ctx.Err())
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("command %s exited with code %d%s", command, exitErr.ExitCode(), suffix(errText))
		}
		return nil, fmt.Errorf("command %s: %w", command, runErr)
	}
	if errText != "" {
		w.logger.Info("command wrote to stderr", "command", command, "stderr", errText)
	}
	if stdout.truncated {
		w.logger.Warn("stdout truncated", "command", command, "max_output_bytes", w.maxOut)
	}
	return Parse(mode, stdout.Bytes(), stdout.truncated, w.maxOut, w.logger)
}

func suffix(stderr string) string {
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

// Parse converts stdout bytes into a JSON-compatible value according to mode.
// json: invalid JSON is returned as a string (never a tool error); truncated
// output is flagged with truncated: true on objects and is an error otherwise.
// lines/raw cannot fail; truncation appends a marker.
func Parse(mode string, out []byte, truncated bool, limit int, logger *slog.Logger) (any, error) {
	marker := fmt.Sprintf("[truncated: output exceeded %d bytes]", limit)
	switch mode {
	case ParseRaw:
		s := string(out)
		if truncated {
			s += "\n" + marker
		}
		return s, nil
	case ParseLines:
		s := strings.TrimSuffix(string(out), "\n")
		lines := []any{}
		if s != "" {
			for _, l := range strings.Split(s, "\n") {
				lines = append(lines, strings.TrimSuffix(l, "\r"))
			}
		}
		if truncated {
			lines = append(lines, marker)
		}
		return lines, nil
	default: // json
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			if truncated {
				return nil, fmt.Errorf("output exceeded %d bytes and the truncated JSON cannot be parsed", limit)
			}
			logger.Warn("stdout is not valid JSON, returning it as a string", "error", err)
			return string(out), nil
		}
		if truncated {
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("output exceeded %d bytes; %s", limit, marker)
			}
			obj["truncated"] = true
		}
		return v, nil
	}
}

// limitedBuffer keeps at most limit bytes and records whether more arrived.
// It never returns an error so the child process is not killed by a broken pipe.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }
