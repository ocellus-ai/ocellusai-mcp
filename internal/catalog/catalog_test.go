package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/template"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

type fakeProcessor struct {
	name     string
	validate func(with map[string]any) error
	lastWith map[string]any
}

func (f *fakeProcessor) Name() string { return f.name }
func (f *fakeProcessor) Validate(with map[string]any) error {
	f.lastWith = with
	if f.validate != nil {
		return f.validate(with)
	}
	return nil
}
func (f *fakeProcessor) Run(_ context.Context, _ map[string]any, data any, _ map[string]any) (any, error) {
	return data, nil
}

func processors() processor.Registry {
	return processor.Registry{"fake": &fakeProcessor{name: "fake"}}
}

type fakeWorker struct {
	name        string
	validateErr error
}

func (f *fakeWorker) Type() string                  { return f.name }
func (f *fakeWorker) Validate(map[string]any) error { return f.validateErr }
func (f *fakeWorker) Execute(context.Context, map[string]any) (any, error) {
	return nil, nil
}

func registry() worker.Registry {
	return worker.Registry{"prometheus": &fakeWorker{name: "prometheus"}, "shell": &fakeWorker{name: "shell"}}
}

const fullSpec = `
name: pod_cpu_usage
description: CPU per pod
worker: prometheus
params:
  namespace: {type: string, required: true, description: "K8s namespace"}
  window:    {type: string, default: "5m", pattern: "^[0-9]+[smh]$"}
  top:       {type: integer, default: 10, minimum: 1, maximum: 100}
  mode:      {type: string, enum: [avg, max]}
  verbose:   {type: boolean, default: false}
request:
  type: instant
  query: "topk({{ .top }}, x{ns={{ promLabel .namespace }}}[{{ .window }}])"
response:
  jq: "[.result[] | .metric.pod]"
  render: "{{ range . }}{{ . }}\n{{ end }}"
`

