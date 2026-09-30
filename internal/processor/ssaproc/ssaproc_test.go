package ssaproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	baseTS = 1757600000.0
	stepS  = 300.0 // 5 minutes: a day is 288 points
)

// vm is a VM metric: a level with a slow drift, a daily cycle with its second
// harmonic, and white noise of sd noise. clean(t) is the same without noise.
func vm(n int, noise float64, seed uint64) []float64 {
	rng := rand.New(rand.NewPCG(seed, 1))
	x := make([]float64, n)
	for i := range x {
		x[i] = clean(float64(i)) + noise*rng.NormFloat64()
	}
	return x
}

func clean(t float64) float64 {
	return 40 + 0.004*t + 8*math.Sin(2*math.Pi*t/288) + 3*math.Sin(4*math.Pi*t/288)
}

// matrix builds a Prometheus range result, one series per value slice.
func matrix(series ...[]float64) map[string]any {
	result := make([]any, 0, len(series))
	for s, vals := range series {
		values := make([]any, 0, len(vals))
		for i, v := range vals {
			if math.IsNaN(v) {
				continue // a missing sample
			}
			values = append(values, []any{baseTS + stepS*float64(i), strconv.FormatFloat(v, 'g', -1, 64)})
		}
		result = append(result, map[string]any{"metric": map[string]any{"instance": "vm-" + strconv.Itoa(s)}, "values": values})
	}
	return map[string]any{"resultType": "matrix", "result": result}
}

// multi builds the multivariate form of one object from columns.
func multi(dims []string, cols ...[]float64) map[string]any {
	points := make([]any, len(cols[0]))
	for i := range points {
		row := []any{baseTS + stepS*float64(i)}
		for _, c := range cols {
			row = append(row, c[i])
		}
		points[i] = row
	}
	return map[string]any{
		"dims":   toAnyList(dims),
		"series": []any{map[string]any{"labels": map[string]any{"instance": "vm-1"}, "points": points}},
	}
}

func run(t *testing.T, p *Processor, with, data map[string]any) map[string]any {
	t.Helper()
	out, err := p.Run(context.Background(), with, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	return out.(map[string]any)
}

func entry(t *testing.T, out map[string]any, i int) map[string]any {
	t.Helper()
	return out["series"].([]any)[i].(map[string]any)
}

func TestValidate(t *testing.T) {
	uni, mv := NewUni(), NewMulti()
	good := []map[string]any{
		nil,
		{"window": "1d", "forecast": "6h", "method": "k", "groups": "0;1-2", "clip": []any{0.0, 100.0}, "k": 0, "iters": 4, "min_points": 50, "max_gaps": 0.1, "reconstruction": true, "step": "5m"},
		{"window": "{{ .window }}", "forecast": "{{ .h }}", "groups": "{{ .groups }}", "method": "{{ .m }}", "clip": "{{ .clip }}"},
		{"window": 288, "clip": "0,100"},
	}
	for _, with := range good {
		if err := uni.Validate(with); err != nil {
			t.Errorf("ssa %v: %v", with, err)
		}
	}
	if err := mv.Validate(map[string]any{"normalize": false, "clip": map[string]any{"cpu": []any{0.0, 100.0}}, "min_delta": map[string]any{"cpu": 1.0}}); err != nil {
		t.Errorf("ssa_mv: %v", err)
	}
	bad := map[string]map[string]any{
		"unknown key":   {"windows": 10},
		"window 1":      {"window": 1},
		"window text":   {"window": "long"},
		"negative k":    {"k": -1},
		"groups":        {"groups": "0;a"},
		"groups repeat": {"groups": "1,1"},
		"method":        {"method": "arima"},
		"clip order":    {"clip": []any{100.0, 0.0}},
		"clip size":     {"clip": []any{1.0}},
		"clip object":   {"clip": map[string]any{"value": []any{0.0, 1.0}}},
		"min_points":    {"min_points": 2},
		"max_gaps":      {"max_gaps": 1.5},
		"step":          {"step": 0},
		"normalize":     {"normalize": true},
	}
	for name, with := range bad {
		if err := uni.Validate(with); err == nil {
			t.Errorf("ssa %s: expected an error", name)
		}
	}
	for name, with := range map[string]map[string]any{
		"min_delta sign":   {"min_delta": -1},
		"min_rel_delta":    {"min_rel_delta": -0.1},
		"clip member":      {"clip": map[string]any{"cpu": "5,1"}},
		"normalize string": {"normalize": "maybe"},
	} {
		if err := mv.Validate(with); err == nil {
			t.Errorf("ssa_mv %s: expected an error", name)
		}
	}
	// min_delta also floors the stretches away from the usual profile, so it
	// is valid without normalization, and ssa takes it too.
	if _, err := mv.Run(context.Background(), map[string]any{"normalize": false, "min_delta": 1}, multi([]string{"a"}, vm(100, 0.1, 1)), nil); err != nil {
		t.Errorf("min_delta without normalize: %v", err)
	}
	if err := uni.Validate(map[string]any{"min_delta": "{{ .d }}", "min_rel_delta": 0.1}); err != nil {
		t.Errorf("ssa min_delta: %v", err)
	}
	if err := uni.Validate(map[string]any{"min_delta": map[string]any{"cpu": 1.0}}); err == nil || !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("ssa min_delta naming a dim: %v", err)
	}
}

