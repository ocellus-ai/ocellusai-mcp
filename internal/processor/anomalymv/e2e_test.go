package anomalymv_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/anomalymv"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/join"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

const baseTS = 1757600000.0

// promFake stands in for the prometheus worker: it answers the cpu and the
// memory range queries of testdata/vm_multivariate_anomalies.yaml with
// synthetic matrices and records the rendered requests.
type promFake struct {
	requests []map[string]any
}

func (*promFake) Type() string                  { return "prometheus" }
func (*promFake) Validate(map[string]any) error { return nil }
func (f *promFake) Execute(_ context.Context, req map[string]any) (any, error) {
	f.requests = append(f.requests, req)
	q, _ := req["query"].(string)
	switch {
	case strings.Contains(q, "node_cpu_seconds_total"):
		return matrix(series("vm-1:9100", cpuOf("vm-1")), series("vm-2:9100", cpuOf("vm-2"))), nil
	case strings.Contains(q, "node_memory_MemAvailable_bytes"):
		return matrix(series("vm-1:9100", memOf("vm-1")), series("vm-2:9100", memOf("vm-2"))), nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

// cpuOf/memOf: 30 samples per VM, memory following cpu (mem ≈ 0.4 + cpu with
// a thin scatter); vm-1 gets one joint anomaly at index 17 where cpu stays
// ordinary but memory jumps.
func cpuOf(string) []float64 {
	offsets := []float64{-0.02, -0.01, 0, 0.01, 0.02}
	out := make([]float64, 30)
	for i := range out {
		out[i] = 0.3 + offsets[i%len(offsets)]
	}
	return out
}

func memOf(vm string) []float64 {
	scatter := []float64{0.004, -0.002, -0.002}
	cpu := cpuOf(vm)
	out := make([]float64, len(cpu))
	for i := range out {
		out[i] = 0.4 + cpu[i] + scatter[i%len(scatter)]
	}
	if vm == "vm-1" {
		out[17] = 0.75
	}
	return out
}

func series(instance string, vals []float64) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		values = append(values, []any{baseTS + float64(60*i), fmt.Sprint(v)})
	}
	return map[string]any{"metric": map[string]any{"instance": instance, "job": "node"}, "values": values}
}

func matrix(items ...map[string]any) map[string]any {
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	return map[string]any{"resultType": "matrix", "result": list}
}

func loadReference(t *testing.T) (*catalog.Tool, *promFake) {
	t.Helper()
	fw := &promFake{}
	procs := processor.Registry{}
	for _, p := range []processor.Processor{join.New(), anomalymv.New()} {
		if err := procs.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := catalog.Load("testdata", worker.Registry{"prometheus": fw}, procs)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Spec.Name != "vm_multivariate_anomalies" {
		t.Fatalf("tools = %v", tools)
	}
	return tools[0], fw
}

func TestReferenceToolLoads(t *testing.T) {
	tool, _ := loadReference(t)
	if !tool.Named() || strings.Join(tool.WorkerNames(), ",") != "prometheus" || len(tool.Steps) != 2 || tool.Steps[0].Fn != "join" || tool.Steps[1].Fn != "anomaly_mv" {
		t.Errorf("tool = calls %v steps %v", tool.Named(), tool.Steps)
	}
}

func TestReferenceToolEndToEnd(t *testing.T) {
	tool, fw := loadReference(t)
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"instance": "vm-.*", "range": "1h"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fw.requests) != 2 {
		t.Fatalf("want 2 worker calls, got %d", len(fw.requests))
	}
	for i, req := range fw.requests {
		if req["type"] != "range" || req["start"] != "now-1h" || req["end"] != "now" || req["step"] != "60s" {
			t.Errorf("request[%d] = %v", i, req)
		}
		if q, _ := req["query"].(string); !strings.Contains(q, `instance=~"vm-.*"`) {
			t.Errorf("request[%d].query = %q", i, q)
		}
	}
	structured, ok := res.Structured.(map[string]any)
	if !ok {
		t.Fatalf("structured = %T", res.Structured)
	}
	if structured["total"] != 1 || structured["vms_analyzed"] != 2 || structured["vms_skipped"] != 0 {
		t.Errorf("structured = %v", structured)
	}
	vms := structured["vms"].([]any)
	if len(vms) != 1 {
		t.Fatalf("vms = %v", vms)
	}
	vm := vms[0].(map[string]any)
	if vm["instance"] != "vm-1:9100" || vm["count"] != 1 {
		t.Errorf("vm = %v", vm)
	}
	a := vm["anomalies"].([]any)[0].(map[string]any)
	if a["time"] != "2025-09-11T14:30:20Z" || a["dominant"] != "mem" {
		t.Errorf("anomaly = %v", a)
	}
	for _, want := range []string{
		"1 anomalous sample(s) over the last 1h (Mahalanobis distance > 3; 2 VM(s) analyzed, 0 skipped):",
		"vm-1:9100: 1 anomaly(ies)",
		"- 2025-09-11T14:30:20Z cpu=0.30 (expected 0.30) mem=0.75 (expected 0.70), driven by mem, score ",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
	if _, err := json.Marshal(res.Structured); err != nil {
		t.Errorf("structured must encode: %v", err)
	}
}

func TestReferenceToolThresholdParam(t *testing.T) {
	tool, _ := loadReference(t)
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"threshold": 20}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "No joint CPU/memory anomalies for instance .* over the last 2h (2 VM(s) analyzed, 0 skipped).") {
		t.Errorf("text = %q", res.Text)
	}
	if _, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"threshold": 0}`)); err == nil || !strings.Contains(err.Error(), "arguments: ") {
		t.Errorf("schema minimum must reject threshold 0: %v", err)
	}
}
