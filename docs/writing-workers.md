# Writing a worker

[Architecture](architecture.md) · [Components](components.md) · **Writing a worker** · [Writing a processor](writing-processors.md)

A worker type is the code that talks to one kind of data source. Ocellus AI has three: `prometheus`, `shell` and
`rest`. This guide explains when a new type is worth writing, what the server expects from it, and then builds one
step by step: a `file` worker that reads files under a configured directory. Every code block of the tutorial
compiles, passes its tests and `golangci-lint`, and the example tool was called through the real binary.

Read [Architecture](architecture.md) first: it explains the pipeline a worker plugs into and the rules about values,
errors and concurrency that this guide applies.

## Contents

- [Do you need a new worker?](#do-you-need-a-new-worker)
- [How the server uses a worker](#how-the-server-uses-a-worker)
- [The contract](#the-contract)
- [Tutorial: a file worker](#tutorial-a-file-worker)
  - [1. Design the instance and the request](#1-design-the-instance-and-the-request)
  - [2. The package](#2-the-package)
  - [3. Unit tests](#3-unit-tests)
  - [4. The config section](#4-the-config-section)
  - [5. The factory](#5-the-factory)
  - [6. Try it](#6-try-it)
- [A tool test](#a-tool-test)
- [Patterns from the built-in workers](#patterns-from-the-built-in-workers)
- [Documentation](#documentation)
- [Checklist](#checklist)
- [Common mistakes](#common-mistakes)

## Do you need a new worker?

Many sources need no code at all:

| Source | Use |
|---|---|
| Anything that serves the Prometheus query API: VictoriaMetrics, Thanos, Mimir, Cortex | a `prometheus` instance |
| A JSON HTTP API with static credentials: Alertmanager, GitLab, Grafana, an internal service | a `rest` instance and tool files |
| A command-line program that prints JSON or text | a `shell` instance with the program on its allowlist |

A new worker type is worth writing when:

- **the protocol is not HTTP with JSON**: SQL databases, gRPC, LDAP, DNS, SNMP;
- **authentication is more than static headers**: tokens that expire and must be refreshed, request signing such as
  AWS Signature V4, client certificates;
- **an official client library does the job better** than raw requests: the Kubernetes API through client-go, a
  cloud provider's SDK;
- **one answer needs several requests or a format jq cannot read**: pagination, streaming, binary data;
- **the reach must be narrower than a generic worker can enforce**: read-only SQL with an allowlist of statements,
  files under one directory.

A worker fetches data. It does not analyse it: summaries, anomalies and forecasts belong to jq and processors, which
work the same for every source.

## How the server uses a worker

```
config.yaml                       tool file                        each call
workers:                          worker: inventory                rendered request
  inventory:                      request:                               │
    type: file      ──New──▶ worker value ──Validate(raw request)──▶ Execute(ctx, request) ──▶ value
    root: /srv/inv    (once per instance)   (once per call section,     (concurrently)
                                             at startup)
```

1. At startup `newWorker` in `cmd/ocellusai-mcp/main.go` calls your constructor once for every instance of your type,
   with that instance's settings. The value is registered under the instance name.
2. The catalog calls `Validate(request)` once for every call section of a tool that names one of your instances,
   with the request exactly as written in the YAML.
3. On every tool call the pipeline renders the request and calls `Execute(ctx, request)`. Calls of different
   sessions run concurrently on the same value.
4. The value you return goes to the call's jq, the processors and the answer.

## The contract

```go
type Worker interface {
    Type() string
    Validate(req map[string]any) error
    Execute(ctx context.Context, req map[string]any) (any, error)
}
```

### Type

`Type` returns the type name, the value of `type:` in the config. By convention the package exports it as a
constant `Type`. It appears in the logs (`worker_type`) and in the `-validate` table.

### Validate

`Validate` receives the raw `request` section of a tool: a `map[string]any` as YAML decoded it. It runs at startup,
so a mistake in a tool file stops the server with the file and the field. It should:

- reject unknown keys with `worker.CheckKeys(req, "key1", "key2", …)`;
- require the keys the request cannot do without (`worker.RequiredString`);
- check every literal value completely: types, allowed values, ranges, paths;
- skip values that are still templates (`worker.IsTemplate`); `Execute` checks them after rendering;
- check the request against the instance: a command against the allowlist, a method against the allowed methods.
  `Validate` is a method of the instance, so it has the instance's settings at hand;
- not contact the source. `-validate` and the server's start must work while the source is down.

Some keys are better forced to be literals. If a key decides what kind of operation runs or how the result is shaped
(`method` of `rest`, `command` of `shell`, `parse` of the tutorial's worker), a template there would make the tool
impossible to check at startup and could let an argument choose the operation. Reject a template in such a key:
`request.method: must be a literal, not a template`.

Errors start with `request.<key>:`. The catalog adds the file name and, for tools with `calls`, the call.

### Execute

`Execute` receives the same map after rendering: every string that contained `{{` has been replaced by the output of
its template. It should:

- check the rendered values again, because a template can produce anything;
- apply a time limit: the instance's default, optionally overridden by a `timeout` key of the request;
- do the work, passing the context to every I/O operation;
- limit how much it reads;
- return a JSON-compatible value or an error.

### Values you receive

| In the tool file | In `Validate` | In `Execute` |
|---|---|---|
| `limit: 50` | `int` 50 | `int` 50 |
| `ratio: 0.5` | `float64` 0.5 | `float64` 0.5 |
| `limit: "{{ .limit }}"` | `string` `"{{ .limit }}"` | `string` `"50"` |
| `args: [get, "{{ .ns }}"]` | `[]any{"get", "{{ .ns }}"}` | `[]any{"get", "prod"}` |
| `verbose: true` | `bool` | `bool` |
| `path: "{{ .p }}"` with `p` set to `""` | the template | `""` |
| `path: "{{ .p }}"` with `p` not passed and no default | the template | `"<no value>"` (Go templates print nil this way) |
| key not written | absent | absent |

So read a number both as a number and as a numeric string, and decide what an empty string means: the built-in
workers treat it as "not set" (`rest` drops a query parameter that renders empty; `prometheus` uses the default time).
Tool authors give optional parameters a default (`default: ""`) or guard them in the template; the worker cannot
tell `<no value>` from a real value, so it validates rendered values like any other input.
`worker.String` and `worker.RequiredString` read strings with the right error messages; write small helpers for
other types in your package.

### Values you return

Return only `map[string]any`, `[]any`, `string`, `float64`, `int`, `bool` and `nil`. Processors receive your value
as is, so a `[]string`, a `map[string]float64` or a struct breaks the first processor that reads it.

- JSON decoded with `encoding/json` into an `any` is already in this form.
- Convert the types of a client library yourself, as `prometheus.Normalize` does for the Prometheus client.
- Keep the source's own shape. Tool authors know it from the source's documentation, and jq written against the
  source's API works unchanged. Shaping and summarizing are jq's job.
- If the source returns time series, consider returning a Prometheus matrix
  (`{resultType: matrix, result: [{metric, values}]}`): the time-series processors accept it directly.

### Errors

Return an error; never panic. The pipeline reports it at stage `worker`, prefixed with the call for tools with
`calls`, and the agent reads it:

```
tool inventory_hosts failed at stage worker: hosts/staging.json: no such file under the instance root
```

- Start with the operation: `GET /api/v2/silences: 404 Not Found: …`, `hosts/prod.json: …`.
- Say what was expected, and when you can, how to fix it.
- Name timeouts as such: `request timed out after 15s`.
- Keep secrets, full URLs with query strings and paths on the server out of messages. Errors from `net/http` contain
  the full URL; unwrap them (see `requestError` in the `rest` worker).

### Time limits and cancellation

The context carries the cancellation of the call: the client cancelled it or went away, or the server is shutting
down. Add your own limit on top of it, and pass the result to every blocking operation:

```go
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()
req, err := http.NewRequestWithContext(ctx, method, url, body)   // or exec.CommandContext, db.QueryContext, …
```

Report `context.DeadlineExceeded` as a timeout with its duration and `context.Canceled` as a cancellation.

### Concurrency

One worker value serves every call to its instance, concurrently. Keep only the configuration and clients that are
safe for concurrent use (an `http.Client`, a `*sql.DB`) in the struct; everything about one call lives in local
variables of `Execute`. A cache or a pool shared between calls needs a mutex. `make test-race` finds most mistakes
here.

### Secrets and logs

Credentials come from the instance's settings, which can take them from the environment (`${TOKEN}`). Keep them
inside the worker and apply them where the tool cannot override them: the `rest` worker sets the instance's headers
after the tool's, the `prometheus` worker adds its headers in an `http.RoundTripper`. Never return them in the value
and never put them in errors.

Log with the `*slog.Logger` from your config. Use `Debug` for the operation, the target, the status, the size and
the duration; `Warn` for something the operator should see (a truncated output, a warning from the source). Never
log headers, query strings or environment values.

### The constructor

By convention a worker package has:

```go
const Type = "file"

type Config struct { … ; Logger *slog.Logger }

func New(cfg Config) (*Worker, error)
```

- `Config` is a struct of the worker package, not of the `config` package, so the worker can be created in tests
  without YAML and does not depend on the config format.
- Apply defaults for zero values in `New` too, so the package works on its own; the `config` package applies the same
  defaults when it loads the instance.
- Check the settings that need the worker's code (a URL that must parse, a CA file that must be readable, a directory
  that must exist) and return an error; `main.go` prefixes it with `workers.<instance>:`.
- Do not connect to the source.

## Tutorial: a file worker

The `file` worker reads one file under a directory set in the instance and returns its content as data. It suits
data that lives in files: a host inventory exported by Terraform or Ansible, an on-call rota, status files written
by scheduled jobs. It needs no external service, so it is easy to follow and test, and it shows every part of a
worker: a reach limited by the operator (a root directory, the way `rest` has a base URL), a template in the request,
a size limit and errors that do not reveal server paths.

### 1. Design the instance and the request

Start from what the operator writes in the config and what a tool author writes in a tool file.

The instance:

```yaml
workers:
  inventory:
    type: file
    root: /etc/ocellus/inventory   # required: the directory tools read from
    max_bytes: 1048576             # default 1 MiB; a larger file is an error
```

The request:

```yaml
worker: inventory
request:
  path: "hosts/{{ .env }}.json"   # required, relative to root, a template
  parse: json                     # json (default) | lines | raw; a literal
```

What comes back:

| `parse` | Result |
|---|---|
| `json` | the parsed JSON; an empty file is `null`; anything that is not JSON is an error |
| `lines` | a list of strings, one per line, without `\r` and without the final newline |
| `raw` | the whole file as one string |

The decisions behind it:

- **The root belongs to the instance.** A tool gives only a relative path. A path that is absolute or climbs out with
  `..` is rejected, at startup if it is a literal and on every call after rendering. The file is opened with
  `os.OpenInRoot`, which also refuses symbolic links that point outside the root, so a link placed in the directory
  cannot widen the reach.
- **`parse` is a literal.** It decides the shape of the result, which the tool's jq depends on.
- **JSON is strict.** A file the tool declares as JSON but that does not parse is a mistake to report, not text to
  pass on.
- **Errors name the relative path only.** The root is a path on the server.

### 2. The package

`internal/worker/file/file.go`:

```go
// Package file implements the "file" worker: it reads one file under the
// instance's root directory and returns its content as JSON-compatible data.
// The root and the size limit belong to the instance; a tool gives a path
// relative to the root and can leave it neither with ".." nor through a
// symbolic link.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// Type is the worker type referenced from config (`type: file`).
const Type = "file"

// Parse modes supported in request.parse.
const (
	ParseJSON  = "json"
	ParseLines = "lines"
	ParseRaw   = "raw"
)

const defaultMaxBytes = 1 << 20

// Config configures the worker.
type Config struct {
	Root     string // directory every request path is relative to
	MaxBytes int    // default 1 MiB; a larger file is an error
	Logger   *slog.Logger
}

// Worker reads files under one root directory.
type Worker struct {
	root     string
	maxBytes int
	logger   *slog.Logger
}

// New creates a worker. The root must exist and be a directory.
func New(cfg Config) (*Worker, error) {
	if cfg.Root == "" {
		return nil, errors.New("file: root is required")
	}
	info, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("file: root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("file: root: %s is not a directory", cfg.Root)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{root: cfg.Root, maxBytes: cfg.MaxBytes, logger: cfg.Logger}, nil
}

// Type implements worker.Worker.
func (w *Worker) Type() string { return Type }

// Validate implements worker.Worker for the raw request section. Literal
// values are checked here; a templated path is checked after rendering.
func (w *Worker) Validate(req map[string]any) error {
	if err := worker.CheckKeys(req, "path", "parse"); err != nil {
		return err
	}
	p, err := worker.RequiredString(req, "path")
	if err != nil {
		return err
	}
	if !worker.IsTemplate(p) {
		if err := checkPath(p); err != nil {
			return err
		}
	}
	_, err = parseMode(req)
	return err
}

// Execute implements worker.Worker.
func (w *Worker) Execute(ctx context.Context, req map[string]any) (any, error) {
	p, err := worker.RequiredString(req, "path")
	if err != nil {
		return nil, err
	}
	if err := checkPath(p); err != nil {
		return nil, err
	}
	mode, err := parseMode(req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// OpenInRoot refuses names that resolve outside the root, including
	// through symbolic links, so the check above is not the only guard.
	f, err := os.OpenInRoot(w.root, filepath.FromSlash(p))
	if err != nil {
		return nil, pathError(p, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(w.maxBytes)+1))
	if err != nil {
		return nil, pathError(p, err)
	}
	if len(data) > w.maxBytes {
		return nil, fmt.Errorf("%s: file exceeds %d bytes (max_bytes of the instance)", p, w.maxBytes)
	}
	w.logger.Debug("file read", "path", p, "bytes", len(data))
	return parse(mode, p, data)
}

// checkPath accepts a relative path that stays inside the root.
func checkPath(p string) error {
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return fmt.Errorf("request.path: %q must be relative to the instance root and stay inside it", p)
	}
	return nil
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
		if worker.IsTemplate(mode) {
			return "", errors.New("request.parse: must be a literal, not a template")
		}
		return "", fmt.Errorf("request.parse: must be json, lines or raw, got %q", mode)
	}
}

// pathError reports a failure by the request path only: the root, a server
// path, stays out of the message the agent reads.
func pathError(p string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: no such file under the instance root", p)
	}
	return fmt.Errorf("%s: %w", p, err)
}

func parse(mode, p string, data []byte) (any, error) {
	switch mode {
	case ParseRaw:
		return string(data), nil
	case ParseLines:
		text := strings.TrimSuffix(string(data), "\n")
		if text == "" {
			return []any{}, nil
		}
		parts := strings.Split(text, "\n")
		out := make([]any, len(parts))
		for i, line := range parts {
			out[i] = strings.TrimSuffix(line, "\r")
		}
		return out, nil
	default:
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, nil
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("%s: not valid JSON: %w (use parse: lines or raw for text files)", p, err)
		}
		return v, nil
	}
}
```

How it follows the contract:

- **`New`** checks what only this code can check (the root exists and is a directory), applies the default limit and
  does no other I/O.
- **`Validate`** rejects unknown keys, requires `path`, checks a literal path and leaves a templated one for later;
  `parse` must be a valid literal.
- **`Execute`** checks the rendered path again, stops if the call is already cancelled, opens the file inside the
  root, reads at most one byte more than the limit (to tell "exactly at the limit" from "over it") and parses.
  Reading a local file does not block for long, so a check of the context before the work is enough; a worker that
  waits on the network passes the context to the operation itself.
- **The result** is built from `string`, `[]any` and what `encoding/json` produces, so it is JSON-compatible.
- **Errors** carry the relative path and the reason (`pathError` drops the `*fs.PathError` wrapper, which may hold
  more of the path), and suggest `parse: lines` when a text file is read as JSON.
- **Concurrency**: the struct holds only settings, every call works on its own file handle.

### 3. Unit tests

`internal/worker/file/file_test.go`. Test what the contract promises: `Validate` on literals and templates, every
`parse` mode, the root as a boundary, the limits and the error texts.

```go
package file

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newWorker writes files (relative path → content) under a fresh root and
// returns a worker for it.
func newWorker(t *testing.T, files map[string]string) *Worker {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New(Config{Root: root, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestValidate(t *testing.T) {
	w := newWorker(t, nil)
	for name, tc := range map[string]struct {
		req  map[string]any
		want string // "" = valid
	}{
		"plain":         {map[string]any{"path": "hosts/prod.json"}, ""},
		"template path": {map[string]any{"path": "hosts/{{ .env }}.json", "parse": "json"}, ""},
		"lines":         {map[string]any{"path": "a.txt", "parse": "lines"}, ""},
		"unknown key":   {map[string]any{"path": "a", "mode": "x"}, "unknown field(s) mode"},
		"no path":       {map[string]any{"parse": "raw"}, "request.path: required"},
		"absolute":      {map[string]any{"path": "/etc/passwd"}, "must be relative"},
		"climbs out":    {map[string]any{"path": "../secret.json"}, "must be relative"},
		"bad parse":     {map[string]any{"path": "a", "parse": "yaml"}, "must be json, lines or raw"},
		"parse tmpl":    {map[string]any{"path": "a", "parse": "{{ .p }}"}, "must be a literal"},
	} {
		err := w.Validate(tc.req)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}

func TestExecuteParse(t *testing.T) {
	w := newWorker(t, map[string]string{
		"hosts/prod.json": `{"hosts": [{"name": "db-1", "port": 5432}]}`,
		"oncall.txt":      "alice\r\nbob\n",
		"empty.json":      "\n",
	})
	ctx := context.Background()
	got, err := w.Execute(ctx, map[string]any{"path": "hosts/prod.json"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"hosts": []any{map[string]any{"name": "db-1", "port": float64(5432)}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json = %#v", got)
	}
	if got, _ := w.Execute(ctx, map[string]any{"path": "oncall.txt", "parse": "lines"}); !reflect.DeepEqual(got, []any{"alice", "bob"}) {
		t.Errorf("lines = %#v", got)
	}
	if got, _ := w.Execute(ctx, map[string]any{"path": "oncall.txt", "parse": "raw"}); got != "alice\r\nbob\n" {
		t.Errorf("raw = %#v", got)
	}
	if got, err := w.Execute(ctx, map[string]any{"path": "empty.json"}); got != nil || err != nil {
		t.Errorf("empty = %#v, %v", got, err)
	}
}

func TestExecuteStaysInRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"token": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newWorker(t, nil)
	if _, err := w.Execute(context.Background(), map[string]any{"path": "../secret.json"}); err == nil || !strings.Contains(err.Error(), "must be relative") {
		t.Errorf("..: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(w.root, "link.json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	_, err := w.Execute(context.Background(), map[string]any{"path": "link.json"})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("symlink out of the root: %v", err)
	}
}

func TestExecuteErrors(t *testing.T) {
	w := newWorker(t, map[string]string{
		"big.json":  `"` + strings.Repeat("x", 2000) + `"`,
		"text.json": "not json",
	})
	ctx := context.Background()
	for path, want := range map[string]string{
		"missing.json": "missing.json: no such file under the instance root",
		"big.json":     "exceeds 1024 bytes",
		"text.json":    "not valid JSON",
	} {
		_, err := w.Execute(ctx, map[string]any{"path": path})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
		if err != nil && strings.Contains(err.Error(), w.root) {
			t.Errorf("%s: the error leaks the root: %v", path, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.Execute(cancelled, map[string]any{"path": "text.json"}); err == nil {
		t.Error("a cancelled context must stop the call")
	}
}

func TestNew(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"no root":   {},
		"missing":   {Root: filepath.Join(t.TempDir(), "nope")},
		"not a dir": {Root: notDir},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
```

```bash
go test -race ./internal/worker/file/
```

### 4. The config section

The config knows every worker type, so that it can decode each instance strictly. Five places in
`internal/config/config.go` change.

The type name and the list of types:

```go
	WorkerRest       = "rest"
	WorkerFile       = "file"
)

// WorkerTypes lists the supported worker types in display order.
var WorkerTypes = []string{WorkerPrometheus, WorkerShell, WorkerRest, WorkerFile}
```

The settings struct and its pointer in `Worker`. The `yaml` tags are the keys of the instance; `checkFields`
derives the list of allowed keys in error messages from them:

```go
	Rest       *Rest
	File       *File
}
```

```go
// File configures a file instance: files under one root directory.
type File struct {
	Root     string `yaml:"root"`
	MaxBytes int    `yaml:"max_bytes"`
}
```

A `case` in `decodeWorker`, which picks the struct by type:

```go
	case WorkerFile:
		inst.File = &File{}
		target = inst.File
```

The defaults, in `applyDefaults`:

```go
		if f := inst.File; f != nil && f.MaxBytes == 0 {
			f.MaxBytes = 1 << 20
		}
```

The checks that need no code of the worker, in `Worker.validate`. Whether the directory exists is left to `file.New`:

```go
	case WorkerFile:
		f := w.File
		if f == nil {
			return fmt.Errorf("%s: file settings missing", prefix)
		}
		if strings.TrimSpace(f.Root) == "" {
			return fmt.Errorf("%s.root: required", prefix)
		}
		if f.MaxBytes < 0 {
			return fmt.Errorf("%s.max_bytes: must not be negative", prefix)
		}
```

A test in `internal/config/config_test.go`:

```go
func TestParseFileInstance(t *testing.T) {
	cfg, err := Parse([]byte("workers:\n  inventory:\n    type: file\n    root: /srv/inventory\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Workers["inventory"].File
	if f == nil || f.Root != "/srv/inventory" || f.MaxBytes != 1<<20 {
		t.Fatalf("file = %+v", f)
	}
	for yml, want := range map[string]string{
		"workers:\n  inventory:\n    type: file\n":                                    "workers.inventory.root: required",
		"workers:\n  inventory:\n    type: file\n    root: /srv\n    max_bytes: -1\n": "workers.inventory.max_bytes: must not be negative",
		"workers:\n  inventory:\n    type: file\n    root: /srv\n    url: http://x\n": "unknown field(s) url (allowed: max_bytes, root, type) (type file)",
	} {
		if _, err := Parse([]byte(yml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", yml, err, want)
		}
	}
}
```

### 5. The factory

`newWorker` in `cmd/ocellusai-mcp/main.go` turns an instance's settings into a worker. Add the import
`"github.com/ocellus-ai/ocellusai-mcp/internal/worker/file"` and a `case`:

```go
	case config.WorkerFile:
		f := inst.File
		return file.New(file.Config{Root: f.Root, MaxBytes: f.MaxBytes, Logger: logger})
```

`buildWorkers` needs no change: it creates and registers every instance whatever its type, and prefixes a
constructor's error with the instance name. A test in `cmd/ocellusai-mcp/main_test.go` checks both:

```go
func TestBuildWorkersFile(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Parse([]byte("workers:\n  inventory: {type: file, root: \"" + root + "\"}\n  broken: {type: file, root: \"" + root + "/missing\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildWorkers(cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "workers.broken: file: root:") {
		t.Fatalf("expected the missing root to stop the start, got %v", err)
	}
	delete(cfg.Workers, "broken")
	reg, err := buildWorkers(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if reg["inventory"].Type() != "file" {
		t.Errorf("type = %q", reg["inventory"].Type())
	}
}
```

Then run the whole suite and the linters:

```bash
make vet test-race lint
```

### 6. Try it

An inventory file, `inventory/hosts/prod.json`:

```json
{"hosts": [
  {"name": "db-1",  "role": "postgres", "address": "10.0.1.11"},
  {"name": "db-2",  "role": "postgres", "address": "10.0.1.12"},
  {"name": "web-1", "role": "nginx",    "address": "10.0.2.21"}
]}
```

A tool that reads it, `tools/inventory_hosts.yaml`:

```yaml
name: inventory_hosts
description: |
  Hosts of an environment from the inventory files: name, role and address,
  optionally only one role. Use it to find which hosts serve a role before
  asking Prometheus about them.
worker: inventory

params:
  env:  {type: string, enum: [prod, staging], default: prod, description: "Environment"}
  role: {type: string, default: "", pattern: "^[a-z0-9-]*$", description: "Only hosts with this role; empty for all"}

request:
  path: "hosts/{{ .env }}.json"

response:
  jq: '[.hosts[] | select($params.role == "" or .role == $params.role) | {name, role, address}]'
  render: |
    {{ len . }} host(s) in {{ param "env" }}{{ with param "role" }} with role {{ . }}{{ end }}:
    {{ range . }}{{ .name }} ({{ .role }}) {{ .address }}
    {{ end }}
```

A config with the stdio transport, so the tool can be called by `scripts/mcp-call.py` (use absolute paths):

```yaml
server:
  transport: stdio
tools_dir: /path/to/tools
workers:
  inventory:
    type: file
    root: /path/to/inventory
log:
  level: warn
```

Validate, then call:

```bash
make build
```

```bash
./bin/ocellusai-mcp -config config.file.yaml -validate
```

```
inventory_hosts                  worker=inventory    type=file       calls=-            process=-            file=/path/to/tools/inventory_hosts.yaml
```

```bash
python3 scripts/mcp-call.py -quiet -config config.file.yaml call inventory_hosts '{"role": "postgres"}'
```

```
== inventory_hosts({"role": "postgres"}): ok in 0.00s
2 host(s) in prod with role postgres:
db-1 (postgres) 10.0.1.11
db-2 (postgres) 10.0.1.12
```

A missing file is a readable error that does not show the root:

```
== inventory_hosts({"env": "staging"}): ERROR in 0.00s
tool inventory_hosts failed at stage worker: hosts/staging.json: no such file under the instance root
```

The other messages of the worker, for the documentation:

| Message | Cause |
|---|---|
| `request.path: "../x" must be relative to the instance root and stay inside it` | an absolute path or `..` |
| `link.json: path escapes from parent` | a symbolic link that points outside the root |
| `big.json: file exceeds 1048576 bytes (max_bytes of the instance)` | the file is larger than `max_bytes` |
| `text.json: not valid JSON: invalid character … (use parse: lines or raw for text files)` | `parse: json` on a text file |
| `hosts: is a directory` | the path names a directory |

## A tool test

Unit tests call the worker directly. A tool test runs a tool file through the catalog and the pipeline, which
checks how the worker's output meets templates and jq. It needs no server and no binary:

```go
package file

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

const inventoryTool = `
name: inventory_hosts
description: Hosts of an environment from the inventory files.
worker: inventory
params:
  env:  {type: string, enum: [prod, staging], default: prod}
  role: {type: string, default: ""}
request:
  path: "hosts/{{ .env }}.json"
response:
  jq: '[.hosts[] | select($params.role == "" or .role == $params.role) | .name]'
`

func TestInventoryTool(t *testing.T) {
	w := newWorker(t, map[string]string{
		"hosts/prod.json": `{"hosts": [{"name": "db-1", "role": "postgres"}, {"name": "web-1", "role": "nginx"}]}`,
	})
	tool, err := catalog.Parse("inventory_hosts.yaml", []byte(inventoryTool), worker.Registry{"inventory": w}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"role": "postgres"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "[\n  \"db-1\"\n]" {
		t.Errorf("text = %q", res.Text)
	}
	_, err = pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"env": "staging"}`))
	if err == nil || !strings.Contains(err.Error(), "worker: hosts/staging.json: no such file under the instance root") {
		t.Errorf("missing file: %v", err)
	}
}
```

`catalog.Parse` takes the file name used in errors, the YAML, a worker registry and a processor registry (`nil` when
the tool has no `process`). The same pattern with a fake worker is how the processors test their example tools.

When the worker becomes part of the project's reference set, also give it a place in the server tests: an instance in
`referenceWorkers` in `internal/server/server_test.go`, a tool in `tools-test/` and a case in
`TestReferenceCatalogCallTools`. A tool in `tools/` that uses the new type needs that instance too, because
`TestWorkingCatalogLoads` loads `tools/` with the reference workers.

## Patterns from the built-in workers

The three built-in workers solve problems a new worker is likely to meet. Read them before writing your own.

**An HTTP client** (`rest`, `prometheus`):

- Build one `http.Client` per instance in `New`, so connections are reused. Clone `http.DefaultTransport` to change
  TLS settings; that keeps the proxy variables (`HTTPS_PROXY`, `NO_PROXY`) working.
- Put the timeout on the context of each request, not on the client, so a request can override it.
- Apply the instance's credentials last, or in an `http.RoundTripper`, so a tool cannot replace them.
- Do not follow redirects when requests carry credentials:
  `CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }`.
- Read through `io.LimitReader(body, max+1)` and fail when more than `max` bytes arrive; JSON cannot be cut safely.
- Put a short, whitespace-collapsed piece of an error response into the message (`rest` keeps 512 bytes).
- Unwrap `*url.Error` before reporting it, or the full URL with its query string ends up in the message.

**A process** (`shell`):

- `exec.CommandContext` without a shell, every argument separate, and an explicit environment.
- A stdout buffer that stops at the limit without failing writes, so the program is not killed by SIGPIPE, and
  `WaitDelay` so a child that keeps its pipes open cannot hang the call.

**A client library** (`prometheus`):

- Convert the library's result into the source's documented JSON (`Normalize`), so tool authors can rely on the
  public API documentation.
- Map the library's error types into short messages (`mapError`), and log warnings instead of failing on them.

**Time parameters** (`prometheus`): accept the forms tool authors already use, `now`, `now-5m`, `now+1h`, RFC 3339
and unix seconds (`parseTime`), and keep the clock in an unexported `now func() time.Time` field that tests replace.

## Documentation

A worker type is only as useful as its documentation. Update:

- [`docs/workers.md`](workers.md): a row in the table at the top, and a section like the others: *Connecting* (the
  instance keys with defaults), *What a tool asks for* (the request keys), *What comes back*, *Errors* (the messages
  and their causes), plus rows in the error reference at the end;
- [`config.example.yaml`](../config.example.yaml): a commented example instance;
- [`README.md`](../README.md): the list of sources under *Features*;
- the lists of worker types in [User guide](user-guide.md) and [Architecture](architecture.md);
- [`skills/ocellus-tool-builder/`](../skills/ocellus-tool-builder): a reference page and a template, if agents should
  write tools for the new source.

## Checklist

- [ ] A package `internal/worker/<type>` with the constant `Type`, a `Config`, `New`, `Type`, `Validate` and `Execute`.
- [ ] `New` checks the settings that need the worker's code and does not contact the source.
- [ ] `Validate` rejects unknown keys, requires the required ones, checks literals, skips templates, checks against
      the instance and does no I/O.
- [ ] Keys that decide the operation or the shape of the result are literals.
- [ ] `Execute` checks rendered values, applies a timeout, passes the context to every blocking call, limits what it
      reads and returns only JSON-compatible values.
- [ ] Errors start with the key or the operation and contain no secrets, full URLs or server paths.
- [ ] The struct holds no per-call state; `make test-race` is clean.
- [ ] `internal/config`: the constant, `WorkerTypes`, the settings struct and field, `decodeWorker`, `applyDefaults`,
      `validate`, and a test.
- [ ] `cmd/ocellusai-mcp`: a `case` in `newWorker` and a test.
- [ ] Unit tests for every request key, every result form, the limits, timeouts and cancellation, and the error texts.
- [ ] An example tool validated with `-validate` and called with `scripts/mcp-call.py`, or a tool test.
- [ ] Documentation updated.
- [ ] `make vet test-race lint validate` passes.

## Common mistakes

- **Typed results.** Returning a `[]string`, a `map[string]int` or a struct. jq copes, but processors receive the
  value as is and fail.
- **Asserting a number's type.** `req["limit"].(int)` fails when the tool writes `limit: "{{ .limit }}"`, which
  arrives as a string.
- **Checking a template in `Validate`.** A valid tool is rejected at startup because `"{{ .path }}"` is not a valid
  path. Check templated values in `Execute`.
- **I/O in `New` or `Validate`.** The server cannot start, and `-validate` cannot run, while the source is down.
- **No timeout, or a timeout without the context.** A source that hangs holds the call until the client gives up.
- **Unbounded reads.** One large answer exhausts the server's memory.
- **Letting the tool choose the reach.** A `url`, `host` or `dsn` key in the request turns the server into a proxy
  to anywhere an agent asks. The reach belongs to the instance.
- **Secrets in messages.** An error from `net/http` contains the full URL with its query string; a connection string
  in an error contains the password.
- **State in the struct.** A field that `Execute` writes is a data race once two sessions call the tool.
