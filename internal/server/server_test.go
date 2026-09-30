package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
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
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/prometheus"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/rest"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker/shell"
)

// promMock answers the queries used by the reference tools in ../../tools-test.
type promMock struct {
	mu      sync.Mutex
	queries []string
}

func (m *promMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		m.mu.Lock()
		m.queries = append(m.queries, q)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q == "up":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			 {"metric":{"instance":"10.0.0.1:9100","job":"node"},"value":[1757600000,"1"]},
			 {"metric":{"instance":"10.0.0.7:9100","job":"node"},"value":[1757600000,"0"]},
			 {"metric":{"instance":"10.0.0.9:9100","job":"node"},"value":[1757600000,"0"]}]}}`))
		case q == "up == 0":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			 {"metric":{"instance":"10.0.0.7:9100","job":"node"},"value":[1757600000,"0"]},
			 {"metric":{"instance":"10.0.0.9:9100","job":"node"},"value":[1757600000,"0"]}]}}`))
		case strings.HasPrefix(q, "topk("), strings.Contains(q, `pod=~"api-1|api-2"`):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			 {"metric":{"pod":"api-1"},"value":[1757600000,"0.25"]},
			 {"metric":{"pod":"api-2"},"value":[1757600000,"1"]}]}}`))
		case strings.Contains(q, "sum by (pod)") && strings.Contains(q, `namespace="prod"`):
			// pod_cpu_outliers: seven pods around 0.2 cores and one hot pod.
			_, _ = w.Write([]byte(outlierVector()))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	})
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		m.mu.Lock()
		m.queries = append(m.queries, q)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "by (pod)") {
			_, _ = w.Write([]byte(anomalyMatrix()))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
		 {"metric":{},"values":[[1757600000,"0.5"],[1757600060,"0.75"]]}]}}`))
	})
	return mux
}

// outlierVector is the instant result for pod_cpu_outliers: batch-9 is far
// above the seven api pods.
func outlierVector() string {
	vals := map[string]string{"api-1": "0.18", "api-2": "0.2", "api-3": "0.22", "api-4": "0.19", "api-5": "0.21", "api-6": "0.2", "api-7": "0.23", "batch-9": "1.5"}
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, fmt.Sprintf(`{"metric":{"pod":"%s"},"value":[1757600000,"%s"]}`, n, vals[n]))
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(items, ",") + `]}}`
}

// anomalyMatrix is the range result for pod_cpu_anomalies: api-1 is flat at
// 0.2 cores with one spike to 2 cores; api-2 has too few samples to analyse.
func anomalyMatrix() string {
	var api1 []string
	for i := 0; i < 30; i++ {
		v := "0.2"
		if i == 17 {
			v = "2"
		}
		api1 = append(api1, fmt.Sprintf(`[%d,"%s"]`, 1757600000+60*i, v))
	}
	var api2 []string
	for i := 0; i < 5; i++ {
		api2 = append(api2, fmt.Sprintf(`[%d,"0.1"]`, 1757600000+60*i))
	}
	return `{"status":"success","data":{"resultType":"matrix","result":[
	 {"metric":{"pod":"api-1"},"values":[` + strings.Join(api1, ",") + `]},
	 {"metric":{"pod":"api-2"},"values":[` + strings.Join(api2, ",") + `]}]}}`
}

// amMock answers the Alertmanager API v2 endpoints used by the alertmanager_*
// reference tools and records the last request it received.
type amMock struct {
	mu   sync.Mutex
	last amRequest
}

type amRequest struct {
	Method, Path, Query, Body string
}

func (m *amMock) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.last = amRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)}
	m.mu.Unlock()
}

func (m *amMock) lastRequest() amRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

const amSilences = `[
 {"id":"a1","status":{"state":"active"},"updatedAt":"2026-09-17T10:00:00Z","comment":"maintenance","createdBy":"ops",
  "startsAt":"2026-09-17T10:00:00Z","endsAt":"2026-09-17T14:00:00Z",
  "matchers":[{"isEqual":true,"isRegex":false,"name":"alertname","value":"HighCPU"},{"isEqual":true,"isRegex":false,"name":"namespace","value":"prod"}]},
 {"id":"e2","status":{"state":"expired"},"updatedAt":"2026-09-10T10:00:00Z","comment":"old","createdBy":"bot",
  "startsAt":"2026-09-10T10:00:00Z","endsAt":"2026-09-10T12:00:00Z",
  "matchers":[{"isEqual":true,"isRegex":true,"name":"alertname","value":"Disk.*"}]}
]`