func TestRunUni(t *testing.T) {
	short := vm(10, 0.5, 9)
	out := run(t, NewUni(), map[string]any{"window": "1d"}, matrix(vm(2016, 0.5, 1), short))
	if out["series_total"] != 2 || out["series_analyzed"] != 1 || out["series_skipped"] != 1 {
		t.Fatalf("counters: %v %v %v", out["series_total"], out["series_analyzed"], out["series_skipped"])
	}
	if _, ok := out["dims"]; ok {
		t.Error("ssa reports no dims")
	}
	if e := entry(t, out, 1); e["skipped"] != true || !strings.Contains(e["reason"].(string), "min_points") {
		t.Errorf("short series: %v", e)
	}
	e := entry(t, out, 0)
	if e["window"] != 288 || e["K"] != 2016-288+1 || e["decomposition"] != "full" || e["grid_points"] != 2016 || e["gaps"] != 0 || e["step"] != stepS {
		t.Errorf("header: window %v K %v decomposition %v grid %v gaps %v step %v", e["window"], e["K"], e["decomposition"], e["grid_points"], e["gaps"], e["step"])
	}
	if len(e["spectrum"].([]any)) != spectrumShow {
		t.Errorf("spectrum has %d rows", len(e["spectrum"].([]any)))
	}
	noise := e["noise"].(map[string]any)
	if sd := noise["sd"].(float64); math.Abs(sd-0.5) > 0.05 {
		t.Errorf("noise sd %v, want ~0.5", sd)
	}
	comps := e["components"].([]any)
	daily := comps[1].(map[string]any)
	if daily["kind"] != "harmonic" || math.Abs(daily["period_seconds"].(float64)-86400) > 300 || daily["group"] != "1-2" {
		t.Errorf("daily component: %v", daily)
	}
	if s := daily["variance_share"].(float64); s < 0.5 {
		t.Errorf("daily share %v", s)
	}
	groups := e["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups: %v", groups)
	}
	g0, g1 := groups[0].(map[string]any), groups[1].(map[string]any)
	if g0["kind"] != "trend" || g0["source"] != "suggested" || g0["label"] != "trend" || g1["kind"] != "cycle" {
		t.Errorf("groups: %v / %v", g0, g1)
	}
	if d := g0["drift"].(float64); math.Abs(d-0.004*(2016-288)) > 1 {
		t.Errorf("trend drift %v", d)
	}
	if _, ok := g0["points"]; ok {
		t.Error("points without with.reconstruction")
	}
	ck := e["groups_check"].(map[string]any)
	if ck["separation"].(float64) > 0.1 || len(ck["left_out"].([]any)) != 0 {
		t.Errorf("check: %v", ck)
	}
	if ex := e["residual"].(map[string]any)["explained"].(float64); ex < 0.95 {
		t.Errorf("explained %v", ex)
	}
	if fs := e["findings"].([]any); len(fs) < 5 || fs[0].(map[string]any)["level"] == nil {
		t.Errorf("findings: %v", fs)
	}
	if _, ok := e["forecast"]; ok {
		t.Error("forecast without with.forecast")
	}
}

