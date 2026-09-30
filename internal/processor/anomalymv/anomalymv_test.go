package anomalymv

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

const baseTS = 1757600000.0

// noise is the scatter of the correlated test data around y = x.
const noise = 0.01

// correlated returns n samples with x = i/n and y = x ± noise (alternating),
// i.e. two strongly correlated inputs with a thin scatter around y = x.
func correlated(n int) (xs, ys []float64) {
	xs = make([]float64, n)
	ys = make([]float64, n)
	for i := range xs {
		xs[i] = float64(i) / float64(n)
		if i%2 == 0 {
			ys[i] = xs[i] + noise
		} else {
			ys[i] = xs[i] - noise
		}
	}
	return xs, ys
}

// multi builds one multivariate series with one sample per minute.
func multi(labels map[string]string, dims []string, cols ...[]float64) Series {
	s := Series{Labels: labels, Dims: dims}
	for i := range cols[0] {
		v := make([]float64, len(cols))
		for d := range cols {
			v[d] = cols[d][i]
		}
		s.Points = append(s.Points, Point{T: baseTS + float64(60*i), V: v})
	}
	return s
}

var cpuMem = []string{"cpu", "mem"}

// form writes the given series in the multivariate form the processor parses.
func form(dims []string, ss ...Series) map[string]any {
	return series.ToMulti(series.Multi{Dims: dims, Series: ss}, nil)
}

func vm(name string) map[string]string { return map[string]string{"instance": name} }

