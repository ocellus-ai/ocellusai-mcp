package ensemble

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
	"github.com/ocellus-ai/ocellusai-mcp/internal/tsanomaly"
)

const (
	baseTS = 1757600000.0
	stepS  = 15.0
)

// synthMulti mirrors the tsanomaly test data: d correlated metrics driven by
// one seasonal base; metric 2 flips sign on [a, b] (same amplitude, broken
// correlation).
func synthMulti(n, d int, seed int64) (cols [][]float64, a, b int) {
	rng := rand.New(rand.NewSource(seed))
	base := make([]float64, n)
	for t := range base {
		base[t] = math.Sin(2*math.Pi*float64(t)/150) + 0.3*math.Sin(2*math.Pi*float64(t)/37)
	}
	cols = make([][]float64, d)
	for j := range cols {
		cols[j] = make([]float64, n)
		for t := range base {
			cols[j][t] = float64(j+1)*base[t] + rng.NormFloat64()*0.05
		}
	}
	a, b = 900, 920
	for t := a; t <= b; t++ {
		cols[2][t] = -3*base[t] + rng.NormFloat64()*0.05
	}
	return cols, a, b
}

// synthUni is a sine with noise, a spike at 700 and a flattened segment
// 1200..1260 (a shape anomaly whose values stay in range).
func synthUni(seed int64) (x []float64, spike, shapeStart, shapeEnd int) {
	const n = 2000
	rng := rand.New(rand.NewSource(seed))
	x = make([]float64, n)
	for t := range x {
		x[t] = 10 + 3*math.Sin(2*math.Pi*float64(t)/100) + rng.NormFloat64()*0.2
	}
	spike, shapeStart, shapeEnd = 700, 1200, 1260
	x[spike] += 12
	for t := shapeStart; t <= shapeEnd; t++ {
		x[t] = 10 + rng.NormFloat64()*0.2
	}
	return x, spike, shapeStart, shapeEnd
}

// multi builds one multivariate series with one sample every stepS seconds.
func multi(labels map[string]string, dims []string, cols ...[]float64) series.MultiSeries {
	s := series.MultiSeries{Labels: labels, Dims: dims}
	for i := range cols[0] {
		v := make([]float64, len(cols))
		for d := range cols {
			v[d] = cols[d][i]
		}
		s.Points = append(s.Points, series.MultiPoint{T: baseTS + stepS*float64(i), V: v})
	}
	return s
}