func TestRunForecast(t *testing.T) {
	const n = 2016
	for _, method := range []string{"L", "K"} {
		out := run(t, NewUni(), map[string]any{"window": "1d", "forecast": "12h", "method": method}, matrix(vm(n, 0.5, 2)))
		f := entry(t, out, 0)["forecast"].(map[string]any)
		if f["method"] != method || f["steps"] != 144 || f["source"] != "structure" || f["clipped"] != false {
			t.Fatalf("%s forecast header: %v", method, f)
		}
		if f["start"] != baseTS+stepS*n || f["end_time"] == nil {
			t.Errorf("%s start %v", method, f["start"])
		}
		pts := f["points"].([]any)
		var sq float64
		for i, p := range pts {
			row := p.([]any)
			if len(row) != 2 || row[0] != baseTS+stepS*float64(n+i) {
				t.Fatalf("%s row %d: %v", method, i, row)
			}
			d := row[1].(float64) - clean(float64(n+i))
			sq += d * d
		}
		if rmse := math.Sqrt(sq / float64(len(pts))); rmse > 0.3 {
			t.Errorf("%s forecast rmse %.3f against the clean continuation", method, rmse)
		}
	}
	// Clipping applies to a forecast that holds the level.
	out := run(t, NewUni(), map[string]any{"window": "1d", "forecast": 288, "clip": "0,45"}, matrix(vm(n, 0.5, 2)))
	f := entry(t, out, 0)["forecast"].(map[string]any)
	if f["clipped"] != true {
		t.Fatalf("clip: %v", f["clipped"])
	}
	top := 0.0
	for _, p := range f["points"].([]any) {
		top = math.Max(top, p.([]any)[1].(float64))
	}
	if top != 45 {
		t.Errorf("clipped forecast peaks at %v, want 45", top)
	}
	// Given groups: the forecast continues their union; the cycle alone has no level to clip.
	out = run(t, NewUni(), map[string]any{"window": "1d", "forecast": 10, "groups": "1-2;3-4", "clip": []any{0.0, 1.0}}, matrix(vm(n, 0.5, 2)))
	e := entry(t, out, 0)
	f = e["forecast"].(map[string]any)
	if f["group"] != "1-4" || f["source"] != "given" || f["clipped"] != false {
		t.Errorf("given groups forecast: %v", f)
	}
	if g := e["groups"].([]any)[0].(map[string]any); g["source"] != "given" || g["kind"] != "cycle" {
		t.Errorf("given group: %v", g)
	}
	// White noise has no structure to continue.
	rng := rand.New(rand.NewPCG(3, 3))
	white := make([]float64, 500)
	for i := range white {
		white[i] = rng.NormFloat64()
	}
	f = entry(t, run(t, NewUni(), map[string]any{"forecast": 10}, matrix(white)), 0)["forecast"].(map[string]any)
	if !strings.Contains(f["reason"].(string), "no structure") {
		t.Errorf("white noise forecast: %v", f)
	}
}

func TestRunGroupsAndReconstruction(t *testing.T) {
	x := vm(1000, 0.5, 4)
	out := run(t, NewUni(), map[string]any{"window": 288, "groups": "0;1-4", "reconstruction": true, "clip": []any{0.0, 50.0}}, matrix(x))
	e := entry(t, out, 0)
	groups := e["groups"].([]any)
	g0 := groups[0].(map[string]any)
	pts := g0["points"].([]any)
	if len(pts) != 1000 || g0["clipped"] != true || groups[1].(map[string]any)["clipped"] != false {
		t.Fatalf("reconstruction: %d points, clipped %v/%v", len(pts), g0["clipped"], groups[1].(map[string]any)["clipped"])
	}
	// Away from the ends, where reconstructions are less reliable, the level is the trend.
	if row := pts[500].([]any); row[0] != baseTS+500*stepS || math.Abs(row[1].(float64)-(40+0.004*500)) > 0.3 {
		t.Errorf("trend at 500: %v", row)
	}
	// A component the decomposition does not have skips the series.
	e = entry(t, run(t, NewUni(), map[string]any{"window": 20, "groups": "0;25"}, matrix(x)), 0)
	if e["skipped"] != true || !strings.Contains(e["reason"].(string), "with.groups: component 25") {
		t.Errorf("out of range group: %v", e["reason"])
	}
}

