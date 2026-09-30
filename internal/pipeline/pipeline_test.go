package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// wrapProcessor nests its input under "data" next to the rendered "tag" from with.
type wrapProcessor struct {
	err error
}

func (*wrapProcessor) Name() string                  { return "wrap" }
func (*wrapProcessor) Validate(map[string]any) error { return nil }
func (w *wrapProcessor) Run(_ context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	if w.err != nil {
		return nil, w.err
	}
	return map[string]any{"data": data, "tag": with["tag"]}, nil
}

type fakeWorker struct {
	result  any
	err     error
	lastReq map[string]any
}

func (f *fakeWorker) Type() string                  { return "fake" }
func (f *fakeWorker) Validate(map[string]any) error { return nil }
func (f *fakeWorker) Execute(_ context.Context, req map[string]any) (any, error) {
	f.lastReq = req
	return f.result, f.err
}

func vector() any {
	var v any
	_ = json.Unmarshal([]byte(`{"resultType":"vector","result":[
	  {"metric":{"job":"node","instance":"10.0.0.7:9100"},"value":[1,"0"]},
	  {"metric":{"job":"node","instance":"10.0.0.9:9100"},"value":[1,"0"]}]}`), &v)
	return v
}

func tool(t *testing.T, fw *fakeWorker, yaml string) *catalog.Tool {
	t.Helper()
	return toolP(t, fw, nil, yaml)
}

func toolP(t *testing.T, fw *fakeWorker, procs processor.Registry, yaml string) *catalog.Tool {
	t.Helper()
	return toolR(t, worker.Registry{"fake": fw}, procs, yaml)
}