// form writes the given series in the multivariate form the processor parses.
func form(dims []string, ss ...series.MultiSeries) map[string]any {
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

func first(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list, _ := out["series"].([]any)
	if len(list) == 0 {
		t.Fatalf("no series in %v", out)
	}
	return list[0].(map[string]any)
}

// overlapping returns the ranges that cover any grid index in [a, b].
func overlapping(ranges []any, a, b int) []map[string]any {
	var out []map[string]any
	for _, r := range ranges {
		m := r.(map[string]any)
		start := int(math.Round((m["start"].(float64) - baseTS) / stepS))
		end := int(math.Round((m["end"].(float64) - baseTS) / stepS))
		if end >= a && start <= b {
			out = append(out, m)
		}
	}
	return out
}

func TestName(t *testing.T) {
	if New().Name() != "anomaly_ensemble" {
		t.Errorf("Name() = %q", New().Name())
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with map[string]any
		err  string
	}{
		{"empty", map[string]any{}, ""},
		{"all keys", map[string]any{
			"window": "1h", "season": 5760, "step": "15s", "detectors": []any{"mp", "ar"},
			"risk": 1e-3, "z_threshold": 0, "z_floor": 0, "min_len": 2, "merge_gap": 3,
			"ar_lags": []any{1, 2, 3, 96}, "kmp_dims": 0, "knn": 1, "pca_train_fraction": 0.5,
			"min_points": 10, "max_ranges": 0, "top_metrics": 3,
		}, ""},
		{"numbers as strings", map[string]any{"window": "240", "risk": "0.001", "min_len": "2", "ar_lags": "1,2,3", "detectors": "mp,pca"}, ""},
		{"templates deferred", map[string]any{"window": "{{ .w }}", "detectors": []any{"{{ .d }}"}, "ar_lags": "{{ .l }}", "risk": "{{ .r }}", "step": "{{ .s }}"}, ""},
		{"unknown key", map[string]any{"threshold": 3}, "with: unknown field(s) threshold (allowed: window, season, step, detectors"},
		{"window text", map[string]any{"window": "soon"}, `with.window: expected a number of points or a duration such as 1h, got "soon"`},
		{"window negative", map[string]any{"window": -1}, "with.window: must be >= 0 points or a duration"},
		{"window fraction", map[string]any{"window": 1.5}, "with.window: must be an integer"},
		{"season text", map[string]any{"season": "daily"}, `with.season: expected a number of points or a duration`},
		{"step zero", map[string]any{"step": 0}, "with.step: must be > 0 seconds"},
		{"step text", map[string]any{"step": "fast"}, "with.step: expected seconds or a duration such as 30s"},
		{"detectors unknown", map[string]any{"detectors": []any{"lof"}}, `with.detectors[0]: unknown detector "lof" (available: mp, ar, level, seasonal, iforest, pca, kmp)`},
		{"detectors duplicate", map[string]any{"detectors": "mp,mp"}, `with.detectors: duplicate "mp"`},
		{"detectors number", map[string]any{"detectors": []any{1}}, "with.detectors[0]: must be a string, got number"},
		{"detectors object", map[string]any{"detectors": map[string]any{}}, "with.detectors: must be a list, got object"},
		{"risk one", map[string]any{"risk": 1}, "with.risk: must be in (0, 1), got 1"},
		{"risk text", map[string]any{"risk": "low"}, `with.risk: "low" is not a number`},
		{"z_threshold negative", map[string]any{"z_threshold": -1}, "with.z_threshold: must be >= 0"},
		{"z_floor negative", map[string]any{"z_floor": -1}, "with.z_floor: must be >= 0"},
		{"min_len zero", map[string]any{"min_len": 0}, "with.min_len: must be >= 1"},
		{"min_len zero duration", map[string]any{"min_len": "0s"}, "with.min_len: must be >= 1 point or a duration such as 5m, got 0s"},
		{"min_len text", map[string]any{"min_len": "soon"}, "with.min_len: expected a number of points or a duration"},
		{"min_len fraction", map[string]any{"min_len": 1.5}, "with.min_len: must be an integer"},
		{"lengths as durations", map[string]any{"min_len": "5m", "merge_gap": "2m"}, ""},
		{"merge_gap negative", map[string]any{"merge_gap": -1}, "with.merge_gap: must be >= 0"},
		{"merge_gap text", map[string]any{"merge_gap": "later"}, "with.merge_gap: expected a number of points or a duration"},
		{"ar_lags zero", map[string]any{"ar_lags": []any{0}}, "with.ar_lags[0]: must be a positive integer, got 0"},
		{"ar_lags duplicate", map[string]any{"ar_lags": "1,1"}, "with.ar_lags: duplicate 1"},
		{"ar_lags text", map[string]any{"ar_lags": []any{"one"}}, `with.ar_lags[0]: "one" is not a number`},
		{"ar_lags empty", map[string]any{"ar_lags": []any{}}, "with.ar_lags: must not be empty"},
		{"kmp_dims", map[string]any{"kmp_dims": -2}, "with.kmp_dims: must be -1 (worst dim), 0 (all dims)"},
		{"knn zero", map[string]any{"knn": 0}, "with.knn: must be >= 1"},
		{"pca_train_fraction", map[string]any{"pca_train_fraction": 1.5}, "with.pca_train_fraction: must be in (0, 1]"},
		{"min_points zero", map[string]any{"min_points": 0}, "with.min_points: must be >= 1"},
		{"max_ranges negative", map[string]any{"max_ranges": -1}, "with.max_ranges: must be >= 0"},
		{"top_metrics zero", map[string]any{"top_metrics": 0}, "with.top_metrics: must be >= 1"},
		{"min_delta number", map[string]any{"min_delta": 1, "min_rel_delta": 0.2}, ""},
		{"min_delta per dim", map[string]any{"min_delta": map[string]any{"cpu": 5, "steal": "{{ .s }}"}, "min_rel_delta": "{{ .r }}"}, ""},
		{"min_delta negative", map[string]any{"min_delta": map[string]any{"cpu": -5}}, "with.min_delta.cpu: must be >= 0"},
		{"min_delta list", map[string]any{"min_delta": []any{5}}, "with.min_delta: must be a number or an object of numbers per dimension"},
		{"min_rel_delta negative", map[string]any{"min_rel_delta": -1}, "with.min_rel_delta: must be >= 0 (0 = off)"},
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
	x, _, _, _ := synthUni(1)
	data := form([]string{"x"}, multi(vm("a"), []string{"x"}, x))
	for _, with := range []map[string]any{
		{"window": "{{ .w }}"},
		{"detectors": []any{"{{ .d }}"}},
		{"ar_lags": "{{ .l }}"},
		{"risk": "{{ .r }}"},
		{"step": "{{ .s }}"},
	} {
		if _, err := New().Run(context.Background(), with, data, nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
			t.Errorf("with %v: error = %v, want unrendered template", with, err)
		}
	}
}

func TestRunMultivariate(t *testing.T) {
	cols, a, b := synthMulti(1500, 4, 5)
	dims := []string{"m0", "m1", "m2", "m3"}
	data := form(dims, multi(vm("vm-1"), dims, cols...))
	out := run(t, map[string]any{"window": 50, "detectors": []any{"mp", "ar", "pca", "kmp"}}, data)

	if got := out["dims"]; !reflect.DeepEqual(got, []any{"m0", "m1", "m2", "m3"}) {
		t.Errorf("dims = %v", got)
	}
	if got := out["detectors"]; !reflect.DeepEqual(got, []any{"mp", "ar", "pca", "kmp"}) {
		t.Errorf("detectors = %v", got)
	}
	if thr := out["threshold"].(map[string]any); thr["mode"] != "pot" || thr["risk"] != 1e-4 || thr["z_floor"] != 4.0 {
		t.Errorf("threshold = %v", thr)
	}
	if out["series_total"] != 1 || out["series_analyzed"] != 1 || out["series_skipped"] != 0 {
		t.Errorf("counts = %v/%v/%v", out["series_total"], out["series_analyzed"], out["series_skipped"])
	}
	s := first(t, out)
	if s["skipped"] != false || s["points"] != 1500 || s["grid_points"] != 1500 || s["gaps"] != 0 || s["step"] != stepS || s["window"] != 50 || s["season"] != 0 {
		t.Errorf("series = points %v grid %v gaps %v step %v window %v season %v", s["points"], s["grid_points"], s["gaps"], s["step"], s["window"], s["season"])
	}
	if _, has := s["error"]; has {
		t.Errorf("unexpected error: %v", s["error"])
	}
	count := s["anomaly_count"].(int)
	if count < 1 || out["total_anomalies"] != count {
		t.Fatalf("anomaly_count = %d, total = %v", count, out["total_anomalies"])
	}
	hits := overlapping(s["anomalies"].([]any), a, b)
	if len(hits) == 0 {
		t.Fatalf("no multivariate range over [%d, %d]: %v", a, b, s["anomalies"])
	}
	r := hits[0]
	if r["kind"] == "" || r["points"].(int) < 1 || r["duration"].(float64) != float64(r["points"].(int))*stepS {
		t.Errorf("range = %v", r)
	}
	if r["start_time"] != series.FormatTime(r["start"].(float64)) || r["peak_time"] != series.FormatTime(r["peak"].(float64)) {
		t.Errorf("times = %v", r)
	}
	top := r["top_metrics"].([]any)
	if len(top) != 4 {
		t.Fatalf("top_metrics = %v", top)
	}
	lead := top[0].(map[string]any)
	if lead["metric"] != "m2" || lead["score"].(float64) <= 0 || lead["pca_share"].(float64) < 0.5 {
		t.Errorf("top metric = %v, want m2 with a dominant PCA share", lead)
	}
	names := s["detectors"].([]any)
	joined := ""
	for _, n := range names {
		joined += n.(string) + " "
	}
	if !strings.Contains(joined, "pca(") || !strings.Contains(joined, "kmp(") {
		t.Errorf("multivariate detectors = %v", names)
	}
	thr := s["thresholds"].(map[string]any)
	for _, n := range names {
		if _, ok := thr[n.(string)]; !ok {
			t.Errorf("thresholds lack %v: %v", n, thr)
		}
	}
	if s["max_score"].(float64) <= 0 {
		t.Errorf("max_score = %v", s["max_score"])
	}
	if fired := r["fired_by"].([]any); len(fired) == 0 {
		t.Errorf("multivariate range must name the detectors that fired: %v", r)
	}
	if vals := r["values"].(map[string]any); len(vals) != 4 || vals["m2"] == nil {
		t.Errorf("values at peak = %v", vals)
	}
	if base := r["baseline"].(map[string]any); len(base) != 4 || base["m2"] == nil {
		t.Errorf("baseline = %v", base)
	}
	m2 := s["metrics"].(map[string]any)["m2"].(map[string]any)
	m2hits := overlapping(m2["anomalies"].([]any), a, b)
	if m2["anomaly_count"].(int) < 1 || len(m2hits) == 0 {
		t.Fatalf("metric m2 = %v", m2)
	}
	m2r := m2hits[0]
	if _, has := m2r["kind"]; has {
		t.Errorf("univariate range must not carry kind: %v", m2r)
	}
	if _, has := m2r["values"]; has {
		t.Errorf("univariate range carries a scalar value, not values: %v", m2r)
	}
	if _, ok := m2r["value"].(float64); !ok {
		t.Errorf("value at peak = %v", m2r["value"])
	}
	if _, ok := m2r["baseline"].(float64); !ok {
		t.Errorf("baseline = %v", m2r["baseline"])
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("report must encode: %v", err)
	}
}

func TestRunUnivariate(t *testing.T) {
	x, spike, ss, se := synthUni(4)
	data := form([]string{"x"}, multi(vm("a"), []string{"x"}, x))
	with := map[string]any{"window": 100, "detectors": "mp,ar", "merge_gap": 25}
	out := run(t, with, data)
	s := first(t, out)
	anomalies := s["anomalies"].([]any)
	if len(overlapping(anomalies, spike, spike)) == 0 || len(overlapping(anomalies, ss, se)) == 0 {
		t.Fatalf("spike or shape anomaly missed: %v", anomalies)
	}
	sp := overlapping(anomalies, spike, spike)[0]
	if v, _ := sp["value"].(float64); v < 15 { // 10 + 12 spike, sine at zero phase
		t.Errorf("value at the spike = %v, want > 15", sp["value"])
	}
	if b, _ := sp["baseline"].(float64); b < 9 || b > 11 {
		t.Errorf("baseline = %v, want the median of the sine ≈ 10", sp["baseline"])
	}
	// the series carries the same baseline per dim and the medians of its quarters
	if b, _ := s["baseline"].(map[string]any)["x"].(float64); b < 9 || b > 11 {
		t.Errorf("series baseline = %v, want ≈ 10", s["baseline"])
	}
	if qs, _ := s["quarters"].(map[string]any)["x"].([]any); len(qs) != 4 {
		t.Errorf("series quarters = %v, want four medians", s["quarters"])
	} else if q, _ := qs[3].(float64); q < 9 || q > 11 {
		t.Errorf("last quarter median = %v, want ≈ 10", qs[3])
	}
	firedAR := false
	for _, d := range sp["fired_by"].([]any) {
		firedAR = firedAR || strings.HasPrefix(d.(string), "ar(")
	}
	if !firedAR {
		t.Errorf("the spike must be caught by the AR residual: fired_by = %v", sp["fired_by"])
	}
	for _, r := range anomalies {
		if _, has := r.(map[string]any)["kind"]; has {
			t.Errorf("single-dim headline must not carry kind: %v", r)
		}
	}
	x1 := s["metrics"].(map[string]any)["x"].(map[string]any)
	if s["anomaly_count"] != x1["anomaly_count"] || !reflect.DeepEqual(s["thresholds"], x1["thresholds"]) {
		t.Errorf("headline %v/%v != metric %v/%v", s["anomaly_count"], s["thresholds"], x1["anomaly_count"], x1["thresholds"])
	}
	count := s["anomaly_count"].(int)
	if count < 2 {
		t.Fatalf("anomaly_count = %d", count)
	}

	with["max_ranges"] = 1
	capped := first(t, run(t, with, data))
	if got := capped["anomalies"].([]any); len(got) != 1 || capped["anomaly_count"] != count {
		t.Errorf("max_ranges 1: %d ranges, count %v (want 1, %d)", len(got), capped["anomaly_count"], count)
	}
}

func TestRunSkips(t *testing.T) {
	dims := []string{"x"}
	short := make([]float64, 10)
	irregular := series.MultiSeries{Labels: vm("irregular"), Dims: dims}
	for i := 0; i < 30; i++ {
		irregular.Points = append(irregular.Points, series.MultiPoint{T: baseTS + stepS*float64(i*i), V: []float64{1}})
	}
	same := series.MultiSeries{Labels: vm("same"), Dims: dims}
	for i := 0; i < 25; i++ {
		same.Points = append(same.Points, series.MultiPoint{T: baseTS, V: []float64{1}})
	}
	data := form(dims,
		series.MultiSeries{Labels: vm("unjoined"), Dims: dims, Reason: "missing in input(s) mem"},
		multi(vm("short"), dims, short),
		irregular,
		same,
	)
	out := run(t, nil, data)
	if out["series_total"] != 4 || out["series_skipped"] != 4 || out["series_analyzed"] != 0 || out["total_anomalies"] != 0 {
		t.Errorf("counts = %v", out)
	}
	want := map[string]string{
		"unjoined":  "missing in input(s) mem",
		"short":     "fewer than 20 points (min_points)",
		"irregular": "samples are not on a regular grid: a step of 15s (the smallest interval) needs 842 slots for 30 samples",
		"same":      "cannot infer the step: all samples share one timestamp",
	}
	for _, item := range out["series"].([]any) {
		s := item.(map[string]any)
		name := s["labels"].(map[string]any)["instance"].(string)
		if s["skipped"] != true || !strings.Contains(s["reason"].(string), want[name]) {
			t.Errorf("%s: skipped %v reason %q, want %q", name, s["skipped"], s["reason"], want[name])
		}
		if s["anomaly_count"] != 0 || len(s["anomalies"].([]any)) != 0 {
			t.Errorf("%s: anomalies = %v", name, s["anomalies"])
		}
	}
}

func TestRunGaps(t *testing.T) {
	x, _, _, _ := synthUni(11)
	dims := []string{"x"}
	s := multi(vm("a"), dims, x)
	s.Points = append(s.Points[:500:500], s.Points[561:]...) // drop 61 samples
	out := run(t, map[string]any{"window": 100, "detectors": "mp,ar,iforest,level", "merge_gap": 10}, form(dims, s))
	e := first(t, out)
	if e["points"] != 1939 || e["grid_points"] != 2000 || e["gaps"] != 61 {
		t.Errorf("points %v grid %v gaps %v", e["points"], e["grid_points"], e["gaps"])
	}
	if hits := overlapping(e["anomalies"].([]any), 480, 580); len(hits) != 0 {
		t.Errorf("gap flagged as anomaly: %v", hits)
	}
}

func TestRunWindowSeasonStep(t *testing.T) {
	x, _, _, _ := synthUni(2)
	dims := []string{"x"}
	data := form(dims, multi(vm("a"), dims, x))

	s := first(t, run(t, map[string]any{"window": "5m", "detectors": "ar"}, data))
	if s["window"] != 20 {
		t.Errorf("window 5m at 15s = %v, want 20", s["window"])
	}
	s = first(t, run(t, map[string]any{"detectors": "ar"}, data))
	if s["window"] != 40 { // max(8, n/50)
		t.Errorf("auto window = %v, want 40", s["window"])
	}
	if _, err := New().Run(context.Background(), map[string]any{"window": "30s"}, data, nil); err == nil || !strings.Contains(err.Error(), "with.window: must be at least 4 points, got 2") {
		t.Errorf("window 30s: error = %v", err)
	}

	out := run(t, map[string]any{"season": "1h", "detectors": "ar,seasonal"}, data)
	if got := out["detectors"]; !reflect.DeepEqual(got, []any{"ar", "seasonal"}) {
		t.Errorf("detectors = %v", got)
	}
	s = first(t, out)
	if s["season"] != 240 || !reflect.DeepEqual(s["detectors"], []any{"ar(lags=[1 2 3])", "seasonal(S=240)"}) {
		t.Errorf("season = %v, detectors = %v", s["season"], s["detectors"])
	}
	// no season: seasonal is neither listed nor run
	out = run(t, map[string]any{"detectors": "ar,seasonal"}, data)
	if got := out["detectors"]; !reflect.DeepEqual(got, []any{"ar"}) {
		t.Errorf("detectors without season = %v", got)
	}
	if got := first(t, out)["detectors"]; !reflect.DeepEqual(got, []any{"ar(lags=[1 2 3])"}) {
		t.Errorf("series detectors without season = %v", got)
	}

	s = first(t, run(t, map[string]any{"step": "30s", "detectors": "ar"}, data))
	if s["step"] != 30.0 || s["grid_points"] != 1001 {
		t.Errorf("explicit step: step %v grid %v (want 30, 1001)", s["step"], s["grid_points"])
	}

	// min_len and merge_gap are points or durations, resolved by the step
	s = first(t, run(t, map[string]any{"window": 100, "min_len": "5m", "merge_gap": "1m", "detectors": "ar"}, data))
	if s["min_len"] != 20 || s["merge_gap"] != 4 {
		t.Errorf("min_len 5m / merge_gap 1m at 15s = %v / %v, want 20 / 4", s["min_len"], s["merge_gap"])
	}
	s = first(t, run(t, map[string]any{"window": 100, "min_len": 3, "detectors": "ar"}, data))
	if s["min_len"] != 3 || s["merge_gap"] != 6 { // default merge_gap = window/16
		t.Errorf("min_len 3 / default merge_gap at window 100 = %v / %v, want 3 / 6", s["min_len"], s["merge_gap"])
	}
	s = first(t, run(t, map[string]any{"window": 100, "min_len": "1s", "detectors": "ar"}, data))
	if s["min_len"] != 1 { // shorter than a step still means one point
		t.Errorf("min_len 1s at 15s = %v, want 1", s["min_len"])
	}
}

// TestRunMinLenDuration checks the semantics of a min_len given as a
// duration: a segment shorter than it is dropped from the report.
func TestRunMinLenDuration(t *testing.T) {
	x, spike, _, _ := synthUni(4)
	data := form([]string{"x"}, multi(vm("a"), []string{"x"}, x))
	with := map[string]any{"window": 100, "detectors": "ar"}
	s := first(t, run(t, with, data))
	hits := overlapping(s["anomalies"].([]any), spike, spike)
	if len(hits) != 1 {
		t.Fatalf("spike not found by ar: %v", s["anomalies"])
	}
	if pts := hits[0]["points"].(int); pts >= 20 {
		t.Fatalf("the spike segment spans %d points, the test needs it shorter than 5m", pts)
	}
	with["min_len"] = "5m" // 20 points at a 15s step
	s = first(t, run(t, with, data))
	if got := overlapping(s["anomalies"].([]any), spike, spike); len(got) != 0 {
		t.Errorf("min_len 5m kept the spike segment: %v", got)
	}
}

func TestRunFixedThreshold(t *testing.T) {
	x, spike, _, _ := synthUni(3)
	dims := []string{"x"}
	out := run(t, map[string]any{"z_threshold": 6, "detectors": "ar", "ar_lags": "1,2"}, form(dims, multi(vm("a"), dims, x)))
	if thr := out["threshold"].(map[string]any); thr["mode"] != "fixed" || thr["z"] != 6.0 {
		t.Errorf("threshold = %v", thr)
	}
	s := first(t, out)
	if got := s["thresholds"]; !reflect.DeepEqual(got, map[string]any{"ar(lags=[1 2])": 6.0}) {
		t.Errorf("thresholds = %v", got)
	}
	if len(overlapping(s["anomalies"].([]any), spike, spike)) == 0 {
		t.Errorf("spike missed: %v", s["anomalies"])
	}
}

func TestRunErrors(t *testing.T) {
	for _, tc := range []struct {
		data any
		err  string
	}{
		{[]any{}, "expected a multivariate series object {dims: [...], series: [...]} (the output of fn: join), got array"},
		{map[string]any{"dims": []any{}, "series": []any{}}, "dims: expected a non-empty list of dimension names"},
		{map[string]any{"resultType": "matrix", "result": []any{}}, "dims: expected a non-empty list of dimension names, got null"},
	} {
		if _, err := New().Run(context.Background(), nil, tc.data, nil); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("data %v: error = %v, want %q", tc.data, err, tc.err)
		}
	}
}

func TestRunEmpty(t *testing.T) {
	out := run(t, nil, form([]string{"x"}))
	if out["series_total"] != 0 || out["total_anomalies"] != 0 || len(out["series"].([]any)) != 0 {
		t.Errorf("empty = %v", out)
	}
}

func TestRunContextCancel(t *testing.T) {
	x, _, _, _ := synthUni(9)
	dims := []string{"x"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(ctx, nil, form(dims, multi(vm("a"), dims, x)), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestAlignSeries(t *testing.T) {
	dims := []string{"a", "b"}
	s := series.MultiSeries{Labels: vm("x"), Dims: dims}
	for _, i := range []int{0, 1, 3, 4} { // slot 2 missing
		s.Points = append(s.Points, series.MultiPoint{T: baseTS + 0.25 + 15*float64(i), V: []float64{float64(i), -float64(i)}})
	}
	g, reason := alignSeries(s, 0)
	if reason != "" {
		t.Fatal(reason)
	}
	if g.step != 15 || g.gaps != 1 || g.frame.Len() != 5 || g.times[2] != baseTS+0.25+30 || g.frame.Timestamps[2] != int64(baseTS)+30 {
		t.Errorf("grid = step %v gaps %d len %d times[2] %v ts[2] %v", g.step, g.gaps, g.frame.Len(), g.times[2], g.frame.Timestamps[2])
	}
	if !math.IsNaN(g.frame.Cols[0][2]) || !math.IsNaN(g.frame.Cols[1][2]) || g.frame.Cols[0][3] != 3 || g.frame.Cols[1][3] != -3 {
		t.Errorf("cols = %v", g.frame.Cols)
	}
	if !reflect.DeepEqual(g.frame.Names, dims) {
		t.Errorf("names = %v", g.frame.Names)
	}
	// quarters by grid index: {0,1} {2} {3} {4}; the empty one is NaN
	if q := g.quarters[0]; len(q) != 4 || q[0] != 0.5 || !math.IsNaN(q[1]) || q[2] != 3 || q[3] != 4 {
		t.Errorf("quarters = %v, want [0.5 NaN 3 4]", g.quarters[0])
	}
	if q := g.quartersAny()["a"].([]any); q[1] != nil || q[3] != 4.0 {
		t.Errorf("quartersAny = %v", q)
	}
	// a level step in the second half shows in the last two quarters
	stepped := series.MultiSeries{Dims: []string{"a"}}
	for i := range 8 {
		v := 0.0
		if i >= 4 {
			v = 10
		}
		stepped.Points = append(stepped.Points, series.MultiPoint{T: baseTS + 15*float64(i), V: []float64{v}})
	}
	if g, reason := alignSeries(stepped, 0); reason != "" || !reflect.DeepEqual(g.quarters[0], []float64{0, 0, 10, 10}) || g.medians[0] != 5 {
		t.Errorf("stepped: reason %q quarters %v median %v", reason, g.quarters[0], g.medians[0])
	}
	// an explicit step wider than the data merges samples into slots
	g, reason = alignSeries(s, 30)
	if reason != "" || g.frame.Len() != 3 || g.gaps != 0 {
		t.Errorf("step 30: reason %q len %d gaps %d", reason, g.frame.Len(), g.gaps)
	}
	// samples arriving out of order are sorted first
	rev := series.MultiSeries{Dims: dims}
	for i := 3; i >= 0; i-- {
		rev.Points = append(rev.Points, series.MultiPoint{T: baseTS + 15*float64(i), V: []float64{float64(i), 0}})
	}
	if g, reason = alignSeries(rev, 0); reason != "" || g.frame.Cols[0][3] != 3 || g.frame.Cols[0][0] != 0 {
		t.Errorf("reversed: reason %q cols %v", reason, g.frame.Cols)
	}
}

func TestCapRanges(t *testing.T) {
	rs := []tsanomaly.Range{
		{Start: 0, End: 2, Peak: 1, PeakScore: 5},
		{Start: 10, End: 12, Peak: 11, PeakScore: 9},
		{Start: 20, End: 21, Peak: 20, PeakScore: 7},
	}
	got := capRanges(rs, nil, 2)
	if len(got) != 2 || got[0].r.Start != 10 || got[1].r.Start != 20 || got[0].attr != nil {
		t.Errorf("cap 2 = %+v", got)
	}
	if got := capRanges(rs, nil, 0); len(got) != 3 || got[0].r.Start != 0 {
		t.Errorf("cap 0 = %+v", got)
	}
	attrs := []tsanomaly.RangeAttribution{{Kind: "a"}, {Kind: "b"}, {Kind: "c"}}
	if got := capRanges(rs, attrs, 2); got[0].attr.Kind != "b" || got[1].attr.Kind != "c" {
		t.Errorf("attribution must follow its range: %+v", got)
	}
}

func TestDetectorListAndThresholdInfo(t *testing.T) {
	st, err := parseSettings(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.detectorList(); !reflect.DeepEqual(got, []string{"mp", "ar", "level", "iforest", "pca", "kmp"}) {
		t.Errorf("default detectors = %v", got)
	}
	st.season = processor.Span{Seconds: 3600}
	if got := st.detectorList(); !reflect.DeepEqual(got, []string{"mp", "ar", "level", "seasonal", "iforest", "pca", "kmp"}) {
		t.Errorf("with season = %v", got)
	}
	st.detectors = map[string]bool{"pca": true, "seasonal": true}
	if got := st.detectorList(); !reflect.DeepEqual(got, []string{"seasonal", "pca"}) {
		t.Errorf("subset = %v", got)
	}
	if got := st.thresholdInfo(); !reflect.DeepEqual(got, map[string]any{"mode": "pot", "risk": 1e-4, "z_floor": 4.0}) {
		t.Errorf("pot info = %v", got)
	}
	st.zThreshold = 6
	if got := st.thresholdInfo(); !reflect.DeepEqual(got, map[string]any{"mode": "fixed", "z": 6.0}) {
		t.Errorf("fixed info = %v", got)
	}
	st.zFloor = 0
	if st.floor() != -1 {
		t.Errorf("floor 0 must map to -1 (disabled), got %v", st.floor())
	}
}

// TestRunLevelShift is a case from a production fleet: a level reached in
// four samples and held for thirty (+20 sigma, like cpu 99.9% against a
// usual 8) is missed by the AR residual and found by the level detector.
func TestRunLevelShift(t *testing.T) {
	const n = 1440
	rng := rand.New(rand.NewSource(9))
	x := make([]float64, n)
	for i := range x {
		x[i] = 10 + rng.NormFloat64()
	}
	start, end := 700, 729
	for k := 1; k <= 4; k++ {
		x[start-5+k] = 10 + 20*float64(k)/5
	}
	for i := start; i <= end; i++ {
		x[i] = 30 + rng.NormFloat64()
	}
	dims := []string{"cpu"}
	data := form(dims, multi(vm("a"), dims, x))

	s := first(t, run(t, map[string]any{"window": 60, "detectors": "level"}, data))
	hits := overlapping(s["anomalies"].([]any), start, end)
	if len(hits) != 1 {
		t.Fatalf("level: %d ranges over the plateau, want 1: %v", len(hits), s["anomalies"])
	}
	r := hits[0]
	if pts := r["points"].(int); pts < 27 {
		t.Errorf("level range covers %d of 30 plateau points", pts)
	}
	if fired := r["fired_by"].([]any); len(fired) != 1 || fired[0] != "level(w=60)" {
		t.Errorf("fired_by = %v", fired)
	}
	if v, _ := r["value"].(float64); v < 25 {
		t.Errorf("value at the peak = %v, want the plateau level", r["value"])
	}
	if th, _ := s["thresholds"].(map[string]any)["level(w=60)"].(float64); th < 4 || th > 13 { // cap ≈ 12.4 at risk 1e-4
		t.Errorf("level threshold = %v, want a POT threshold between the floor and the cap", th)
	}
	// the AR residual sees the ramp at most
	s = first(t, run(t, map[string]any{"window": 60, "detectors": "ar"}, data))
	for _, h := range overlapping(s["anomalies"].([]any), start+6, end) {
		t.Errorf("ar flagged the plateau interior: %v", h)
	}
	// with the default detectors the plateau is still found
	s = first(t, run(t, map[string]any{"window": 60}, data))
	if hits := overlapping(s["anomalies"].([]any), start+6, end); len(hits) == 0 {
		t.Errorf("default ensemble missed the plateau: %v", s["anomalies"])
	}
}

// busyAndIdle is a busy cpu (30% ± a daily-like wave) next to an idle steal
// (0.001% with jitter a tenth of that) that has two negligible blips of
// +0.05 at 200 and 400 and a real jump to 20% on [500, 510].
func busyAndIdle() (cpu, steal []float64) {
	rng := rand.New(rand.NewSource(5))
	const n = 720
	cpu, steal = make([]float64, n), make([]float64, n)
	for i := range cpu {
		cpu[i] = 30 + 5*math.Sin(2*math.Pi*float64(i)/240) + rng.NormFloat64()*0.5
		steal[i] = 0.001 + rng.NormFloat64()*0.0001
	}
	steal[200] += 0.05
	steal[400] += 0.05
	for i := 500; i <= 510; i++ {
		steal[i] = 20 + rng.NormFloat64()*0.5
	}
	return cpu, steal
}

func TestRunMinDelta(t *testing.T) {
	dims := []string{"cpu", "steal"}
	cpu, steal := busyAndIdle()
	data := form(dims, multi(vm("vm-1"), dims, cpu, steal))
	stealRanges := func(out map[string]any) []any {
		return first(t, out)["metrics"].(map[string]any)["steal"].(map[string]any)["anomalies"].([]any)
	}

	plain := run(t, map[string]any{"window": 60}, data)
	if blips := append(overlapping(stealRanges(plain), 199, 201), overlapping(stealRanges(plain), 399, 401)...); len(blips) == 0 {
		t.Fatalf("without min_delta the +0.05 blips of an idle signal are flagged (the problem min_delta solves): %v", stealRanges(plain))
	}
	if _, has := first(t, plain)["noise_floor"]; has {
		t.Error("noise_floor must be absent without min_delta")
	}

	with := map[string]any{"window": 60, "min_delta": map[string]any{"cpu": 5, "steal": "1"}}
	out := run(t, with, data)
	entry := first(t, out)
	if nf := entry["noise_floor"].(map[string]any); nf["cpu"] != 1.25 || nf["steal"] != 0.25 {
		t.Errorf("noise_floor = %v, want cpu 1.25 and steal 0.25", nf)
	}
	ranges := stealRanges(out)
	if blips := append(overlapping(ranges, 199, 201), overlapping(ranges, 399, 401)...); len(blips) != 0 {
		t.Errorf("with min_delta the blips must not be flagged: %v", blips)
	}
	jump := overlapping(ranges, 500, 510)
	if len(jump) == 0 {
		t.Fatalf("the jump to 20%% must stay flagged: %v", ranges)
	}
	// The report is in raw units: the value at the peak is one of the input
	// samples and the baseline is the median of the input, not of the noise.
	peak := int(math.Round((jump[0]["peak"].(float64) - baseTS) / stepS))
	if jump[0]["value"] != steal[peak] {
		t.Errorf("value at the peak = %v, want the raw sample %v", jump[0]["value"], steal[peak])
	}
	if !reflect.DeepEqual(entry["baseline"], first(t, plain)["baseline"]) || !reflect.DeepEqual(entry["quarters"], first(t, plain)["quarters"]) {
		t.Errorf("baseline/quarters must not see the noise: %v vs %v", entry["baseline"], first(t, plain)["baseline"])
	}
	// The noise is seeded by the labels and the dim: the same input gives the same report.
	again := run(t, with, data)
	if a, b := mustJSON(t, out), mustJSON(t, again); a != b {
		t.Error("two runs over the same data differ")
	}

	// One number applies to every dim; min_rel_delta takes a fraction of the median.
	out = run(t, map[string]any{"window": 60, "min_delta": 1, "min_rel_delta": 0.2}, data)
	nf := first(t, out)["noise_floor"].(map[string]any)
	median := first(t, out)["baseline"].(map[string]any)["cpu"].(float64)
	if nf["steal"] != 0.25 || math.Abs(nf["cpu"].(float64)-0.25*0.2*median) > 1e-12 {
		t.Errorf("noise_floor = %v (cpu median %v)", nf, median)
	}

	_, err := New().Run(context.Background(), map[string]any{"min_delta": map[string]any{"iowait": 1}}, data, nil)
	if err == nil || err.Error() != "with.min_delta: unknown dimension(s) iowait (dims: cpu, steal)" {
		t.Errorf("unknown dim: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDithered(t *testing.T) {
	s := multi(vm("vm-1"), []string{"a", "b"}, []float64{1, 2, 3, 4, 5, 6}, []float64{10, 10, 10, 10, 10, 10})
	s.Points = append(s.Points[:2], s.Points[3:]...) // a gap at slot 2
	g, reason := alignSeries(s, 0)
	if reason != "" {
		t.Fatal(reason)
	}
	f := g.dithered([]float64{0.5, 0}, s.Labels)
	if !math.IsNaN(f.Cols[0][2]) || !math.IsNaN(f.Cols[1][2]) {
		t.Errorf("a gap must stay a gap: %v %v", f.Cols[0], f.Cols[1])
	}
	if !reflect.DeepEqual(f.Cols[1], g.frame.Cols[1]) {
		t.Errorf("a dim without a floor is unchanged: %v", f.Cols[1])
	}
	changed := false
	for i, x := range f.Cols[0] {
		if i != 2 && x != g.frame.Cols[0][i] {
			changed = true
		}
	}
	if !changed {
		t.Error("a dim with a floor gets noise")
	}
	if g.frame.Cols[0][0] != 1 {
		t.Error("the grid itself must not be modified")
	}
	again := g.dithered([]float64{0.5, 0}, map[string]string{"instance": "vm-1"})
	other := g.dithered([]float64{0.5, 0}, map[string]string{"instance": "vm-2"})
	if !reflect.DeepEqual(mustJSON(t, finite(f.Cols[0])), mustJSON(t, finite(again.Cols[0]))) {
		t.Error("the same labels must give the same noise")
	}
	if reflect.DeepEqual(finite(f.Cols[0]), finite(other.Cols[0])) {
		t.Error("other labels must give other noise")
	}
}

// finite drops NaN so that slices with gaps can be compared.
func finite(v []float64) []float64 {
	var out []float64
	for _, x := range v {
		if !math.IsNaN(x) {
			out = append(out, x)
		}
	}
	return out
}
