package ssaproc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/join"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/ssaproc"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

const (
	baseTS = 1757600000.0
	stepS  = 300.0
	n      = 2016 // 7 days of 5m samples
	day    = 288
)

// promFake stands in for the prometheus worker: it answers the cpu and the
// memory range queries of the testdata tools with synthetic matrices and
// records the rendered requests. vm-1 has a daily cycle on a rising level,
// vm-2 only noise around a flat level, vm-3 a short history.
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
		return matrix(sample("vm-1:9100", cpu1(n)), sample("vm-2:9100", flat(n, 5, 0.3, 2)), sample("vm-3:9100", flat(50, 20, 1, 3))), nil
	case strings.Contains(q, "node_memory_MemAvailable_bytes"):
		return matrix(sample("vm-1:9100", mem1(n)), sample("vm-2:9100", flat(n, 40, 0.2, 4)), sample("vm-3:9100", flat(50, 60, 1, 5))), nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

// cpu1Clean is vm-1's CPU without noise: a slowly rising level and a daily
// cycle, what a forecast should continue.
func cpu1Clean(t float64) float64 { return 30 + 0.002*t + 10*math.Sin(2*math.Pi*t/day) }

func cpu1(m int) []float64 {
	rng := rand.New(rand.NewPCG(1, 1))
	out := make([]float64, m)
	for i := range out {
		out[i] = cpu1Clean(float64(i)) + rng.NormFloat64()
	}
	return out
}

// mem1 follows the daily cycle of cpu with a lag and has its own 12h swing.
func mem1(m int) []float64 {
	rng := rand.New(rand.NewPCG(6, 6))
	out := make([]float64, m)
	for i := range out {
		t := float64(i)
		out[i] = 60 + 5*math.Sin(2*math.Pi*t/day-0.5) + 2*math.Sin(4*math.Pi*t/day) + 0.3*rng.NormFloat64()
	}
	return out
}