func TestParseFullSpecBuildsSchema(t *testing.T) {
	tool, err := Parse("pod.yaml", []byte(fullSpec), registry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool.JQ == nil || tool.Render == nil || len(tool.Calls) != 1 || tool.Calls[0].Request == nil {
		t.Fatal("expected compiled jq, render and request")
	}
	// The single-call form is one unnamed call bound to the worker instance.
	if c := tool.Calls[0]; c.Name != "" || c.WorkerName != "prometheus" || c.Worker.Type() != "prometheus" || c.JQ != nil || tool.Named() {
		t.Errorf("call = %#v, named = %v", c, tool.Named())
	}
	if !reflect.DeepEqual(tool.WorkerNames(), []string{"prometheus"}) || !reflect.DeepEqual(tool.WorkerTypes(), []string{"prometheus"}) {
		t.Errorf("workers = %v / %v", tool.WorkerNames(), tool.WorkerTypes())
	}
	b, err := json.Marshal(tool.Schema)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	var want map[string]any
	_ = json.Unmarshal([]byte(`{
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["namespace"],
	  "properties": {
	    "namespace": {"type": "string", "description": "K8s namespace"},
	    "window": {"type": "string", "default": "5m", "pattern": "^[0-9]+[smh]$"},
	    "top": {"type": "integer", "default": 10, "minimum": 1, "maximum": 100},
	    "mode": {"type": "string", "enum": ["avg", "max"]},
	    "verbose": {"type": "boolean", "default": false}
	  }}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schema =\n%s", b)
	}
	// Property order follows the YAML.
	if !strings.Contains(string(b), `"namespace"`) || strings.Index(string(b), `"namespace"`) > strings.Index(string(b), `"window"`) {
		t.Errorf("property order not preserved: %s", b)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		yaml, want string
		workers    worker.Registry
	}{
		"missing name":        {yaml: "description: d\nworker: prometheus\nrequest: {query: up}\n", want: "name: required"},
		"bad name":            {yaml: "name: Pod-CPU\ndescription: d\nworker: prometheus\nrequest: {query: up}\n", want: "name:"},
		"missing description": {yaml: "name: a\nworker: prometheus\nrequest: {query: up}\n", want: "description: required"},
		"unknown worker":      {yaml: "name: a\ndescription: d\nworker: loki\nrequest: {query: up}\n", want: `unknown worker "loki"`},
		"missing request":     {yaml: "name: a\ndescription: d\nworker: prometheus\n", want: "request: required"},
		"unknown top field":   {yaml: "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\nparms: {}\n", want: "parms"},
		"bad param type":      {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: text}\nrequest: {query: up}\n", want: "params.x.type"},
		"unknown param field": {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: string, requird: true}\nrequest: {query: up}\n", want: "requird"},
		"required+default":    {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: string, required: true, default: y}\nrequest: {query: up}\n", want: "mutually exclusive"},
		"pattern on int":      {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: integer, pattern: '^a$'}\nrequest: {query: up}\n", want: "pattern"},
		"min on string":       {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: string, minimum: 1}\nrequest: {query: up}\n", want: "minimum"},
		"bad param name":      {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  1x: {type: string}\nrequest: {query: up}\n", want: "params.1x"},
		"default wrong type":  {yaml: "name: a\ndescription: d\nworker: prometheus\nparams:\n  x: {type: integer, default: abc}\nrequest: {query: up}\n", want: "params"},
		"template syntax":     {yaml: "name: a\ndescription: d\nworker: prometheus\nrequest: {query: '{{ .x '}\n", want: "request.query"},
		"bad jq":              {yaml: "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\nresponse: {jq: '.['}\n", want: "response.jq"},
		"bad render":          {yaml: "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\nresponse: {render: '{{ nope }}'}\n", want: "response.render"},
		"worker validate": {
			yaml:    "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\n",
			want:    "request.type: bad",
			workers: worker.Registry{"prometheus": &fakeWorker{name: "prometheus", validateErr: errors.New("request.type: bad")}},
		},
	}
	for name, c := range cases {
		reg := c.workers
		if reg == nil {
			reg = registry()
		}
		_, err := Parse("f.yaml", []byte(c.yaml), reg, nil)
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "f.yaml: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
}

const processSpec = `
name: t
description: d
worker: prometheus
params:
  s: {type: number, default: 2}
request: {query: up}
process:
  - fn: fake
    with: {method: iqr, k: "{{ .s }}"}
  - fn: fake
`

func TestParseProcessSteps(t *testing.T) {
	fp := &fakeProcessor{name: "fake"}
	tool, err := Parse("p.yaml", []byte(processSpec), registry(), processor.Registry{"fake": fp})
	if err != nil {
		t.Fatal(err)
	}
	if len(tool.Steps) != 2 || tool.Steps[0].Fn != "fake" || tool.Steps[0].Processor != fp || tool.Steps[1].With == nil {
		t.Fatalf("steps = %#v", tool.Steps)
	}
	if len(tool.Spec.Process) != 2 || tool.Spec.Process[0].With["method"] != "iqr" {
		t.Errorf("spec.process = %#v", tool.Spec.Process)
	}
	// A step without `with` still hands the processor an (empty) map.
	if fp.lastWith == nil || len(fp.lastWith) != 0 {
		t.Errorf("last validated with = %#v, want empty map", fp.lastWith)
	}
	// The with templates render with the tool parameters.
	rendered, err := tool.Steps[0].With.Render(map[string]any{"s": 3}, nil)
	if err != nil || !reflect.DeepEqual(rendered, map[string]any{"method": "iqr", "k": "3"}) {
		t.Errorf("rendered = %#v, err = %v", rendered, err)
	}
	// Tools without process keep Steps empty.
	plain, err := Parse("s.yaml", []byte(fullSpec), registry(), processors())
	if err != nil || len(plain.Steps) != 0 {
		t.Errorf("plain steps = %#v, err = %v", plain.Steps, err)
	}
}

func TestParseProcessErrors(t *testing.T) {
	const head = "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\n"
	cases := map[string]struct {
		yaml, want string
		processors processor.Registry
	}{
		"not a list":                {yaml: head + "process: {fn: fake}\n", want: "process must be a list of steps"},
		"missing fn":                {yaml: head + "process:\n  - with: {a: 1}\n", want: "process[0].fn: required (available: fake)"},
		"unknown fn":                {yaml: head + "process:\n  - fn: nope\n", want: `process[0].fn: unknown processor "nope" (available: fake)`},
		"unknown step key":          {yaml: head + "process:\n  - fn: fake\n  - fn: fake\n    params: {a: 1}\n", want: "process[1]: "},
		"unknown step key names it": {yaml: head + "process:\n  - fn: fake\n    params: {a: 1}\n", want: "params"},
		"template syntax":           {yaml: head + "process:\n  - fn: fake\n    with: {k: '{{ .x '}\n", want: "process[0].with.k"},
		"processor validate": {
			yaml: head + "process:\n  - fn: fake\n  - fn: fake\n    with: {method: x}\n",
			want: "process[1] (fake): with.method: bad",
			processors: processor.Registry{"fake": &fakeProcessor{name: "fake", validate: func(with map[string]any) error {
				if with["method"] == "x" {
					return errors.New("with.method: bad")
				}
				return nil
			}}},
		},
		"no processors registered": {
			yaml:       head + "process:\n  - fn: fake\n",
			want:       `unknown processor "fake" (available: )`,
			processors: processor.Registry{},
		},
	}
	for name, c := range cases {
		reg := c.processors
		if reg == nil {
			reg = processors()
		}
		_, err := Parse("f.yaml", []byte(c.yaml), registry(), reg)
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "f.yaml: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
	// The first failing step is reported even when a later one would pass.
	fp := &fakeProcessor{name: "fake", validate: func(map[string]any) error { return errors.New("boom") }}
	_, err := Parse("f.yaml", []byte(head+"process:\n  - fn: fake\n  - fn: fake\n"), registry(), processor.Registry{"fake": fp})
	if err == nil || !strings.Contains(err.Error(), "process[0] (fake): boom") {
		t.Errorf("err = %v", err)
	}
}

const callsSpec = `
name: pods_cpu
description: d
params:
  ns: {type: string, required: true}
calls:
  - name: pods
    worker: shell
    request: {command: kubectl, args: [get, pods, -n, "{{ .ns }}"]}
    jq: '[.items[].metadata.name]'
  - name: cpu
    worker: prometheus
    request: {query: 'up{pod=~"{{ result "pods" | join "|" }}"}'}
process:
  - fn: fake
    with: {tag: '{{ result "pods" }}'}
response:
  jq: '{pods: .pods, cpu: .cpu}'
  render: '{{ len (result "pods") }} {{ result "cpu" }}'
`

func TestParseCalls(t *testing.T) {
	reg := registry()
	tool, err := Parse("c.yaml", []byte(callsSpec), reg, processors())
	if err != nil {
		t.Fatal(err)
	}
	if len(tool.Calls) != 2 || !tool.Named() {
		t.Fatalf("calls = %#v", tool.Calls)
	}
	pods, cpu := tool.Calls[0], tool.Calls[1]
	if pods.Name != "pods" || pods.WorkerName != "shell" || pods.Worker != reg["shell"] || pods.JQ == nil || pods.Request == nil {
		t.Errorf("calls[0] = %#v", pods)
	}
	if cpu.Name != "cpu" || cpu.WorkerName != "prometheus" || cpu.Worker != reg["prometheus"] || cpu.JQ != nil {
		t.Errorf("calls[1] = %#v", cpu)
	}
	if !reflect.DeepEqual(tool.WorkerNames(), []string{"shell", "prometheus"}) || !reflect.DeepEqual(tool.WorkerTypes(), []string{"shell", "prometheus"}) {
		t.Errorf("workers = %v / %v", tool.WorkerNames(), tool.WorkerTypes())
	}
	if tool.Spec.Worker != "" || len(tool.Spec.Request) != 0 || len(tool.Spec.Calls) != 2 || tool.Spec.Calls[1].Request["query"] == nil {
		t.Errorf("spec = %#v", tool.Spec)
	}
	// A later call's request renders with the earlier results.
	rendered, err := cpu.Request.Render(map[string]any{"ns": "prod"}, template.Results{"pods": []any{"a", "b"}})
	if err != nil || rendered.(map[string]any)["query"] != `up{pod=~"a|b"}` {
		t.Errorf("rendered = %#v, err = %v", rendered, err)
	}
	// The same worker twice is listed once.
	twice := "name: a\ndescription: d\ncalls:\n  - {name: x, worker: prometheus, request: {query: up}}\n  - {name: y, worker: prometheus, request: {query: down}}\n"
	tool, err = Parse("c.yaml", []byte(twice), reg, nil)
	if err != nil || !reflect.DeepEqual(tool.WorkerNames(), []string{"prometheus"}) {
		t.Errorf("workers = %v, err = %v", tool.WorkerNames(), err)
	}
}

func TestParseCallsErrors(t *testing.T) {
	const head = "name: a\ndescription: d\n"
	const two = head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}}\n  - {name: b, worker: prometheus, request: {query: up}}\n"
	cases := map[string]struct {
		yaml, want string
		workers    worker.Registry
	}{
		"not a list":         {yaml: head + "calls: {name: x}\n", want: "calls must be a list of calls"},
		"missing name":       {yaml: head + "calls:\n  - worker: prometheus\n    request: {query: up}\n", want: "calls[0].name: required"},
		"bad name":           {yaml: head + "calls:\n  - {name: 1x, worker: prometheus, request: {query: up}}\n", want: "calls[0].name:"},
		"duplicate name":     {yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}}\n  - {name: a, worker: shell, request: {command: df}}\n", want: `calls[1].name: duplicate call name "a" (also calls[0])`},
		"missing worker":     {yaml: head + "calls:\n  - {name: a, request: {query: up}}\n", want: "calls[0].worker: required"},
		"unknown worker":     {yaml: head + "calls:\n  - {name: a, worker: loki, request: {query: up}}\n", want: `calls[0].worker: unknown worker "loki"`},
		"missing request":    {yaml: head + "calls:\n  - {name: a, worker: prometheus}\n", want: "calls[0].request: required"},
		"unknown call key":   {yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}, with: {}}\n", want: "calls[0]: "},
		"unknown key named":  {yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}, with: {}}\n", want: "with"},
		"template syntax":    {yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: '{{ .x '}}\n", want: "calls[0].request.query"},
		"bad jq":             {yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}, jq: '.['}\n", want: "calls[0].jq"},
		"worker with calls":  {yaml: head + "worker: prometheus\ncalls:\n  - {name: a, worker: prometheus, request: {query: up}}\n", want: "worker: not allowed together with calls"},
		"request with calls": {yaml: head + "request: {query: up}\ncalls:\n  - {name: a, worker: prometheus, request: {query: up}}\n", want: "request: not allowed together with calls"},
		"empty list":         {yaml: head + "calls: []\n", want: "worker: required (or a calls: list)"},
		"worker validate": {
			yaml:    head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}}\n  - {name: b, worker: shell, request: {command: df}}\n",
			want:    "calls[1] (b): request.command: bad",
			workers: worker.Registry{"prometheus": &fakeWorker{name: "prometheus"}, "shell": &fakeWorker{name: "shell", validateErr: errors.New("request.command: bad")}},
		},
		"result forward ref": {
			yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: '{{ result \"b\" }}'}}\n  - {name: b, worker: prometheus, request: {query: up}}\n",
			want: `calls[0].request: result "b": no call result is available here`,
		},
		"result self ref": {
			yaml: head + "calls:\n  - {name: a, worker: prometheus, request: {query: up}}\n  - {name: b, worker: prometheus, request: {query: '{{ result \"b\" }}'}}\n",
			want: `calls[1].request: result "b": no such call executed before this point (available: a)`,
		},
		"result unknown in render": {yaml: two + "response: {render: '{{ result \"nope\" }}'}\n", want: `response.render: result "nope": no such call executed before this point (available: a, b)`},
		"result unknown in with":   {yaml: two + "process:\n  - fn: fake\n    with: {k: '{{ result \"nope\" }}'}\n", want: `process[0].with: result "nope"`},
		"result in single-call":    {yaml: head + "worker: prometheus\nrequest: {query: '{{ result \"x\" }}'}\n", want: `request: result "x": no call result is available here`},
	}
	for name, c := range cases {
		reg := c.workers
		if reg == nil {
			reg = registry()
		}
		_, err := Parse("f.yaml", []byte(c.yaml), reg, processors())
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "f.yaml: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
	// Valid references pass: earlier calls in a request, any call in with and render.
	if _, err := Parse("f.yaml", []byte(callsSpec), registry(), processors()); err != nil {
		t.Errorf("valid result references rejected: %v", err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.yaml", "name: b\ndescription: d\nworker: prometheus\nrequest: {query: up}\n")
	write("a.yml", "name: a\ndescription: d\nworker: prometheus\nrequest: {query: up}\n")
	write("README.md", "ignored")
	tools, err := Load(dir, registry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Names(tools); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("names = %v", got)
	}
	write("c.yaml", "name: a\ndescription: dup\nworker: prometheus\nrequest: {query: up}\n")
	_, err = Load(dir, registry(), nil)
	if err == nil || !strings.Contains(err.Error(), "already defined") || !strings.Contains(err.Error(), "a.yml") {
		t.Errorf("duplicate error = %v", err)
	}
	if _, err := Load(t.TempDir(), registry(), nil); err == nil {
		t.Error("empty dir should be an error")
	}
	if _, err := Load(filepath.Join(dir, "missing"), registry(), nil); err == nil {
		t.Error("missing dir should be an error")
	}
}

func TestBindArgs(t *testing.T) {
	tool, err := Parse("pod.yaml", []byte(fullSpec), registry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := tool.BindArgs(json.RawMessage(`{"namespace": "prod", "top": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"namespace": "prod", "window": "5m", "top": 3, "mode": nil, "verbose": false}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v", args)
	}
	if args["top"] != 3 {
		t.Errorf("integer should be int, got %T", args["top"])
	}
	for name, raw := range map[string]string{
		"missing required": `{}`,
		"unknown arg":      `{"namespace": "p", "nmspace": "x"}`,
		"wrong type":       `{"namespace": 5}`,
		"below minimum":    `{"namespace": "p", "top": 0}`,
		"bad enum":         `{"namespace": "p", "mode": "sum"}`,
		"bad pattern":      `{"namespace": "p", "window": "5 minutes"}`,
		"not object":       `[1]`,
		"float for int":    `{"namespace": "p", "top": 2.5}`,
	} {
		if _, err := tool.BindArgs(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Defaults are applied for null / empty arguments.
	simple, _ := Parse("s.yaml", []byte("name: s\ndescription: d\nworker: prometheus\nparams:\n  n: {type: integer, default: 7}\nrequest: {query: up}\n"), registry(), nil)
	for _, raw := range []string{"", "null", "{}"} {
		args, err := simple.BindArgs(json.RawMessage(raw))
		if err != nil || args["n"] != 7 {
			t.Errorf("raw %q: args = %#v, err = %v", raw, args, err)
		}
	}
}