func (m *amMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/silences", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(amSilences))
	})
	mux.HandleFunc("POST /api/v2/silences", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		var body struct {
			Matchers []map[string]any `json:"matchers"`
		}
		if err := json.Unmarshal([]byte(m.lastRequest().Body), &body); err != nil || len(body.Matchers) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"matchers required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"silenceID":"n3"}`))
	})
	mux.HandleFunc("DELETE /api/v2/silence/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		if r.PathValue("id") == "a1" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"silence ` + r.PathValue("id") + ` not found"}`))
	})
	return mux
}

// referenceProcessors holds every processor the catalogs in ../../tools-test
// and ../../tools use.
func referenceProcessors() processor.Registry {
	reg := processor.Registry{}
	for _, p := range []processor.Processor{
		anomaly.New(), anomalymv.New(), clusterevents.New(), ensemble.New(), jq.New(),
		join.New(), outliers.New(), ssaproc.NewUni(), ssaproc.NewMulti(),
	} {
		reg[p.Name()] = p
	}
	return reg
}

func (m *promMock) lastQuery() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queries) == 0 {
		return ""
	}
	return m.queries[len(m.queries)-1]
}

// connect builds a server from toolsDir and returns a connected in-memory client session.
func connect(t *testing.T, toolsDir string, workers worker.Registry, processors processor.Registry) *mcp.ClientSession {
	t.Helper()
	tools, err := catalog.Load(toolsDir, workers, processors)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(tools, pipeline.New(nil), Options{Name: "test", Version: "0"})
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// fakeKubectl puts a `kubectl` script first on PATH that prints a fixed pod
// list and records its arguments in <dir>/args. It must run before the shell
// worker is created, which captures PATH for its children.
func fakeKubectl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" > "${0%/*}/args"
printf '%s' '{"kind":"List","items":[
 {"metadata":{"name":"api-1"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","containerStatuses":[{"restartCount":2},{"restartCount":1}]}},
 {"metadata":{"name":"api-2"},"spec":{"nodeName":"n2"},"status":{"phase":"Running","containerStatuses":[{"restartCount":0}]}}]}'
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func referenceWorkers(t *testing.T) (worker.Registry, *promMock, *amMock) {
	t.Helper()
	m := &promMock{}
	ps := httptest.NewServer(m.handler())
	t.Cleanup(ps.Close)
	pw, err := prometheus.New(prometheus.Config{URL: ps.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	sw, err := shell.New(shell.Config{Allowlist: []string{"kubectl", "helm", "df"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	am := &amMock{}
	as := httptest.NewServer(am.handler())
	t.Cleanup(as.Close)
	rw, err := rest.New(rest.Config{URL: as.URL + "/api/v2", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return worker.Registry{prometheus.Type: pw, shell.Type: sw, rest.Type: rw}, m, am
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("expected one content item, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return tc.Text
}

// wrapped returns the value of a structuredContent that wraps a non-object
// result as {"result": v}, failing if it is not wrapped that way.
func wrapped(t *testing.T, res *mcp.CallToolResult) any {
	t.Helper()
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok || len(sc) != 1 {
		t.Fatalf("structuredContent should be {\"result\": ...}: %#v", res.StructuredContent)
	}
	v, ok := sc["result"]
	if !ok {
		t.Fatalf("structuredContent should be {\"result\": ...}: %#v", res.StructuredContent)
	}
	return v
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestReferenceCatalogListTools(t *testing.T) {
	workers, _, _ := referenceWorkers(t)
	cs := connect(t, "../../tools-test", workers, referenceProcessors())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"targets_up", "targets_down", "targets_down_summary", "pod_cpu_usage", "pods_status", "disk_usage", "namespace_cpu_timeseries", "pod_cpu_anomalies", "pods_cpu_status", "alertmanager_silences", "alertmanager_silence_create", "alertmanager_silence_expire", "pod_cpu_outliers", "disk_usage_outliers"} {
		if byName[want] == nil {
			t.Errorf("tool %s missing from ListTools", want)
		}
	}
	schema := byName["pod_cpu_usage"].InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	if schema["type"] != "object" || !reflect.DeepEqual(schema["required"], []any{"namespace"}) {
		t.Errorf("schema = %#v", schema)
	}
	if props["namespace"].(map[string]any)["type"] != "string" || props["top"].(map[string]any)["default"] != float64(10) {
		t.Errorf("properties = %#v", props)
	}
	if byName["targets_up"].Description == "" {
		t.Error("description should be passed to the agent")
	}
	if byName["targets_up"].InputSchema.(map[string]any)["type"] != "object" {
		t.Errorf("tools without params still need an object schema: %#v", byName["targets_up"].InputSchema)
	}
}

// TestWorkingCatalogLoads keeps the working catalog in ../../tools loadable:
// the tests call none of its tools, but every one must compile at startup.
func TestWorkingCatalogLoads(t *testing.T) {
	workers, _, _ := referenceWorkers(t)
	tools, err := catalog.Load("../../tools", workers, referenceProcessors())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("the working catalog is empty")
	}
}

func TestReferenceCatalogCallTools(t *testing.T) {
	kubeDir := fakeKubectl(t)
	workers, m, am := referenceWorkers(t)
	cs := connect(t, "../../tools-test", workers, referenceProcessors())

	t.Run("level0 targets_up", func(t *testing.T) {
		res := call(t, cs, "targets_up", nil)
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(text(t, res)), &parsed); err != nil {
			t.Fatalf("text is not JSON: %v", err)
		}
		if parsed["resultType"] != "vector" || len(parsed["result"].([]any)) != 3 {
			t.Errorf("text = %s", text(t, res))
		}
		if sc, ok := res.StructuredContent.(map[string]any); !ok || sc["resultType"] != "vector" {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
	})

	t.Run("level1 targets_down", func(t *testing.T) {
		res := call(t, cs, "targets_down", map[string]any{})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		want := []any{
			map[string]any{"job": "node", "instance": "10.0.0.7:9100"},
			map[string]any{"job": "node", "instance": "10.0.0.9:9100"},
		}
		if got := wrapped(t, res); !reflect.DeepEqual(got, want) {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
		var parsed []any
		if err := json.Unmarshal([]byte(text(t, res)), &parsed); err != nil || len(parsed) != 2 {
			t.Errorf("text = %s", text(t, res))
		}
	})

	t.Run("level2 targets_down_summary", func(t *testing.T) {
		res := call(t, cs, "targets_down_summary", nil)
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		got := text(t, res)
		want := "2 target(s) down:\n- node @ 10.0.0.7:9100\n- node @ 10.0.0.9:9100\n"
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("text = %q, want %q", got, want)
		}
		if l, ok := wrapped(t, res).([]any); !ok || len(l) != 2 {
			t.Errorf("structuredContent should hold the jq result: %#v", res.StructuredContent)
		}
	})

	t.Run("pod_cpu_usage with params", func(t *testing.T) {
		res := call(t, cs, "pod_cpu_usage", map[string]any{"namespace": "prod", "top": 2})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		q := m.lastQuery()
		if !strings.Contains(q, "topk(2,") || !strings.Contains(q, `namespace="prod"`) || !strings.Contains(q, "[5m]") {
			t.Errorf("rendered query = %q", q)
		}
		got := text(t, res)
		if !strings.Contains(got, "api-2: 1.000 cores") || !strings.Contains(got, "api-1: 0.250 cores") {
			t.Errorf("text = %q", got)
		}
		first := wrapped(t, res).([]any)[0].(map[string]any)
		if first["pod"] != "api-2" {
			t.Errorf("should be sorted by cores desc: %#v", res.StructuredContent)
		}
	})

	t.Run("range query", func(t *testing.T) {
		res := call(t, cs, "namespace_cpu_timeseries", map[string]any{"namespace": "prod", "range": "30m", "step": "15s"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		pts := wrapped(t, res).([]any)
		if len(pts) != 2 || pts[1].(map[string]any)["cores"] != 0.75 {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
	})

	t.Run("process pod_cpu_anomalies", func(t *testing.T) {
		res := call(t, cs, "pod_cpu_anomalies", map[string]any{"namespace": "prod", "range": "1h", "sensitivity": 3})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		q := m.lastQuery()
		if !strings.Contains(q, "by (pod)") || !strings.Contains(q, `namespace="prod"`) {
			t.Errorf("rendered query = %q", q)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok || sc["total"] != float64(1) || sc["pods_analyzed"] != float64(1) || sc["pods_skipped"] != float64(1) {
			t.Fatalf("structuredContent = %#v", res.StructuredContent)
		}
		pods := sc["pods"].([]any)
		if len(pods) != 1 {
			t.Fatalf("pods = %#v", pods)
		}
		pod := pods[0].(map[string]any)
		an := pod["anomalies"].([]any)
		if pod["pod"] != "api-1" || pod["count"] != float64(1) || pod["expected"] != 0.2 || len(an) != 1 {
			t.Errorf("pod = %#v", pod)
		}
		first := an[0].(map[string]any)
		if first["direction"] != "up" || first["value"] != float64(2) || first["time"] != "2025-09-11T14:30:20Z" {
			t.Errorf("anomaly = %#v", first)
		}
		got := text(t, res)
		for _, want := range []string{"1 anomalous CPU sample(s) in namespace prod over the last 1h", "k=3", "api-1: 1 anomaly(ies), expected ~0.200 cores", "2025-09-11T14:30:20Z up to 2.000 cores"} {
			if !strings.Contains(got, want) {
				t.Errorf("text %q lacks %q", got, want)
			}
		}
		// No anomalies: the render takes the other branch.
		res = call(t, cs, "namespace_cpu_timeseries", map[string]any{"namespace": "prod"})
		if res.IsError {
			t.Fatalf("sanity: %s", text(t, res))
		}
	})

	t.Run("calls pods_cpu_status joins kubectl and prometheus", func(t *testing.T) {
		res := call(t, cs, "pods_cpu_status", map[string]any{"namespace": "prod", "selector": "app=api"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		// First call: kubectl saw the rendered parameters.
		args, err := os.ReadFile(filepath.Join(kubeDir, "args"))
		if err != nil || strings.TrimSpace(string(args)) != "get pods -n prod -l app=api -o json" {
			t.Errorf("kubectl args = %q, err = %v", args, err)
		}
		// Second call: the query was built from the first call's result.
		q := m.lastQuery()
		for _, want := range []string{`namespace="prod"`, `pod=~"api-1|api-2"`, "[5m]"} {
			if !strings.Contains(q, want) {
				t.Errorf("rendered query %q lacks %q", q, want)
			}
		}
		// response.jq joined both by pod name, sorted by cores.
		rows := wrapped(t, res).([]any)
		if len(rows) != 2 {
			t.Fatalf("structuredContent = %#v", res.StructuredContent)
		}
		first, second := rows[0].(map[string]any), rows[1].(map[string]any)
		if first["pod"] != "api-2" || first["cores"] != float64(1) || first["restarts"] != float64(0) || first["node"] != "n2" {
			t.Errorf("first row = %#v", first)
		}
		if second["pod"] != "api-1" || second["cores"] != 0.25 || second["restarts"] != float64(3) || second["phase"] != "Running" {
			t.Errorf("second row = %#v", second)
		}
		got := text(t, res)
		for _, want := range []string{"2 pod(s) in prod matching app=api, CPU over the last 5m", "api-2: Running, 0 restart(s), node n2, 1.000 cores", "api-1: Running, 3 restart(s), node n1, 0.250 cores"} {
			if !strings.Contains(got, want) {
				t.Errorf("text %q lacks %q", got, want)
			}
		}
	})

	t.Run("rest alertmanager_silences GET with query", func(t *testing.T) {
		res := call(t, cs, "alertmanager_silences", map[string]any{"filter": `alertname="HighCPU"`})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		req := am.lastRequest()
		if req.Method != "GET" || req.Path != "/api/v2/silences" || req.Query != "filter=alertname%3D%22HighCPU%22" {
			t.Errorf("request = %+v", req)
		}
		rows := wrapped(t, res).([]any)
		if len(rows) != 1 {
			t.Fatalf("active silences = %#v", rows)
		}
		row := rows[0].(map[string]any)
		if row["id"] != "a1" || row["state"] != "active" || row["matchers"] != `alertname="HighCPU", namespace="prod"` || row["created_by"] != "ops" {
			t.Errorf("row = %#v", row)
		}
		got := text(t, res)
		for _, want := range []string{"1 active silence(s):", `- a1 [active] alertname="HighCPU", namespace="prod" until 2026-09-17T14:00:00Z by ops: maintenance`} {
			if !strings.Contains(got, want) {
				t.Errorf("text %q lacks %q", got, want)
			}
		}
		// state=all: both silences, the regex matcher is rendered with =~; an empty filter is left out.
		res = call(t, cs, "alertmanager_silences", map[string]any{"state": "all"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		if req := am.lastRequest(); req.Query != "" {
			t.Errorf("empty filter must be omitted, got query %q", req.Query)
		}
		rows = wrapped(t, res).([]any)
		if len(rows) != 2 || rows[1].(map[string]any)["matchers"] != `alertname=~"Disk.*"` {
			t.Errorf("all silences = %#v", rows)
		}
		res = call(t, cs, "alertmanager_silences", map[string]any{"state": "pending"})
		if res.IsError || strings.TrimSpace(text(t, res)) != "No pending silences." {
			t.Errorf("pending: IsError = %v text = %q", res.IsError, text(t, res))
		}
	})

	t.Run("rest alertmanager_silence_create POST with body", func(t *testing.T) {
		res := call(t, cs, "alertmanager_silence_create", map[string]any{"alertname": "HighCPU", "namespace": "prod", "comment": "maintenance", "duration": "30m"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		req := am.lastRequest()
		if req.Method != "POST" || req.Path != "/api/v2/silences" {
			t.Errorf("request = %+v", req)
		}
		var body struct {
			Matchers  []map[string]any `json:"matchers"`
			StartsAt  string           `json:"startsAt"`
			EndsAt    string           `json:"endsAt"`
			CreatedBy string           `json:"createdBy"`
			Comment   string           `json:"comment"`
		}
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatalf("body %q is not JSON: %v", req.Body, err)
		}
		if len(body.Matchers) != 2 || body.Matchers[0]["name"] != "alertname" || body.Matchers[0]["value"] != "HighCPU" || body.Matchers[0]["isRegex"] != false ||
			body.Matchers[1]["name"] != "namespace" || body.Matchers[1]["value"] != "prod" {
			t.Errorf("matchers = %#v", body.Matchers)
		}
		if body.CreatedBy != "ocellusai-mcp" || body.Comment != "maintenance" {
			t.Errorf("body = %+v", body)
		}
		start, err1 := time.Parse(time.RFC3339, body.StartsAt)
		end, err2 := time.Parse(time.RFC3339, body.EndsAt)
		if err1 != nil || err2 != nil || end.Sub(start) != 30*time.Minute || time.Since(start) > time.Minute {
			t.Errorf("startsAt = %q endsAt = %q (%v %v)", body.StartsAt, body.EndsAt, err1, err2)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok || sc["id"] != "n3" || sc["status"] != float64(200) {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
		if got := strings.TrimSpace(text(t, res)); got != "Silence n3 created (HTTP 200): HighCPU in namespace prod muted for 30m." {
			t.Errorf("text = %q", got)
		}
		// Without a namespace the second matcher is not rendered.
		res = call(t, cs, "alertmanager_silence_create", map[string]any{"alertname": "HighCPU", "comment": "x"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		if b := am.lastRequest().Body; strings.Contains(b, "namespace") || !strings.Contains(b, `"endsAt"`) {
			t.Errorf("body without namespace = %s", b)
		}
	})

	t.Run("rest alertmanager_silence_expire DELETE and API error", func(t *testing.T) {
		res := call(t, cs, "alertmanager_silence_expire", map[string]any{"id": "a1"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		if req := am.lastRequest(); req.Method != "DELETE" || req.Path != "/api/v2/silence/a1" || req.Body != "" {
			t.Errorf("request = %+v", req)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok || sc["id"] != "a1" || sc["status"] != float64(200) {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
		if got := strings.TrimSpace(text(t, res)); got != "Silence a1 expired (HTTP 200)." {
			t.Errorf("text = %q", got)
		}
		// A non-2xx answer is a worker-stage error that names method, path, status and body.
		res = call(t, cs, "alertmanager_silence_expire", map[string]any{"id": "zz"})
		if got := text(t, res); !res.IsError || !strings.Contains(got, `failed at stage worker: DELETE /api/v2/silence/zz: 404 Not Found: {"error":"silence zz not found"}`) {
			t.Errorf("IsError = %v text = %q", res.IsError, got)
		}
		// The id pattern stops path tricks before any request is made.
		res = call(t, cs, "alertmanager_silence_expire", map[string]any{"id": "../silences"})
		if got := text(t, res); !res.IsError || !strings.Contains(got, "failed at stage arguments") {
			t.Errorf("IsError = %v text = %q", res.IsError, got)
		}
	})

	t.Run("invalid arguments is a tool error", func(t *testing.T) {
		res := call(t, cs, "pod_cpu_usage", map[string]any{"top": 5})
		if !res.IsError || !strings.Contains(text(t, res), "namespace") {
			t.Errorf("IsError = %v, text = %q", res.IsError, text(t, res))
		}
		res = call(t, cs, "pod_cpu_usage", map[string]any{"namespace": "p", "window": "five minutes"})
		if !res.IsError || !strings.Contains(text(t, res), "arguments") {
			t.Errorf("IsError = %v, text = %q", res.IsError, text(t, res))
		}
	})

	t.Run("unknown tool is a protocol error", func(t *testing.T) {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "nope"}); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("shell disk_usage runs df", func(t *testing.T) {
		if _, err := os.Stat("/bin/df"); err != nil {
			t.Skip("df not available")
		}
		res := call(t, cs, "disk_usage", map[string]any{"threshold": 0})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		rows, ok := wrapped(t, res).([]any)
		if !ok || len(rows) == 0 {
			t.Errorf("structuredContent = %#v", res.StructuredContent)
		}
	})

	t.Run("process pod_cpu_outliers", func(t *testing.T) {
		res := call(t, cs, "pod_cpu_outliers", map[string]any{"namespace": "prod"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		q := m.lastQuery()
		if !strings.HasPrefix(q, "sum by (pod)") || !strings.Contains(q, `namespace="prod"`) || !strings.Contains(q, "[5m]") {
			t.Errorf("rendered query = %q", q)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok || sc["pods"] != float64(8) || sc["skipped"] != false {
			t.Fatalf("structuredContent = %#v", res.StructuredContent)
		}
		if median, _ := sc["median"].(float64); math.Abs(median-0.205) > 1e-9 {
			t.Errorf("median = %#v", sc["median"])
		}
		out := sc["outliers"].([]any)
		if len(out) != 1 {
			t.Fatalf("outliers = %#v", out)
		}
		hot := out[0].(map[string]any)
		if hot["pod"] != "batch-9" || hot["cores"] != 1.5 || hot["direction"] != "up" || hot["score"].(float64) < 10 {
			t.Errorf("outlier = %#v", hot)
		}
		got := text(t, res)
		for _, want := range []string{"1 outlying pod(s) among 8 in namespace prod over the last 5m", "median 0.205 cores", "k=1.5", "batch-9: 1.500 cores (up, score"} {
			if !strings.Contains(got, want) {
				t.Errorf("text %q lacks %q", got, want)
			}
		}
		// A namespace without pods: the empty vector is one skipped series, not an error.
		res = call(t, cs, "pod_cpu_outliers", map[string]any{"namespace": "empty", "window": "1h"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		if got := text(t, res); !strings.Contains(got, "Not enough pods in namespace empty to compare (fewer than 5 items (min_points))") {
			t.Errorf("text = %q", got)
		}
	})

	t.Run("process disk_usage_outliers runs df", func(t *testing.T) {
		if _, err := os.Stat("/bin/df"); err != nil {
			t.Skip("df not available")
		}
		res := call(t, cs, "disk_usage_outliers", map[string]any{"sensitivity": 3, "direction": "up"})
		if res.IsError {
			t.Fatalf("unexpected error: %s", text(t, res))
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent = %#v", res.StructuredContent)
		}
		// The host decides how many filesystems there are; the shape is what matters.
		if _, ok := sc["filesystems"].(float64); !ok {
			t.Errorf("filesystems = %#v", sc["filesystems"])
		}
		if _, ok := sc["outliers"].([]any); !ok {
			t.Errorf("outliers = %#v", sc["outliers"])
		}
		if got := text(t, res); !strings.Contains(got, "filesystem") {
			t.Errorf("text = %q", got)
		}
	})
}

func TestShellLevels(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("echo_json.yaml", "name: echo_json\ndescription: d\nworker: shell\nparams:\n  msg: {type: string, required: true}\nrequest:\n  command: echo\n  args: ['{\"msg\": \"{{ .msg }}\"}']\n")
	write("echo_lines.yaml", "name: echo_lines\ndescription: d\nworker: shell\nrequest:\n  command: printf\n  args: ['a\\nb\\n']\n  parse: lines\nresponse:\n  jq: 'map(ascii_upcase)'\n  render: '{{ join \",\" . }}'\n")
	write("count_lines.yaml", "name: count_lines\ndescription: d\nworker: shell\nrequest:\n  command: printf\n  args: ['a\\nb\\n']\n  parse: lines\nresponse:\n  jq: 'length'\n")
	write("empty.yaml", "name: empty\ndescription: d\nworker: shell\nrequest:\n  command: printf\n  args: ['']\n")
	write("fail.yaml", "name: fail\ndescription: d\nworker: shell\nrequest:\n  command: sh\n  args: [-c, 'echo nope >&2; exit 2']\n")
	sw, err := shell.New(shell.Config{Allowlist: []string{"echo", "printf", "sh"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, dir, worker.Registry{shell.Type: sw}, nil)

	res := call(t, cs, "echo_json", map[string]any{"msg": "hi"})
	if res.IsError || !reflect.DeepEqual(res.StructuredContent, map[string]any{"msg": "hi"}) {
		t.Errorf("echo_json = %#v (%s)", res.StructuredContent, text(t, res))
	}
	res = call(t, cs, "echo_lines", nil)
	if res.IsError || text(t, res) != "A,B" || !reflect.DeepEqual(wrapped(t, res), []any{"A", "B"}) {
		t.Errorf("echo_lines text = %q structured = %#v", text(t, res), res.StructuredContent)
	}
	res = call(t, cs, "count_lines", nil)
	if res.IsError || text(t, res) != "2" || wrapped(t, res) != float64(2) {
		t.Errorf("count_lines text = %q structured = %#v", text(t, res), res.StructuredContent)
	}
	res = call(t, cs, "empty", nil)
	if res.IsError || text(t, res) != "null" || res.StructuredContent != nil {
		t.Errorf("empty text = %q structured = %#v", text(t, res), res.StructuredContent)
	}
	res = call(t, cs, "fail", nil)
	if !res.IsError || !strings.Contains(text(t, res), "code 2") || !strings.Contains(text(t, res), "nope") {
		t.Errorf("fail: IsError = %v text = %q", res.IsError, text(t, res))
	}
}

// TestCallsErrorNamesCall checks that a failing call in a calls: tool is
// reported to the agent with its index and name.
func TestCallsErrorNamesCall(t *testing.T) {
	dir := t.TempDir()
	spec := "name: two\ndescription: d\ncalls:\n" +
		"  - {name: first, worker: shell, request: {command: printf, args: ['[1]']}}\n" +
		"  - {name: second, worker: shell, request: {command: sh, args: [-c, 'echo nope >&2; exit 2']}}\n"
	if err := os.WriteFile(filepath.Join(dir, "two.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	sw, err := shell.New(shell.Config{Allowlist: []string{"printf", "sh"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, dir, worker.Registry{shell.Type: sw}, nil)
	res := call(t, cs, "two", nil)
	got := text(t, res)
	if !res.IsError || !strings.Contains(got, "failed at stage worker: calls[1] (second): ") || !strings.Contains(got, "code 2") || !strings.Contains(got, "nope") {
		t.Errorf("IsError = %v text = %q", res.IsError, got)
	}
}

func TestHTTPHandlerHealthz(t *testing.T) {
	srv := New(nil, pipeline.New(nil), Options{})
	h := HTTPHandler(srv, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}