func TestRunGridAndWindow(t *testing.T) {
	x := vm(1000, 0.5, 5)
	for _, i := range []int{100, 101, 102, 500} {
		x[i] = math.NaN()
	}
	e := entry(t, run(t, NewUni(), map[string]any{"window": "1d"}, matrix(x)), 0)
	if e["skipped"] != false || e["gaps"] != 4 || e["points"] != 996 || e["grid_points"] != 1000 {
		t.Errorf("gaps: skipped %v gaps %v points %v grid %v", e["skipped"], e["gaps"], e["points"], e["grid_points"])
	}
	e = entry(t, run(t, NewUni(), map[string]any{"max_gaps": 0.001}, matrix(x)), 0)
	if e["skipped"] != true || !strings.Contains(e["reason"].(string), "max_gaps") {
		t.Errorf("max_gaps: %v", e["reason"])
	}
	// Default window n/3; a window longer than the series allows is skipped.
	if e = entry(t, run(t, NewUni(), nil, matrix(vm(300, 0.5, 6))), 0); e["window"] != 100 {
		t.Errorf("default window %v", e["window"])
	}
	e = entry(t, run(t, NewUni(), map[string]any{"window": "7d"}, matrix(vm(300, 0.5, 6))), 0)
	if e["skipped"] != true || !strings.Contains(e["reason"].(string), "exceeds 299") {
		t.Errorf("long window: %v", e["reason"])
	}
	// ssa folds a window longer than n/2 into n−L+1.
	if e = entry(t, run(t, NewUni(), map[string]any{"window": 250}, matrix(vm(300, 0.5, 6))), 0); e["window"] != 51 {
		t.Errorf("folded window %v", e["window"])
	}
	// Above the automatic limit the leading 30 components are computed.
	if e = entry(t, run(t, NewUni(), map[string]any{"window": "2d"}, matrix(vm(2016, 0.5, 7))), 0); e["decomposition"] != "top-30" {
		t.Errorf("automatic top-k: %v", e["decomposition"])
	}
}

func TestRunMulti(t *testing.T) {
	const n = 2016
	rng := rand.New(rand.NewPCG(8, 8))
	cpu, net := make([]float64, n), make([]float64, n)
	for i := range n {
		tt := float64(i)
		cpu[i] = 30 + 10*math.Sin(2*math.Pi*tt/288) + 6*math.Sin(2*math.Pi*tt/96) + 2*rng.NormFloat64()
		net[i] = 2e8 + 5e7*math.Sin(2*math.Pi*tt/288+1) + 2e7*math.Sin(4*math.Pi*tt/288) + 1e7*rng.NormFloat64()
	}
	dims := []string{"cpu", "net"}
	data := multi(dims, cpu, net)
	data["series"] = append(data["series"].([]any), map[string]any{"labels": map[string]any{"instance": "vm-2"}, "points": []any{}, "reason": "missing in input(s) cpu"})
	out := run(t, NewMulti(), map[string]any{"window": "2d", "forecast": "6h"}, data)
	if d := out["dims"].([]any); len(d) != 2 || out["series_skipped"] != 1 {
		t.Fatalf("dims %v, skipped %v", d, out["series_skipped"])
	}
	if e := entry(t, out, 1); e["reason"] != "missing in input(s) cpu" {
		t.Errorf("reason series: %v", e)
	}
	e := entry(t, out, 0)
	scale := e["scale"].(map[string]any)
	if s := scale["net"].(float64); s < 3e7 || s > 5e7 {
		t.Errorf("net scale %v", s)
	}
	sd := e["noise"].(map[string]any)["sd"].(map[string]any)
	if math.Abs(sd["cpu"].(float64)-2) > 0.3 || math.Abs(sd["net"].(float64)-1e7)/1e7 > 0.15 {
		t.Errorf("noise sd %v", sd)
	}
	// The 8h cycle belongs to cpu only.
	var h8 map[string]any
	for _, c := range e["components"].([]any) {
		c := c.(map[string]any)
		if p, ok := c["period_seconds"].(float64); ok && math.Abs(p-8*3600) < 600 {
			h8 = c
		}
	}
	if h8 == nil {
		t.Fatalf("8h cycle not found: %v", e["components"])
	}
	share := h8["variance_share"].(map[string]any)
	if share["cpu"].(float64) < 0.1 || share["net"].(float64) > 0.01 {
		t.Errorf("8h shares %v", share)
	}
	f := e["forecast"].(map[string]any)
	pts := f["points"].([]any)
	if f["steps"] != 72 || len(pts) != 72 || len(pts[0].([]any)) != 3 {
		t.Fatalf("forecast: steps %v rows %d", f["steps"], len(pts))
	}
	// The forecast is in each channel's own units.
	if v := pts[0].([]any)[2].(float64); v < 1e8 || v > 3e8 {
		t.Errorf("net forecast %v", v)
	}
	ex := e["residual"].(map[string]any)["explained"].(map[string]any)
	if ex["cpu"].(float64) < 0.8 || ex["net"].(float64) < 0.8 {
		t.Errorf("explained %v", ex)
	}

	// Without normalization the net scale swamps the decomposition: the 8h
	// cycle of cpu is no longer among the components.
	e = entry(t, run(t, NewMulti(), map[string]any{"window": "2d", "normalize": false}, multi(dims, cpu, net)), 0)
	if _, ok := e["scale"]; ok {
		t.Error("scale without normalization")
	}
	for _, c := range e["components"].([]any) {
		if p, ok := c.(map[string]any)["period_seconds"].(float64); ok && math.Abs(p-8*3600) < 600 {
			t.Errorf("raw scale: the 8h cycle should drown, got %v", c)
		}
	}
	// A window beyond (n+1)/2 is not folded in MSSA.
	e = entry(t, run(t, NewMulti(), map[string]any{"window": 1500}, multi(dims, cpu, net)), 0)
	if e["skipped"] != true || !strings.Contains(e["reason"].(string), "exceeds 1008") {
		t.Errorf("long MSSA window: %v", e["reason"])
	}
}