func flat(m int, level, noise float64, seed uint64) []float64 {
	rng := rand.New(rand.NewPCG(seed, 9))
	out := make([]float64, m)
	for i := range out {
		out[i] = level + noise*rng.NormFloat64()
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

func loadReference(t *testing.T) (map[string]*catalog.Tool, *promFake) {
	t.Helper()
	fw := &promFake{}
	procs := processor.Registry{}
	for _, p := range []processor.Processor{join.New(), ssaproc.NewUni(), ssaproc.NewMulti()} {
		if err := procs.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := catalog.Load("testdata", worker.Registry{"prometheus": fw}, procs)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*catalog.Tool{}
	for _, tl := range tools {
		byName[tl.Spec.Name] = tl
	}
	if len(byName) != 2 || byName["vm_cpu_forecast"] == nil || byName["vm_cpu_mem_mssa"] == nil {
		t.Fatalf("tools = %v", byName)
	}
	return byName, fw
}

func runTool(t *testing.T, tool *catalog.Tool, args string) (*pipeline.Result, map[string]any) {
	t.Helper()
	res, err := pipeline.New(nil).Run(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(res.Structured); err != nil {
		t.Fatalf("structured must encode: %v", err)
	}
	return res, res.Structured.(map[string]any)
}

func vmByName(t *testing.T, s map[string]any, instance string) map[string]any {
	t.Helper()
	for _, v := range s["vms"].([]any) {
		if v := v.(map[string]any); v["instance"] == instance {
			return v
		}
	}
	t.Fatalf("no %s in %v", instance, s["vms"])
	return nil
}

func TestReferenceSSA(t *testing.T) {
	tools, fw := loadReference(t)
	res, s := runTool(t, tools["vm_cpu_forecast"], `{"instance": "vm-.*"}`)
	if len(fw.requests) != 1 {
		t.Fatalf("want 1 worker call, got %d", len(fw.requests))
	}
	if req := fw.requests[0]; req["type"] != "range" || req["start"] != "now-1w" || req["step"] != "5m" {
		t.Errorf("request = %v", req)
	}
	if s["analyzed"] != 2 {
		t.Errorf("analyzed = %v", s["analyzed"])
	}
	skipped := s["skipped"].([]any)
	if len(skipped) != 1 || !strings.Contains(skipped[0].(map[string]any)["reason"].(string), "fewer than 100 points") {
		t.Errorf("skipped = %v", skipped)
	}
	vm1 := vmByName(t, s, "vm-1:9100")
	if vm1["window"] != day {
		t.Errorf("window = %v", vm1["window"])
	}
	structure := strings.Join(toStrings(vm1["structure"]), " | ")
	if !strings.Contains(structure, "0: trend") || !strings.Contains(structure, "1-2: harmonic, period 28") {
		t.Errorf("structure = %s", structure)
	}
	f := vm1["forecast"].(map[string]any)
	// The forecast continues the rising daily cycle: its end is a day after
	// the history, at the same phase, so near the clean value there.
	end := float64(n + day - 1)
	if last := f["last"].(float64); math.Abs(last-cpu1Clean(end)) > 1 {
		t.Errorf("forecast end %v, clean %v", last, cpu1Clean(end))
	}
	if peak := f["peak"].(map[string]any)["value"].(float64); math.Abs(peak-(40+0.002*(n+day/4))) > 1.5 {
		t.Errorf("forecast peak %v", peak)
	}
	vm2 := vmByName(t, s, "vm-2:9100")
	if sd := vm2["noise_sd"].(float64); math.Abs(sd-0.3) > 0.05 {
		t.Errorf("vm-2 noise sd %v", sd)
	}
	for _, want := range []string{
		"2 VM(s) analyzed over the last 7d (window 1d, forecast 1d ahead, method L); skipped: vm-3:9100 (fewer than 100 points (min_points)).",
		"vm-1:9100: 0: trend",
		"  forecast of 0-2 to ",
		"vm-2:9100: ",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
}

func TestReferenceSSAParams(t *testing.T) {
	tools, _ := loadReference(t)
	_, s := runTool(t, tools["vm_cpu_forecast"], `{"groups": "0", "method": "K", "horizon": "6h"}`)
	f := vmByName(t, s, "vm-1:9100")["forecast"].(map[string]any)
	if f["group"] != "0" || f["end_time"] == nil {
		t.Errorf("forecast = %v", f)
	}
	// A window off the daily period is reported as a warning; the period
	// found on a week of history is near a day, so the advice is the day.
	_, s = runTool(t, tools["vm_cpu_forecast"], `{"window": "20h"}`)
	if w := strings.Join(toStrings(vmByName(t, s, "vm-1:9100")["warnings"]), " "); !strings.Contains(w, "most likely that cycle") || !strings.Contains(w, "set with.window to 288 points (1d)") {
		t.Errorf("warnings = %s", w)
	}
	if _, err := pipeline.New(nil).Run(context.Background(), tools["vm_cpu_forecast"], json.RawMessage(`{"groups": "a"}`)); err == nil || !strings.Contains(err.Error(), "arguments: ") {
		t.Errorf("the schema pattern must reject groups a: %v", err)
	}
	if _, err := pipeline.New(nil).Run(context.Background(), tools["vm_cpu_forecast"], json.RawMessage(`{"groups": "1,1"}`)); err == nil || !strings.Contains(err.Error(), "process[0] (ssa): with.groups: component 1 listed twice") {
		t.Errorf("a repeated component must fail at the process stage: %v", err)
	}
}

func TestReferenceMSSA(t *testing.T) {
	tools, fw := loadReference(t)
	res, s := runTool(t, tools["vm_cpu_mem_mssa"], `{"horizon": "12h"}`)
	if len(fw.requests) != 2 {
		t.Fatalf("want 2 worker calls, got %d", len(fw.requests))
	}
	if s["analyzed"] != 2 || len(s["skipped"].([]any)) != 1 {
		t.Errorf("analyzed %v, skipped %v", s["analyzed"], s["skipped"])
	}
	vm1 := vmByName(t, s, "vm-1:9100")
	// The daily cycle is common; the 12h swing is memory's own.
	var daily, half map[string]any
	for _, c := range vm1["structure"].([]any) {
		c := c.(map[string]any)
		m := c["meaning"].(string)
		switch {
		case strings.HasPrefix(m, "harmonic, period 288"):
			daily = c
		case strings.HasPrefix(m, "harmonic, period 144"):
			half = c
		}
	}
	if daily == nil || half == nil {
		t.Fatalf("structure = %v", vm1["structure"])
	}
	if daily["cpu"].(float64) < 0.5 || daily["mem"].(float64) < 0.5 {
		t.Errorf("daily shares cpu %v mem %v", daily["cpu"], daily["mem"])
	}
	if half["mem"].(float64) < 0.05 || half["cpu"].(float64) > 0.01 {
		t.Errorf("12h shares cpu %v mem %v", half["cpu"], half["mem"])
	}
	f := vm1["forecast"].(map[string]any)
	end := float64(n + day/2 - 1)
	if v := f["cpu_end"].(float64); math.Abs(v-cpu1Clean(end)) > 1.5 {
		t.Errorf("cpu forecast end %v, clean %v", v, cpu1Clean(end))
	}
	if v := f["mem_end"].(float64); v < 50 || v > 70 {
		t.Errorf("mem forecast end %v", v)
	}
	// vm-2 is flat: its mem spread (0.2) is under min_delta, so its scale is 1.
	if sc := vmByName(t, s, "vm-2:9100")["scale"].(map[string]any); sc["mem"] != 1.0 {
		t.Errorf("vm-2 scale %v", sc)
	}
	for _, want := range []string{
		"2 VM(s) analyzed over the last 7d (window 1d, forecast 12h ahead); skipped: vm-3:9100 (fewer than 100 points (min_points)).",
		"vm-1:9100: the structure explains ",
		"of their variance)",
		"  forecast of ",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
