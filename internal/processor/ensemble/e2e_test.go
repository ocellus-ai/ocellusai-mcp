package ensemble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/ensemble"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/join"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

const (
	baseTS = 1757600000.0
	stepS  = 60.0
	n      = 720 // 12h of 1m samples
	// vm-1 memory decouples from cpu on [breakStart, breakEnd].
	breakStart, breakEnd = 500, 520
)

// promFake stands in for the prometheus worker: it answers the cpu and the
// memory range queries of testdata/vm_anomaly_ensemble.yaml with synthetic
// matrices and records the rendered requests.
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
		return matrix(sample("vm-1:9100", cpuOf("vm-1")), sample("vm-2:9100", cpuOf("vm-2"))), nil
	case strings.Contains(q, "node_memory_MemAvailable_bytes"):
		return matrix(sample("vm-1:9100", memOf("vm-1")), sample("vm-2:9100", memOf("vm-2"))), nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

func seedOf(vm string) int64 {
	if vm == "vm-1" {
		return 1
	}
	return 2
}

// cpuOf is a busy-percentage with two superimposed cycles and noise.
func cpuOf(vm string) []float64 {
	rng := rand.New(rand.NewSource(seedOf(vm)))
	out := make([]float64, n)
	for t := range out {
		base := math.Sin(2*math.Pi*float64(t)/120) + 0.3*math.Sin(2*math.Pi*float64(t)/37)
		out[t] = 30 + 8*base + rng.NormFloat64()*0.5
	}
	return out
}

// memOf follows cpu (mem ≈ 45 + (cpu-30)/2 with a thin scatter); on vm-1
// memory jumps for 21 samples while cpu stays ordinary.
func memOf(vm string) []float64 {
	rng := rand.New(rand.NewSource(seedOf(vm) + 10))
	cpu := cpuOf(vm)
	out := make([]float64, n)
	for t := range out {
		out[t] = 45 + 0.5*(cpu[t]-30) + rng.NormFloat64()*0.3
	}
	if vm == "vm-1" {
		for t := breakStart; t <= breakEnd; t++ {
			out[t] += 12
		}
	}
	return out
}

func sample(instance string, vals []float64) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		values = append(values, []any{baseTS + stepS*float64(i), fmt.Sprint(v)})
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
	for _, p := range []processor.Processor{join.New(), ensemble.New()} {
		if err := procs.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := catalog.Load("testdata", worker.Registry{"prometheus": fw}, procs)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Spec.Name != "vm_anomaly_ensemble" {
		t.Fatalf("tools = %v", tools)
	}
	return tools[0], fw
}

func TestReferenceToolLoads(t *testing.T) {
	tool, _ := loadReference(t)
	if !tool.Named() || strings.Join(tool.WorkerNames(), ",") != "prometheus" || len(tool.Steps) != 2 || tool.Steps[0].Fn != "join" || tool.Steps[1].Fn != "anomaly_ensemble" {
		t.Errorf("tool = calls %v steps %v", tool.Named(), tool.Steps)
	}
}

func TestReferenceToolEndToEnd(t *testing.T) {
	tool, fw := loadReference(t)
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"instance": "vm-.*", "range": "6h"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fw.requests) != 2 {
		t.Fatalf("want 2 worker calls, got %d", len(fw.requests))
	}
	for i, req := range fw.requests {
		if req["type"] != "range" || req["start"] != "now-6h" || req["end"] != "now" || req["step"] != "60s" {
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
	if structured["vms_analyzed"] != 2 || structured["vms_skipped"] != 0 {
		t.Errorf("structured = %v", structured)
	}
	vms := structured["vms"].([]any)
	if len(vms) == 0 {
		t.Fatalf("no VM with anomalies: %v", structured)
	}
	vm := vms[0].(map[string]any)
	if vm["instance"] != "vm-1:9100" || vm["window"] != 60 {
		t.Errorf("vm = %v", vm)
	}
	var hit map[string]any
	for _, a := range vm["anomalies"].([]any) {
		r := a.(map[string]any)
		if r["end_time"].(string) >= series.FormatTime(baseTS+stepS*breakStart) && r["start_time"].(string) <= series.FormatTime(baseTS+stepS*breakEnd) {
			hit = r
		}
	}
	if hit == nil {
		t.Fatalf("no segment over the memory jump %s..%s: %v", series.FormatTime(baseTS+stepS*breakStart), series.FormatTime(baseTS+stepS*breakEnd), vm["anomalies"])
	}
	top := hit["top"].([]any)
	if len(top) != 2 || top[0].(map[string]any)["metric"] != "mem" || hit["kind"] == "correlation" {
		t.Errorf("segment = %v", hit)
	}
	if fired := hit["fired_by"].([]any); len(fired) == 0 {
		t.Errorf("segment lacks fired_by: %v", hit)
	}
	mem, _ := hit["mem"].(float64)
	base, _ := hit["mem_baseline"].(float64)
	if mem < base+8 || base < 40 || base > 50 { // the jump is +12 over a baseline near 45
		t.Errorf("mem at peak %v vs baseline %v", hit["mem"], hit["mem_baseline"])
	}
	for _, other := range vms[1:] {
		t.Errorf("unexpected anomalies on a clean VM: %v", other)
	}
	for _, want := range []string{
		"anomalous segment(s) over the last 6h (2 VM(s) analyzed, 0 skipped; window 1h, risk 0.0001):",
		"vm-1:9100: ",
		", driven by mem (own z ",
		"% at the peak vs ",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
	if _, err := json.Marshal(res.Structured); err != nil {
		t.Errorf("structured must encode: %v", err)
	}
}

func TestReferenceToolParams(t *testing.T) {
	tool, _ := loadReference(t)
	// a huge window cannot be scored by the Matrix Profile (2*window >= n) and
	// falls out of the ensemble without failing the tool
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"window": "12h", "risk": 0.000001}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "over the last 12h (2 VM(s) analyzed, 0 skipped") {
		t.Errorf("text = %q", res.Text)
	}
	if _, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"risk": 0}`)); err == nil || !strings.Contains(err.Error(), "arguments: ") {
		t.Errorf("schema minimum must reject risk 0: %v", err)
	}
	if _, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(`{"window": "1s"}`)); err == nil || !strings.Contains(err.Error(), "process[1] (anomaly_ensemble): with.window: must be at least 4 points") {
		t.Errorf("window 1s must fail at the process stage: %v", err)
	}
}