func TestRunMultiScaleFloor(t *testing.T) {
	// steal sits near zero: without a floor its own tiny spread is its scale.
	n := 600
	cpu, steal := vm(n, 0.5, 10), make([]float64, n)
	rng := rand.New(rand.NewPCG(11, 11))
	for i := range steal {
		steal[i] = 0.001 + 0.0001*rng.NormFloat64()
	}
	dims := []string{"cpu", "steal"}
	e := entry(t, run(t, NewMulti(), nil, multi(dims, cpu, steal)), 0)
	if s := e["scale"].(map[string]any)["steal"].(float64); s > 0.001 {
		t.Errorf("steal scale without a floor %v", s)
	}
	e = entry(t, run(t, NewMulti(), map[string]any{"min_delta": map[string]any{"steal": 1.0}}, multi(dims, cpu, steal)), 0)
	if s := e["scale"].(map[string]any)["steal"].(float64); s != 1 {
		t.Errorf("steal scale with min_delta %v, want 1", s)
	}
	if _, err := NewMulti().Run(context.Background(), map[string]any{"min_delta": map[string]any{"iowait": 1.0}}, multi(dims, cpu, steal), nil); err == nil || !strings.Contains(err.Error(), "unknown dimension(s) iowait") {
		t.Errorf("unknown min_delta dim: %v", err)
	}
	if _, err := NewMulti().Run(context.Background(), map[string]any{"clip": map[string]any{"mem": "0,1"}}, multi(dims, cpu, steal), nil); err == nil || !strings.Contains(err.Error(), "unknown dimension(s) mem") {
		t.Errorf("unknown clip dim: %v", err)
	}
}