func toolR(t *testing.T, reg worker.Registry, procs processor.Registry, yaml string) *catalog.Tool {
	t.Helper()
	tl, err := catalog.Parse("t.yaml", []byte(yaml), reg, procs)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// podList mimics `kubectl get pods -o json` with the given pod names.
func podList(names ...string) any {
	items := make([]any, 0, len(names))
	for _, n := range names {
		items = append(items, map[string]any{"metadata": map[string]any{"name": n}})
	}
	return map[string]any{"kind": "List", "items": items}
}

const head = "name: t\ndescription: d\nworker: fake\nparams:\n  ns: {type: string, default: prod}\n  n: {type: integer, default: 2}\nrequest:\n  query: \"up{ns={{ promLabel .ns }}}\"\n  n: \"{{ .n }}\"\n"

func TestLevel0PassThrough(t *testing.T) {
	fw := &fakeWorker{result: vector()}
	res, err := New(nil).Run(context.Background(), tool(t, fw, head), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Structured, vector()) {
		t.Errorf("structured = %#v", res.Structured)
	}
	if !strings.HasPrefix(res.Text, "{\n  \"result\": [") {
		t.Errorf("text = %q", res.Text)
	}
	if fw.lastReq["query"] != `up{ns="prod"}` || fw.lastReq["n"] != "2" {
		t.Errorf("rendered request = %#v", fw.lastReq)
	}
}

func TestLevel1JQ(t *testing.T) {
	fw := &fakeWorker{result: vector()}
	tl := tool(t, fw, head+"response:\n  jq: '[.result[] | .metric.instance]'\n")
	res, err := New(nil).Run(context.Background(), tl, json.RawMessage(`{"ns": "dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Structured, []any{"10.0.0.7:9100", "10.0.0.9:9100"}) {
		t.Errorf("structured = %#v", res.Structured)
	}
	if res.Text != "[\n  \"10.0.0.7:9100\",\n  \"10.0.0.9:9100\"\n]" {
		t.Errorf("text = %q", res.Text)
	}
	if fw.lastReq["query"] != `up{ns="dev"}` {
		t.Errorf("param not rendered: %#v", fw.lastReq)
	}
}

func TestLevel2Render(t *testing.T) {
	fw := &fakeWorker{result: vector()}
	tl := tool(t, fw, head+"response:\n  jq: '[.result[] | {job: .metric.job, instance: .metric.instance}]'\n  render: |\n    {{ len . }} down in {{ param \"ns\" }}:\n    {{ range . }}- {{ .job }} @ {{ .instance }}\n    {{ end }}\n")
	res, err := New(nil).Run(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "2 down in prod:\n- node @ 10.0.0.7:9100\n- node @ 10.0.0.9:9100"
	if strings.TrimSpace(res.Text) != want {
		t.Errorf("text = %q", res.Text)
	}
	if l, ok := res.Structured.([]any); !ok || len(l) != 2 {
		t.Errorf("structured should still hold the jq result: %#v", res.Structured)
	}
}

func TestRenderWithoutJQ(t *testing.T) {
	fw := &fakeWorker{result: vector()}
	tl := tool(t, fw, head+"response:\n  render: '{{ .resultType }}/{{ len .result }}'\n")
	res, err := New(nil).Run(context.Background(), tl, nil)
	if err != nil || res.Text != "vector/2" {
		t.Errorf("text = %q, err = %v", res.Text, err)
	}
}

func TestJQParamsVariable(t *testing.T) {
	fw := &fakeWorker{result: []any{1, 2, 3, 4}}
	tl := tool(t, fw, head+"response:\n  jq: 'map(select(. > $params.n))'\n")
	res, err := New(nil).Run(context.Background(), tl, json.RawMessage(`{"n": 3}`))
	if err != nil || !reflect.DeepEqual(res.Structured, []any{4}) {
		t.Errorf("structured = %#v, err = %v", res.Structured, err)
	}
}

func TestProcessChain(t *testing.T) {
	fw := &fakeWorker{result: []any{1, 2}}
	procs := processor.Registry{"wrap": &wrapProcessor{}}
	tl := toolP(t, fw, procs, head+"process:\n  - fn: wrap\n    with: {tag: \"{{ .ns }}-1\"}\n  - fn: wrap\n    with: {tag: second}\nresponse:\n  jq: '{outer: .tag, inner: .data.tag, n: (.data.data | length)}'\n")
	res, err := New(nil).Run(context.Background(), tl, json.RawMessage(`{"ns": "dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"outer": "second", "inner": "dev-1", "n": 2}
	if !reflect.DeepEqual(res.Structured, want) {
		t.Errorf("structured = %#v", res.Structured)
	}
	// Without jq the agent sees the last step's output.
	tl = toolP(t, fw, procs, head+"process:\n  - fn: wrap\n    with: {tag: only}\n")
	res, err = New(nil).Run(context.Background(), tl, nil)
	if err != nil || !reflect.DeepEqual(res.Structured, map[string]any{"data": []any{1, 2}, "tag": "only"}) {
		t.Errorf("structured = %#v, err = %v", res.Structured, err)
	}
	if !strings.Contains(res.Text, `"tag": "only"`) {
		t.Errorf("text = %q", res.Text)
	}
}

func TestProcessStageErrors(t *testing.T) {
	fw := &fakeWorker{result: []any{1}}
	// A failing processor names its step.
	procs := processor.Registry{"wrap": &wrapProcessor{}, "bad": &wrapProcessor{err: errors.New("boom")}}
	tl := toolP(t, fw, procs, head+"process:\n  - fn: wrap\n  - fn: bad\n")
	_, err := New(nil).Run(context.Background(), tl, nil)
	var perr *Error
	if !errors.As(err, &perr) || perr.Stage != StageProcess || !strings.Contains(err.Error(), "process[1] (bad): boom") {
		t.Errorf("err = %v", err)
	}
	// A with template that fails to render is a process-stage error too.
	tl = toolP(t, fw, procs, head+"process:\n  - fn: wrap\n    with: {tag: '{{ param \"nope\" }}'}\n")
	_, err = New(nil).Run(context.Background(), tl, nil)
	if !errors.As(err, &perr) || perr.Stage != StageProcess || !strings.Contains(err.Error(), "process[0] (wrap)") {
		t.Errorf("err = %v", err)
	}
}

func TestStageErrors(t *testing.T) {
	cases := []struct {
		name, yaml, args, stage string
		fw                      *fakeWorker
	}{
		{"arguments", head, `{"ns": 1}`, StageArguments, &fakeWorker{}},
		{"worker", head, ``, StageWorker, &fakeWorker{err: errors.New("boom")}},
		{"jq", head + "response:\n  jq: '.result[0].value | tonumber'\n", ``, StageJQ, &fakeWorker{result: vector()}},
		{"render", head + "response:\n  render: '{{ .missing }}'\n", ``, StageRender, &fakeWorker{result: map[string]any{}}},
		{"request", "name: t\ndescription: d\nworker: fake\nrequest:\n  q: '{{ param \"nope\" }}'\n", ``, StageRequest, &fakeWorker{}},
	}
	for _, c := range cases {
		tl := tool(t, c.fw, c.yaml)
		_, err := New(nil).Run(context.Background(), tl, json.RawMessage(c.args))
		var perr *Error
		if !errors.As(err, &perr) || perr.Stage != c.stage {
			t.Errorf("%s: err = %v, want stage %s", c.name, err, c.stage)
		}
	}
}

// callsHead is a two-call tool: kubectl-like pods → per-call jq → Prometheus
// query built from the pod names via result "pods".
const callsHead = `name: t
description: d
params:
  ns: {type: string, default: prod}
calls:
  - name: pods
    worker: kube
    request: {command: kubectl, ns: "{{ .ns }}"}
    jq: '[.items[].metadata.name]'
  - name: cpu
    worker: prom
    request: {query: 'up{ns={{ promLabel .ns }}, pod=~{{ result "pods" | join "|" | quote }}}'}
`

func TestCallsChain(t *testing.T) {
	kube := &fakeWorker{result: podList("a", "b")}
	prom := &fakeWorker{result: vector()}
	reg := worker.Registry{"kube": kube, "prom": prom}

	// Level 0: without response the agent sees {name: result} for every call.
	tl := toolR(t, reg, nil, callsHead)
	res, err := New(nil).Run(context.Background(), tl, json.RawMessage(`{"ns": "dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	if kube.lastReq["ns"] != "dev" || kube.lastReq["command"] != "kubectl" {
		t.Errorf("first request = %#v", kube.lastReq)
	}
	if prom.lastReq["query"] != `up{ns="dev", pod=~"a|b"}` {
		t.Errorf("second request did not see the first result: %#v", prom.lastReq)
	}
	want := map[string]any{"pods": []any{"a", "b"}, "cpu": vector()}
	if !reflect.DeepEqual(res.Structured, want) {
		t.Errorf("structured = %#v", res.Structured)
	}
	if !strings.Contains(res.Text, `"pods": [`) || !strings.Contains(res.Text, `"cpu": {`) {
		t.Errorf("text = %q", res.Text)
	}

	// Levels 1/2: jq joins the calls, render reads results too.
	tl = toolR(t, reg, nil, callsHead+"response:\n  jq: '{pods: .pods, instances: [.cpu.result[].metric.instance], n: $params.ns}'\n  render: '{{ .n }}: {{ join \",\" .pods }} / {{ len (result \"cpu\").result }} series'\n")
	res, err = New(nil).Run(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"pods": []any{"a", "b"}, "instances": []any{"10.0.0.7:9100", "10.0.0.9:9100"}, "n": "prod"}
	if !reflect.DeepEqual(res.Structured, want) {
		t.Errorf("structured = %#v", res.Structured)
	}
	if res.Text != "prod: a,b / 2 series" {
		t.Errorf("text = %q", res.Text)
	}

	// process steps receive the merged object and may read results in with.
	procs := processor.Registry{"wrap": &wrapProcessor{}}
	tl = toolR(t, reg, procs, callsHead+"process:\n  - fn: wrap\n    with: {tag: '{{ index (result \"pods\") 0 }}'}\nresponse:\n  jq: '{tag: .tag, keys: (.data | keys)}'\n")
	res, err = New(nil).Run(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Structured, map[string]any{"tag": "a", "keys": []any{"cpu", "pods"}}) {
		t.Errorf("structured = %#v", res.Structured)
	}
}

func TestCallsRunSequentially(t *testing.T) {
	// The second call must not start before the first finished: its request
	// depends on the first result, and a failing first call stops the chain.
	kube := &fakeWorker{err: errors.New("boom")}
	prom := &fakeWorker{result: vector()}
	tl := toolR(t, worker.Registry{"kube": kube, "prom": prom}, nil, callsHead)
	_, err := New(nil).Run(context.Background(), tl, nil)
	var perr *Error
	if !errors.As(err, &perr) || perr.Stage != StageWorker || !strings.Contains(err.Error(), "calls[0] (pods): boom") {
		t.Errorf("err = %v", err)
	}
	if prom.lastReq != nil {
		t.Errorf("second call ran after the first failed: %#v", prom.lastReq)
	}
}

func TestCallsStageErrors(t *testing.T) {
	kube := &fakeWorker{result: podList("a")}
	cases := []struct {
		name, yaml, args, stage, want string
		prom                          *fakeWorker
	}{
		{"worker names the call", callsHead, ``, StageWorker, "calls[1] (cpu): boom", &fakeWorker{err: errors.New("boom")}},
		{"call jq", strings.Replace(callsHead, "jq: '[.items[].metadata.name]'", "jq: '.items | tonumber'", 1), ``, StageJQ, "calls[0] (pods): jq:", &fakeWorker{}},
		{"request render", strings.Replace(callsHead, `pod=~{{ result "pods" | join "|" | quote }}`, `{{ param "nope" }}`, 1), ``, StageRequest, `calls[1] (cpu): `, &fakeWorker{}},
		{"dynamic result name", strings.Replace(callsHead, `result "pods"`, `result .ns`, 1), `{"ns": "zzz"}`, StageRequest, `calls[1] (cpu): template calls[1].request.query: `, &fakeWorker{}},
	}
	for _, c := range cases {
		tl := toolR(t, worker.Registry{"kube": kube, "prom": c.prom}, nil, c.yaml)
		_, err := New(nil).Run(context.Background(), tl, json.RawMessage(c.args))
		var perr *Error
		if !errors.As(err, &perr) || perr.Stage != c.stage || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want stage %s mentioning %q", c.name, err, c.stage, c.want)
		}
	}
	// The dynamic name error explains what was available.
	tl := toolR(t, worker.Registry{"kube": kube, "prom": &fakeWorker{}}, nil, strings.Replace(callsHead, `result "pods"`, `result .ns`, 1))
	_, err := New(nil).Run(context.Background(), tl, json.RawMessage(`{"ns": "zzz"}`))
	if err == nil || !strings.Contains(err.Error(), `result "zzz": no such call result (available: pods)`) {
		t.Errorf("err = %v", err)
	}
	// Single-call tools keep their plain errors (no calls[i] prefix).
	_, err = New(nil).Run(context.Background(), tool(t, &fakeWorker{err: errors.New("boom")}, head), nil)
	if err == nil || err.Error() != "worker: boom" {
		t.Errorf("single-call error = %v", err)
	}
}
