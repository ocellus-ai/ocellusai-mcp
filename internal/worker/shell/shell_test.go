package shell

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	if cfg.Allowlist == nil {
		cfg.Allowlist = []string{"echo", "printf", "sh", "cat", "sleep", "true"}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewRequiresAllowlist(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected error")
	}
}

func TestValidate(t *testing.T) {
	w := newWorker(t, Config{})
	if err := w.Validate(map[string]any{"command": "echo", "args": []any{"a", 1}, "parse": "lines", "timeout": "1s", "stdin": "x"}); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	bad := map[string]map[string]any{
		"not allowlisted": {"command": "rm"},
		"path not listed": {"command": "/bin/echo"},
		"missing command": {"args": []any{"x"}},
		"unknown key":     {"command": "echo", "shell": true},
		"bad parse":       {"command": "echo", "parse": "yaml"},
		"bad timeout":     {"command": "echo", "timeout": "soon"},
		"args not list":   {"command": "echo", "args": "a b"},
		"nested args":     {"command": "echo", "args": []any{[]any{"x"}}},
		"template cmd":    {"command": "{{ .cmd }}"},
	}
	for name, req := range bad {
		if err := w.Validate(req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExecuteJSON(t *testing.T) {
	w := newWorker(t, Config{})
	out, err := w.Execute(context.Background(), map[string]any{"command": "echo", "args": []any{`{"a": 1, "b": [true]}`}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": float64(1), "b": []any{true}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %#v", out)
	}
}

func TestExecuteInvalidJSONFallsBackToString(t *testing.T) {
	w := newWorker(t, Config{})
	out, err := w.Execute(context.Background(), map[string]any{"command": "echo", "args": []any{"not json"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "not json\n" {
		t.Errorf("out = %#v", out)
	}
}

func TestExecuteEmptyJSONIsNull(t *testing.T) {
	w := newWorker(t, Config{})
	out, err := w.Execute(context.Background(), map[string]any{"command": "true"})
	if err != nil || out != nil {
		t.Errorf("out = %#v, err = %v", out, err)
	}
}

func TestExecuteLinesAndRaw(t *testing.T) {
	w := newWorker(t, Config{})
	out, err := w.Execute(context.Background(), map[string]any{"command": "printf", "args": []any{`a\n\nb\n`}, "parse": "lines"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []any{"a", "", "b"}) {
		t.Errorf("lines = %#v", out)
	}
	out, err = w.Execute(context.Background(), map[string]any{"command": "printf", "args": []any{`x y`}, "parse": "raw"})
	if err != nil || out != "x y" {
		t.Errorf("raw = %#v, %v", out, err)
	}
}

func TestExecuteStdin(t *testing.T) {
	w := newWorker(t, Config{})
	out, err := w.Execute(context.Background(), map[string]any{"command": "cat", "stdin": "from stdin", "parse": "raw"})
	if err != nil || out != "from stdin" {
		t.Errorf("out = %#v, %v", out, err)
	}
}

func TestExecuteExitCodeWithStderr(t *testing.T) {
	w := newWorker(t, Config{})
	_, err := w.Execute(context.Background(), map[string]any{"command": "sh", "args": []any{"-c", "echo boom >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "code 3") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

func TestExecuteTimeout(t *testing.T) {
	w := newWorker(t, Config{})
	start := time.Now()
	_, err := w.Execute(context.Background(), map[string]any{"command": "sleep", "args": []any{"5"}, "timeout": "100ms"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout did not kill the process promptly")
	}
}

func TestExecuteCancelled(t *testing.T) {
	w := newWorker(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Execute(ctx, map[string]any{"command": "sleep", "args": []any{"5"}})
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Errorf("err = %v", err)
	}
}

func TestExecuteEnvIsolation(t *testing.T) {
	t.Setenv("LEAKY", "yes")
	w := newWorker(t, Config{Env: map[string]string{"FOO": "bar"}})
	out, err := w.Execute(context.Background(), map[string]any{
		"command": "sh", "args": []any{"-c", `printf '%s|%s|%s' "$FOO" "${LEAKY:-unset}" "${PATH:+path}"`}, "parse": "raw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "bar|unset|path" {
		t.Errorf("env = %q", out)
	}
}

func TestExecuteTruncation(t *testing.T) {
	w := newWorker(t, Config{MaxOutputBytes: 8})
	big := strings.Repeat("x", 100) + "\n" + strings.Repeat("y", 100)
	out, err := w.Execute(context.Background(), map[string]any{"command": "printf", "args": []any{big}, "parse": "raw"})
	if err != nil {
		t.Fatal(err)
	}
	if s := out.(string); !strings.HasPrefix(s, "xxxxxxxx\n[truncated") {
		t.Errorf("raw truncated = %q", s)
	}
	out, err = w.Execute(context.Background(), map[string]any{"command": "printf", "args": []any{big}, "parse": "lines"})
	if err != nil {
		t.Fatal(err)
	}
	if l := out.([]any); len(l) != 2 || !strings.HasPrefix(l[1].(string), "[truncated") {
		t.Errorf("lines truncated = %#v", l)
	}
	_, err = w.Execute(context.Background(), map[string]any{"command": "printf", "args": []any{`{"k": "` + big + `"}`}, "parse": "json"})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("json truncated should be an error, got %v", err)
	}
}

func TestParse(t *testing.T) {
	logger := slog.Default()
	if v, _ := Parse(ParseLines, []byte("a\r\nb\n"), false, 10, logger); !reflect.DeepEqual(v, []any{"a", "b"}) {
		t.Errorf("lines = %#v", v)
	}
	if v, _ := Parse(ParseLines, nil, false, 10, logger); !reflect.DeepEqual(v, []any{}) {
		t.Errorf("empty lines = %#v", v)
	}
	v, err := Parse(ParseJSON, []byte(`{"a":1}`), true, 10, logger)
	if err != nil || v.(map[string]any)["truncated"] != true {
		t.Errorf("truncated object = %#v, %v", v, err)
	}
	if _, err := Parse(ParseJSON, []byte(`[1,2]`), true, 10, logger); err == nil {
		t.Error("truncated array should be an error")
	}
	if v, err := Parse(ParseJSON, []byte("  \n"), false, 10, logger); err != nil || v != nil {
		t.Errorf("blank json = %#v, %v", v, err)
	}
}