func TestRunShapesAndCancel(t *testing.T) {
	if _, err := NewUni().Run(context.Background(), nil, multi([]string{"a"}, vm(50, 1, 1)), nil); err == nil || !strings.Contains(err.Error(), "matrix") {
		t.Errorf("ssa on the multivariate form: %v", err)
	}
	if _, err := NewMulti().Run(context.Background(), nil, matrix(vm(50, 1, 1)), nil); err == nil || !strings.Contains(err.Error(), "dims") {
		t.Errorf("ssa_mv on a matrix: %v", err)
	}
	if _, err := NewUni().Run(context.Background(), map[string]any{"window": "{{ .w }}"}, matrix(vm(50, 1, 1)), nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
		t.Errorf("unrendered with: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewUni().Run(ctx, nil, matrix(vm(500, 1, 1), vm(500, 1, 2)), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// An empty result is a valid, empty report.
	out := run(t, NewUni(), nil, map[string]any{"resultType": "matrix", "result": []any{}})
	if out["series_total"] != 0 || len(out["series"].([]any)) != 0 {
		t.Errorf("empty: %v", out)
	}
}

// An outage and its catch-up inside the history: the stretches are listed
// with their times, the series is broken and the forecast warns.
func TestRunRegimeBreak(t *testing.T) {
	const n = 1152
	x := vm(n, 0.5, 5)
	for i := 700; i < 850; i++ {
		x[i] = 0 // 12.5 hours without data flowing
	}
	for i := 850; i < 950; i++ {
		x[i] *= 2 // the catch-up
	}
	e := entry(t, run(t, NewUni(), map[string]any{"window": "1d", "forecast": "6h"}, matrix(x)), 0)
	if e["broken"] != true {
		t.Fatalf("broken = %v, runs %v", e["broken"], e["runs"])
	}
	var long []map[string]any
	for _, r := range e["runs"].([]any) {
		if r := r.(map[string]any); r["kind"] == runLong {
			long = append(long, r)
		}
	}
	if len(long) != 2 || long[0]["start"] != baseTS+stepS*700 || long[0]["deviation"].(float64) > -30 || long[1]["deviation"].(float64) < 30 {
		t.Fatalf("long runs %v", long)
	}
	if d := long[0]["duration"].(float64); math.Abs(d-150*stepS) > 3*stepS {
		t.Errorf("outage duration %v", d)
	}
	if w, _ := e["forecast"].(map[string]any)["warning"].(string); !strings.Contains(w, "stretch(es) in another regime, the longest") || !strings.Contains(w, "below the usual profile") {
		t.Errorf("forecast warning %q", w)
	}
	// The same VM without the incident: nothing long, no warning.
	e = entry(t, run(t, NewUni(), map[string]any{"window": "1d", "forecast": "6h"}, matrix(vm(n, 0.5, 5))), 0)
	if e["broken"] != false || e["forecast"].(map[string]any)["warning"] != nil {
		t.Errorf("clean VM: broken %v, runs %v, forecast %v", e["broken"], e["runs"], e["forecast"])
	}
	// ssa_mv names the dim of every stretch.
	e = entry(t, run(t, NewMulti(), map[string]any{"window": "1d"}, multi([]string{"cpu", "mem"}, x, vm(n, 0.5, 6))), 0)
	for _, r := range e["runs"].([]any) {
		if r := r.(map[string]any); r["kind"] == runLong && r["dim"] != "cpu" {
			t.Errorf("run of dim %v: %v", r["dim"], r)
		}
	}
	if e["broken"] != true {
		t.Errorf("ssa_mv broken = %v", e["broken"])
	}
}

// A weak harmonic pair growing within the history is left out of a long
// default forecast and reported with its growth; given groups keep it.
func TestRunForecastGrowing(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 41))
	const n = 1692
	x := make([]float64, n)
	for i := range x {
		tt := float64(i)
		x[i] = 17 + 7*math.Sin(2*math.Pi*tt/288) + 2*math.Sin(4*math.Pi*tt/288) +
			0.08*math.Pow(1.0015, tt)*math.Sin(2*math.Pi*tt/92) + 0.3*rng.NormFloat64()
	}
	e := entry(t, run(t, NewUni(), map[string]any{"window": "1d", "forecast": "3d"}, matrix(x)), 0)
	f := e["forecast"].(map[string]any)
	grow, ok := f["growing"].([]any)
	if !ok || len(grow) != 1 {
		t.Fatalf("growing = %v, forecast %v", f["growing"], f["group"])
	}
	g := grow[0].(map[string]any)
	comps := g["components"].([]any)
	if p := g["period"].(float64); math.Abs(p-92) > 3 || g["growth"].(float64) < 2 || g["growth_per_step"].(float64) < 1.001 {
		t.Errorf("growing pair %v", g)
	}
	left := f["left_out"].([]any)
	for _, c := range comps {
		if !slices.Contains(left, c) || strings.Contains(","+f["group"].(string)+",", fmt.Sprintf(",%v,", c)) {
			t.Errorf("component %v: left_out %v, group %v", c, left, f["group"])
		}
	}
	if w, _ := f["warning"].(string); strings.Contains(w, "growing") {
		t.Errorf("the forecast without the growing pair still warns: %s", w)
	}
	for _, c := range e["components"].([]any) {
		if _, ok := c.(map[string]any)["growth_per_step"]; !ok {
			t.Fatalf("component without growth_per_step: %v", c)
		}
	}
	// A short horizon keeps the pair.
	f = entry(t, run(t, NewUni(), map[string]any{"window": "1d", "forecast": "1h"}, matrix(x)), 0)["forecast"].(map[string]any)
	if _, ok := f["growing"]; ok {
		t.Errorf("an hour ahead: growing %v", f["growing"])
	}
}