func run(t *testing.T, with map[string]any, data any) map[string]any {
	t.Helper()
	out, err := New().Run(context.Background(), with, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func withM(extra map[string]any) map[string]any {
	w := map[string]any{"method": "mahalanobis"}
	for k, v := range extra {
		w[k] = v
	}
	return w
}

func TestDetectorsTable(t *testing.T) {
	if got := Detectors(); !reflect.DeepEqual(got, []string{"mahalanobis"}) {
		t.Errorf("Detectors() = %v", got)
	}
	for name, d := range detectors {
		if d.Name() != name {
			t.Errorf("detector %q reports Name() %q", name, d.Name())
		}
	}
	if New().Name() != "anomaly_mv" {
		t.Errorf("Name() = %q", New().Name())
	}
}

func TestInvert(t *testing.T) {
	inv, ok := invert([][]float64{{2, 1}, {1, 2}})
	if !ok {
		t.Fatal("matrix is invertible")
	}
	want := [][]float64{{2.0 / 3, -1.0 / 3}, {-1.0 / 3, 2.0 / 3}}
	for i := range want {
		for j := range want[i] {
			if math.Abs(inv[i][j]-want[i][j]) > 1e-12 {
				t.Errorf("inv[%d][%d] = %v, want %v", i, j, inv[i][j], want[i][j])
			}
		}
	}
	if _, ok := invert([][]float64{{1, 2}, {2, 4}}); ok {
		t.Error("collinear matrix must be singular")
	}
	if _, ok := invert([][]float64{{1, 0}, {0, 0}}); ok {
		t.Error("zero-variance dimension must be singular")
	}
	if _, ok := invert([][]float64{{0, 0}, {0, 0}}); ok {
		t.Error("zero matrix must be singular")
	}
	// Pivoting: a zero on the diagonal is fine when rows can be swapped.
	inv, ok = invert([][]float64{{0, 1}, {1, 0}})
	if !ok || inv[0][1] != 1 || inv[1][0] != 1 || inv[0][0] != 0 {
		t.Errorf("permutation matrix: ok=%v inv=%v", ok, inv)
	}
	if got := quadratic([][]float64{{2, 0}, {0, 3}}, []float64{1, 2}); got != 2+12 {
		t.Errorf("quadratic = %v, want 14", got)
	}
}

func TestCovariance(t *testing.T) {
	s := multi(nil, []string{"x", "y"}, []float64{1, 2, 3, 4}, []float64{2, 4, 6, 8})
	mu := means(s)
	if !reflect.DeepEqual(mu, []float64{2.5, 5}) {
		t.Errorf("means = %v", mu)
	}
	cov := covariance(s, mu)
	want := [][]float64{{1.25, 2.5}, {2.5, 5}}
	if !reflect.DeepEqual(cov, want) {
		t.Errorf("covariance = %v, want %v", cov, want)
	}
	if _, ok := invert(cov); ok {
		t.Error("y = 2x is perfectly collinear: covariance must be singular")
	}
}

// TestMahalanobisJointAnomaly is the multivariate case: a sample whose every
// coordinate is within its own normal range but which breaks the correlation
// between the inputs scores high, while a sample far along the correlation
// line does not.
func TestMahalanobisJointAnomaly(t *testing.T) {
	xs, ys := correlated(40)
	xs = append(xs, 1.2, 0.5) // A: on the line beyond the range; B: off the line
	ys = append(ys, 1.2, 0.8)
	s := multi(vm("vm-1"), cpuMem, xs, ys)

	v, err := (mahalanobis{}).Detect(map[string]any{}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Skip != "" {
		t.Fatalf("unexpected skip: %s", v.Skip)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 41 {
		t.Fatalf("want exactly the off-line sample (index 41) flagged, got %+v", v.Anomalies)
	}
	a := v.Anomalies[0]
	if a.Score <= 3 {
		t.Errorf("score = %v, want > threshold 3", a.Score)
	}
	// Per axis the sample is unremarkable: this is what a per-metric detector would see.
	for i, d := range a.Deviation {
		if math.Abs(d) >= 3 {
			t.Errorf("deviation[%s] = %v, want |z| < 3 (the anomaly is joint, not per-metric)", s.Dims[i], d)
		}
	}
	if a.Deviation[1] <= a.Deviation[0] {
		t.Errorf("mem should deviate more than cpu: %v", a.Deviation)
	}
	if len(a.Expected) != 2 || math.Abs(a.Expected[0]-mean(xs)) > 1e-12 {
		t.Errorf("expected = %v, want the mean vector", a.Expected)
	}
	for _, k := range []string{"mean_cpu", "mean_mem", "stddev_cpu", "stddev_mem", "corr_cpu_mem", "threshold", "max_distance"} {
		if _, ok := v.Stats[k]; !ok {
			t.Errorf("stats missing %q: %v", k, v.Stats)
		}
	}
	if v.Stats["corr_cpu_mem"] < 0.95 {
		t.Errorf("corr = %v, want strongly positive", v.Stats["corr_cpu_mem"])
	}
	if v.Stats["max_distance"] != a.Score {
		t.Errorf("max_distance = %v, want the top score %v", v.Stats["max_distance"], a.Score)
	}
}

func TestMahalanobisThreshold(t *testing.T) {
	xs, ys := correlated(40)
	xs = append(xs, 1.2)
	ys = append(ys, 1.2)
	s := multi(nil, cpuMem, xs, ys)
	// The on-line sample is ~2.4σ along the correlation line: a lower threshold catches it.
	v, err := (mahalanobis{}).Detect(map[string]any{"threshold": "2"}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 40 {
		t.Errorf("threshold 2 should flag the far on-line sample: %+v", v.Anomalies)
	}
	if v.Stats["threshold"] != 2 {
		t.Errorf("stats.threshold = %v", v.Stats["threshold"])
	}
	if _, err := (mahalanobis{}).Detect(map[string]any{"threshold": "{{ .t }}"}, s, nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
		t.Errorf("unrendered template must fail at run time: %v", err)
	}
}

func TestMahalanobisThreeDimensions(t *testing.T) {
	xs, ys := correlated(30)
	zs := make([]float64, len(xs))
	for i := range zs {
		zs[i] = 0.5 - xs[i]/2 + float64(i%3)*0.004
	}
	xs = append(xs, 0.5)
	ys = append(ys, 0.5)
	zs = append(zs, 0.9) // only the third input breaks the pattern
	s := multi(nil, []string{"cpu", "mem", "disk"}, xs, ys, zs)
	v, err := (mahalanobis{}).Detect(map[string]any{}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 30 {
		t.Fatalf("anomalies = %+v", v.Anomalies)
	}
	if got := v.Anomalies[0].Deviation; len(got) != 3 || math.Abs(got[2]) <= math.Abs(got[0]) || math.Abs(got[2]) <= math.Abs(got[1]) {
		t.Errorf("disk must dominate the deviation: %v", got)
	}
	for _, k := range []string{"corr_cpu_mem", "corr_cpu_disk", "corr_mem_disk", "mean_disk"} {
		if _, ok := v.Stats[k]; !ok {
			t.Errorf("stats missing %q", k)
		}
	}
}

func TestMahalanobisSingular(t *testing.T) {
	xs, _ := correlated(20)
	flat := make([]float64, len(xs))
	for i := range flat {
		flat[i] = 0.5
	}
	v, err := (mahalanobis{}).Detect(map[string]any{}, multi(nil, cpuMem, xs, flat), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Skip, "singular") || len(v.Anomalies) != 0 {
		t.Errorf("constant input must skip the series: %+v", v)
	}
	if v.Stats["stddev_mem"] != 0 {
		t.Errorf("stats should still be reported: %v", v.Stats)
	}
	if _, ok := v.Stats["corr_cpu_mem"]; ok {
		t.Error("correlation with a constant input is undefined and must be omitted")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with map[string]any
		err  string
	}{
		{"ok", withM(map[string]any{"threshold": 2.5, "min_points": 5, "max_anomalies": 0}), ""},
		{"method only", withM(nil), ""},
		{"templated values deferred", withM(map[string]any{"threshold": "{{ .t }}", "min_points": "{{ .n }}"}), ""},
		{"method missing", map[string]any{"threshold": 3}, "with.method: required"},
		{"method template", withM(map[string]any{"method": "{{ .m }}"}), "must be a literal"},
		{"method unknown", withM(map[string]any{"method": "lof"}), `unknown detector "lof" (available: mahalanobis)`},
		{"min_points zero", withM(map[string]any{"min_points": 0}), "with.min_points: must be >= 1"},
		{"max_anomalies negative", withM(map[string]any{"max_anomalies": -1}), "with.max_anomalies: must be >= 0"},
		{"unknown key", withM(map[string]any{"k": 1}), "method mahalanobis: with: unknown field(s) k (allowed: threshold)"},
		// The join keys moved to the join processor: an old tool fails fast at start.
		{"join keys", withM(map[string]any{"inputs": []any{"cpu", "mem"}, "join_by": "instance"}), "method mahalanobis: with: unknown field(s) inputs, join_by (allowed: threshold)"},
		{"threshold zero", withM(map[string]any{"threshold": 0}), "with.threshold: must be > 0"},
		{"threshold text", withM(map[string]any{"threshold": "high"}), `with.threshold: "high" is not a number`},
		{"min_delta number", withM(map[string]any{"min_delta": 5, "min_rel_delta": 0.2}), ""},
		{"min_delta per dim", withM(map[string]any{"min_delta": map[string]any{"cpu": 5, "mem": "{{ .m }}"}, "min_rel_delta": "{{ .r }}"}), ""},
		{"min_delta negative", withM(map[string]any{"min_delta": map[string]any{"cpu": -5}}), "with.min_delta.cpu: must be >= 0"},
		{"min_rel_delta negative", withM(map[string]any{"min_rel_delta": -0.1}), "with.min_rel_delta: must be >= 0 (0 = off)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := New().Validate(tc.with)
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestRunUnrendered(t *testing.T) {
	data := form(cpuMem)
	for _, tc := range []struct {
		with map[string]any
		err  string
	}{
		{withM(map[string]any{"min_points": "{{ .n }}"}), "with.min_points: unrendered template"},
		{withM(map[string]any{"max_anomalies": "{{ .n }}"}), "with.max_anomalies: unrendered template"},
	} {
		if _, err := New().Run(context.Background(), tc.with, data, nil); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("with %v: error = %v, want %q", tc.with, err, tc.err)
		}
	}
}

// vmData returns vm-1 (with one joint anomaly at index 17) and vm-2 (normal),
// 30 samples each.
func vmData() (vm1, vm2 Series) {
	cpu1, mem1 := correlated(30)
	cpu1[17], mem1[17] = 0.5, 0.8
	cpu2, mem2 := correlated(30)
	return multi(vm("vm-1"), cpuMem, cpu1, mem1), multi(vm("vm-2"), cpuMem, cpu2, mem2)
}

func TestRunReport(t *testing.T) {
	vm1, vm2 := vmData()
	// Two objects the join could not build: they keep their reason.
	unjoined := Series{Labels: map[string]string{"job": "node"}, Dims: cpuMem, Reason: "input mem: missing join_by label(s) instance"}
	vm3 := Series{Labels: vm("vm-3"), Dims: cpuMem, Reason: "missing in input(s) cpu"}
	data := form(cpuMem, unjoined, vm1, vm2, vm3)

	out := run(t, withM(map[string]any{"threshold": "3", "min_points": "10"}), data)
	if out["method"] != "mahalanobis" || !reflect.DeepEqual(out["dims"], []any{"cpu", "mem"}) {
		t.Errorf("header = %v", out)
	}
	if out["series_total"] != 4 || out["series_analyzed"] != 2 || out["series_skipped"] != 2 || out["total_anomalies"] != 1 {
		t.Errorf("counters = total %v analyzed %v skipped %v anomalies %v", out["series_total"], out["series_analyzed"], out["series_skipped"], out["total_anomalies"])
	}
	list := out["series"].([]any)
	byName := map[string]map[string]any{}
	for _, e := range list {
		entry := e.(map[string]any)
		inst, _ := entry["labels"].(map[string]any)["instance"].(string)
		byName[inst] = entry
	}
	e1 := byName["vm-1"]
	if e1["skipped"] != false || e1["points"] != 30 || e1["anomaly_count"] != 1 {
		t.Fatalf("vm-1 = %v", e1)
	}
	if !reflect.DeepEqual(e1["labels"], map[string]any{"instance": "vm-1"}) {
		t.Errorf("labels = %v", e1["labels"])
	}
	a := e1["anomalies"].([]any)[0].(map[string]any)
	if a["timestamp"] != baseTS+17*60 || a["time"] != "2025-09-11T14:30:20Z" {
		t.Errorf("timestamp = %v time = %v", a["timestamp"], a["time"])
	}
	if !reflect.DeepEqual(a["values"], map[string]any{"cpu": 0.5, "mem": 0.8}) {
		t.Errorf("values = %v", a["values"])
	}
	if a["dominant"] != "mem" {
		t.Errorf("dominant = %v", a["dominant"])
	}
	for _, k := range []string{"expected", "deviation"} {
		m, ok := a[k].(map[string]any)
		if !ok || len(m) != 2 {
			t.Errorf("%s = %v, want per-dim map", k, a[k])
		}
	}
	if s, _ := a["score"].(float64); s <= 3 {
		t.Errorf("score = %v", a["score"])
	}
	if _, ok := e1["stats"].(map[string]any)["corr_cpu_mem"]; !ok {
		t.Errorf("stats = %v", e1["stats"])
	}
	if e2 := byName["vm-2"]; e2["anomaly_count"] != 0 || len(e2["anomalies"].([]any)) != 0 {
		t.Errorf("vm-2 = %v", e2)
	}
	if e3 := byName["vm-3"]; e3["skipped"] != true || e3["reason"] != "missing in input(s) cpu" || e3["points"] != 0 {
		t.Errorf("vm-3 = %v", e3)
	}
	if e0 := byName[""]; e0["skipped"] != true || e0["reason"] != "input mem: missing join_by label(s) instance" || !reflect.DeepEqual(e0["labels"], map[string]any{"job": "node"}) {
		t.Errorf("unjoined series = %v", e0)
	}
	// The input order is preserved.
	if first := list[0].(map[string]any); first["reason"] != unjoined.Reason {
		t.Errorf("first entry = %v", first)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("report must be JSON-compatible: %v", err)
	}
}

func TestRunMinPoints(t *testing.T) {
	cpu, mem := correlated(22)
	data := form(cpuMem, multi(vm("vm-1"), cpuMem, cpu, mem))
	out := run(t, withM(map[string]any{"min_points": 20}), data)
	entry := out["series"].([]any)[0].(map[string]any)
	if entry["points"] != 22 || entry["skipped"] != false {
		t.Errorf("entry = %v", entry)
	}
	out = run(t, withM(map[string]any{"min_points": 23}), data)
	entry = out["series"].([]any)[0].(map[string]any)
	if entry["skipped"] != true || entry["reason"] != "fewer than 23 points (min_points)" || entry["points"] != 22 {
		t.Errorf("entry = %v", entry)
	}
	if out["series_skipped"] != 1 || out["series_analyzed"] != 0 {
		t.Errorf("counters = %v", out)
	}
	// A series with a reason is skipped even when it carries points.
	withReason := multi(vm("vm-2"), cpuMem, cpu, mem)
	withReason.Reason = "partial"
	out = run(t, withM(nil), form(cpuMem, withReason))
	if entry := out["series"].([]any)[0].(map[string]any); entry["skipped"] != true || entry["reason"] != "partial" || entry["points"] != 22 {
		t.Errorf("entry = %v", entry)
	}
}

func TestRunSingularSeriesSkipped(t *testing.T) {
	cpu, _ := correlated(20)
	flat := make([]float64, 20)
	for i := range flat {
		flat[i] = 1
	}
	out := run(t, withM(nil), form(cpuMem, multi(vm("vm-1"), cpuMem, cpu, flat)))
	entry := out["series"].([]any)[0].(map[string]any)
	if entry["skipped"] != true || !strings.Contains(entry["reason"].(string), "singular") || out["series_skipped"] != 1 {
		t.Errorf("entry = %v", entry)
	}
	if _, ok := entry["stats"]; !ok {
		t.Error("stats should be reported for a degenerate series too")
	}
}

func TestRunCapAndOrder(t *testing.T) {
	// A long window keeps the three injected outliers from inflating the
	// covariance (classical Mahalanobis is not robust to them).
	cpu, mem := correlated(200)
	cpu[5], mem[5] = 0.5, 0.8   // medium
	cpu[20], mem[20] = 0.5, 0.9 // strongest
	cpu[30], mem[30] = 0.5, 0.7 // weakest
	data := form(cpuMem, multi(vm("vm-1"), cpuMem, cpu, mem))

	out := run(t, withM(map[string]any{"max_anomalies": 2}), data)
	entry := out["series"].([]any)[0].(map[string]any)
	if entry["anomaly_count"] != 3 || out["total_anomalies"] != 3 {
		t.Fatalf("anomaly_count = %v (all anomalies are counted before the cap)", entry["anomaly_count"])
	}
	got := entry["anomalies"].([]any)
	if len(got) != 2 {
		t.Fatalf("anomalies = %v", got)
	}
	t0 := got[0].(map[string]any)["timestamp"].(float64)
	t1 := got[1].(map[string]any)["timestamp"].(float64)
	if t0 != baseTS+5*60 || t1 != baseTS+20*60 {
		t.Errorf("want the two strongest in time order (5, 20), got %v, %v", t0, t1)
	}
	out = run(t, withM(map[string]any{"max_anomalies": 0}), data)
	if n := len(out["series"].([]any)[0].(map[string]any)["anomalies"].([]any)); n != 3 {
		t.Errorf("max_anomalies 0 = unlimited, got %d", n)
	}
}

func TestRunErrors(t *testing.T) {
	cpu, _ := correlated(12)
	matrix := map[string]any{"resultType": "matrix", "result": []any{}}
	for _, tc := range []struct {
		name string
		data any
		err  string
	}{
		{"not an object", []any{1}, "expected a multivariate series object {dims: [...], series: [...]} (the output of fn: join), got array"},
		{"matrix instead of multi", matrix, "dims: expected a non-empty list of dimension names, got null"},
		{"calls object", map[string]any{"cpu": matrix, "mem": matrix}, "dims: expected a non-empty list of dimension names, got null"},
		{"one dim", form([]string{"cpu"}, multi(vm("vm-1"), []string{"cpu"}, cpu)), "need at least 2 dims to detect multivariate anomalies, got dims [cpu] (join at least two inputs)"},
		{"bad point", map[string]any{"dims": []any{"cpu", "mem"}, "series": []any{map[string]any{"points": []any{[]any{1, "x", 2}}}}}, `series[0].points[0]: cpu: "x" is not a number`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Run(context.Background(), withM(nil), tc.data, nil)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestRunEmpty(t *testing.T) {
	out := run(t, withM(nil), form(cpuMem))
	if out["series_total"] != 0 || len(out["series"].([]any)) != 0 || out["total_anomalies"] != 0 {
		t.Errorf("empty report = %v", out)
	}
}

func TestRunContextCancel(t *testing.T) {
	vm1, vm2 := vmData()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(ctx, withM(nil), form(cpuMem, vm1, vm2), nil); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %v", err)
	}
}

func TestCapBySeverity(t *testing.T) {
	in := []Anomaly{{Index: 1, Score: 1}, {Index: 2, Score: 9}, {Index: 3, Score: 5}, {Index: 4, Score: 7}}
	got := capBySeverity(in, 2)
	if len(got) != 2 || got[0].Index != 2 || got[1].Index != 4 {
		t.Errorf("cap 2 = %+v", got)
	}
	if got := capBySeverity(in, 0); len(got) != 4 {
		t.Errorf("cap 0 = %+v", got)
	}
	if got := capBySeverity(in, 10); len(got) != 4 {
		t.Errorf("cap 10 = %+v", got)
	}
}

// steady is a memory-like dimension: 50% with a thousandth of jitter.
func steady(n int, overrides map[int]float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 50 + 0.001*float64(i%2*2-1)
		if o, ok := overrides[i]; ok {
			out[i] = o
		}
	}
	return out
}

func TestMahalanobisFloor(t *testing.T) {
	cpu, _ := correlated(40)
	// A +0.05 blip is six standard deviations of a dimension this steady.
	blip := multi(nil, cpuMem, cpu, steady(40, map[int]float64{10: 50.05}))
	v, err := (mahalanobis{}).Detect(map[string]any{}, blip, nil)
	if err != nil || len(v.Anomalies) != 1 || v.Anomalies[0].Index != 10 {
		t.Fatalf("without a floor the blip is an anomaly: %+v %v", v.Anomalies, err)
	}
	// With a floor of 0.75 (min_delta 3) the blip is 0.07 of the assumed spread.
	floor := []float64{0, 0.75}
	if v, err = (mahalanobis{}).Detect(map[string]any{}, blip, floor); err != nil || len(v.Anomalies) != 0 {
		t.Fatalf("with a floor the blip must not be an anomaly: %+v %v", v.Anomalies, err)
	}
	if got := v.Stats["stddev_mem"]; got > 0.01 {
		t.Errorf("stddev_mem = %v: stats must describe the data, not the floor", got)
	}
	// A real move of 10 points stays an anomaly; its deviation is in units of
	// sqrt(stddev² + floor²).
	move := multi(nil, cpuMem, cpu, steady(40, map[int]float64{30: 60}))
	v, err = (mahalanobis{}).Detect(map[string]any{}, move, floor)
	if err != nil || len(v.Anomalies) != 1 || v.Anomalies[0].Index != 30 {
		t.Fatalf("the move must stay an anomaly: %+v %v", v.Anomalies, err)
	}
	sd := v.Stats["stddev_mem"]
	want := (60 - v.Stats["mean_mem"]) / math.Sqrt(sd*sd+0.75*0.75)
	if got := v.Anomalies[0].Deviation[1]; math.Abs(got-want) > 1e-9 {
		t.Errorf("deviation = %v, want %v", got, want)
	}
	// A constant dimension is singular on its own and scorable with a floor.
	constant := multi(nil, cpuMem, cpu, flat(40, 50))
	if v, _ = (mahalanobis{}).Detect(map[string]any{}, constant, nil); !strings.Contains(v.Skip, "with.min_delta gives every dimension a noise floor") {
		t.Errorf("skip without a floor = %q", v.Skip)
	}
	if v, err = (mahalanobis{}).Detect(map[string]any{}, constant, floor); err != nil || v.Skip != "" {
		t.Errorf("with a floor the constant dimension is scorable: skip %q, err %v", v.Skip, err)
	}
}

// flat returns n copies of v.
func flat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestRunMinDelta(t *testing.T) {
	cpu, _ := correlated(40)
	data := form(cpuMem,
		multi(vm("vm-1"), cpuMem, cpu, steady(40, map[int]float64{10: 50.05})),
		multi(vm("vm-2"), cpuMem, cpu, flat(40, 50)))
	plain := run(t, withM(nil), data)
	if plain["total_anomalies"] != 1 || plain["series_skipped"] != 1 {
		t.Fatalf("without min_delta: the blip is an anomaly and the constant series is skipped: %v", plain)
	}
	out := run(t, withM(map[string]any{"min_delta": map[string]any{"mem": "3"}}), data)
	if out["total_anomalies"] != 0 || out["series_skipped"] != 0 || out["series_analyzed"] != 2 {
		t.Fatalf("with min_delta mem 3: %v", out)
	}
	entry := out["series"].([]any)[0].(map[string]any)
	if nf := entry["noise_floor"].(map[string]any); nf["mem"] != 0.75 || nf["cpu"] != 0.0 {
		t.Errorf("noise_floor = %v, want mem 0.75 and cpu 0", nf)
	}
	// min_rel_delta takes a fraction of the dim's mean: 0.2 * 50 / 4 = 2.5.
	out = run(t, withM(map[string]any{"min_rel_delta": 0.2}), data)
	if nf := out["series"].([]any)[1].(map[string]any)["noise_floor"].(map[string]any); nf["mem"] != 2.5 {
		t.Errorf("relative noise_floor = %v, want mem 2.5", nf)
	}
	if _, has := plain["series"].([]any)[0].(map[string]any)["noise_floor"]; has {
		t.Error("noise_floor must be absent without min_delta")
	}
	_, err := New().Run(context.Background(), withM(map[string]any{"min_delta": map[string]any{"disk": 10}}), data, nil)
	if err == nil || err.Error() != "with.min_delta: unknown dimension(s) disk (dims: cpu, mem)" {
		t.Errorf("unknown dim: %v", err)
	}
}
