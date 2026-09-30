package anomaly

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

const baseTS = 1757600000.0

// row builds a Prometheus matrix entry with one sample per minute.
func row(labels map[string]any, vals ...float64) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		values = append(values, []any{baseTS + float64(60*i), formatFloat(v)})
	}
	return map[string]any{"metric": labels, "values": values}
}

func formatFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func matrix(items ...map[string]any) map[string]any {
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	return map[string]any{"resultType": "matrix", "result": list}
}

// flat returns n copies of v with the given overrides (index → value).
func flat(n int, v float64, overrides map[int]float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
		if o, ok := overrides[i]; ok {
			out[i] = o
		}
	}
	return out
}

func run(t *testing.T, with map[string]any, data any) map[string]any {
	t.Helper()
	out, err := New().Run(context.Background(), with, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func TestDetectorsTable(t *testing.T) {
	if got := Detectors(); !reflect.DeepEqual(got, []string{"iqr", "zscore"}) {
		t.Errorf("Detectors() = %v", got)
	}
	for name, d := range detectors {
		if d.Name() != name {
			t.Errorf("detector %q reports Name() %q", name, d.Name())
		}
		if got, ok := Lookup(name); !ok || got != d {
			t.Errorf("Lookup(%q) = %v, %v", name, got, ok)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup of an unknown method must fail")
	}
}

func TestQuantile(t *testing.T) {
	s := []float64{1, 2, 3, 4}
	for q, want := range map[float64]float64{0: 1, 0.25: 1.75, 0.5: 2.5, 0.75: 3.25, 1: 4} {
		if got := quantile(s, q); math.Abs(got-want) > 1e-9 {
			t.Errorf("quantile(%v) = %v, want %v", q, got, want)
		}
	}
	if quantile([]float64{7}, 0.5) != 7 || quantile(nil, 0.5) != 0 {
		t.Error("degenerate quantiles")
	}
}

func TestZScoreDetect(t *testing.T) {
	d := zscore{}
	pts := flat(30, 1, map[int]float64{17: 10})
	s := Series{Points: toPoints(pts)}
	v, err := d.Detect(map[string]any{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 17 || v.Anomalies[0].Direction != DirectionUp {
		t.Fatalf("anomalies = %+v", v.Anomalies)
	}
	a := v.Anomalies[0]
	if a.Score <= 3 || math.Abs(a.Expected-v.Stats["mean"]) > 1e-9 || v.Stats["threshold"] != 3 || v.Stats["stddev"] <= 0 {
		t.Errorf("anomaly = %+v stats = %v", a, v.Stats)
	}
	// Downward spike, custom threshold from a rendered string.
	v, _ = d.Detect(map[string]any{"threshold": "2"}, Series{Points: toPoints(flat(30, 5, map[int]float64{3: -20}))})
	if len(v.Anomalies) != 1 || v.Anomalies[0].Direction != DirectionDown || v.Stats["threshold"] != 2 {
		t.Errorf("down: %+v %v", v.Anomalies, v.Stats)
	}
	// Constant series: no spread, no anomalies, no NaN.
	v, _ = d.Detect(map[string]any{}, Series{Points: toPoints(flat(10, 2, nil))})
	if len(v.Anomalies) != 0 || v.Stats["stddev"] != 0 || math.IsNaN(v.Stats["mean"]) {
		t.Errorf("constant: %+v %v", v.Anomalies, v.Stats)
	}
	// Threshold too high: nothing.
	v, _ = d.Detect(map[string]any{"threshold": 100}, s)
	if len(v.Anomalies) != 0 {
		t.Errorf("high threshold: %+v", v.Anomalies)
	}
	for name, p := range map[string]map[string]any{
		"zero":    {"threshold": 0},
		"neg":     {"threshold": -1},
		"text":    {"threshold": "big"},
		"unknown": {"thresh": 1},
	} {
		if err := d.Validate(p); err == nil {
			t.Errorf("%s: expected validate error", name)
		}
	}
	if err := d.Validate(map[string]any{"threshold": "{{ .s }}"}); err != nil {
		t.Errorf("template must be accepted at load: %v", err)
	}
}

func TestIQRDetect(t *testing.T) {
	d := iqr{}
	// Regular spread 1..4 repeated, one huge spike: only the spike is outside the fences.
	vals := make([]float64, 0, 41)
	for i := 0; i < 40; i++ {
		vals = append(vals, float64(i%4+1))
	}
	vals = append(vals, 100)
	v, err := d.Detect(map[string]any{}, Series{Points: toPoints(vals)})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 40 || v.Anomalies[0].Direction != DirectionUp {
		t.Fatalf("anomalies = %+v", v.Anomalies)
	}
	if v.Stats["k"] != 1.5 || v.Stats["iqr"] <= 0 || v.Stats["upper"] <= v.Stats["q3"] || v.Stats["lower"] >= v.Stats["q1"] {
		t.Errorf("stats = %v", v.Stats)
	}
	if want := (100 - v.Stats["upper"]) / v.Stats["iqr"]; math.Abs(v.Anomalies[0].Score-want) > 1e-9 || v.Anomalies[0].Expected != v.Stats["median"] {
		t.Errorf("score/expected = %+v, stats = %v", v.Anomalies[0], v.Stats)
	}
	// Flat series with a dip: IQR is zero, fences collapse, score is the raw distance.
	v, _ = d.Detect(map[string]any{"k": "3"}, Series{Points: toPoints(flat(20, 1, map[int]float64{4: 0.25}))})
	if len(v.Anomalies) != 1 || v.Anomalies[0].Direction != DirectionDown || v.Anomalies[0].Score != 0.75 || v.Stats["iqr"] != 0 || v.Stats["k"] != 3 {
		t.Errorf("flat: %+v %v", v.Anomalies, v.Stats)
	}
	for name, p := range map[string]map[string]any{
		"zero":    {"k": 0},
		"text":    {"k": "x"},
		"unknown": {"threshold": 1},
	} {
		if err := d.Validate(p); err == nil {
			t.Errorf("%s: expected validate error", name)
		}
	}
	if err := d.Validate(map[string]any{"k": "{{ .s }}"}); err != nil {
		t.Errorf("template must be accepted at load: %v", err)
	}
}

func toPoints(vals []float64) []Point {
	out := make([]Point, len(vals))
	for i, v := range vals {
		out[i] = Point{T: baseTS + float64(60*i), V: v}
	}
	return out
}

func TestValidate(t *testing.T) {
	p := New()
	if p.Name() != "anomaly" {
		t.Errorf("name = %q", p.Name())
	}
	ok := []map[string]any{
		{"method": "zscore"},
		{"method": "iqr", "k": 2, "min_points": 5, "direction": "up", "max_anomalies": 0},
		{"method": "iqr", "k": "{{ .s }}", "min_points": "{{ .m }}", "direction": "{{ .d }}", "max_anomalies": "{{ .n }}"},
		{"method": "zscore", "threshold": "3.5"},
		{"method": "iqr", "min_delta": 5, "min_rel_delta": "0.2"},
		{"method": "iqr", "min_delta": "{{ .d }}", "min_rel_delta": "{{ .r }}"},
	}
	for i, with := range ok {
		if err := p.Validate(with); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]struct {
		with map[string]any
		want string
	}{
		"missing method":   {map[string]any{}, "with.method: required (one of: iqr, zscore)"},
		"unknown method":   {map[string]any{"method": "prophet"}, `unknown detector "prophet" (available: iqr, zscore)`},
		"template method":  {map[string]any{"method": "{{ .m }}"}, "with.method: must be a literal"},
		"method not str":   {map[string]any{"method": 1}, "with.method: must be a string"},
		"detector key":     {map[string]any{"method": "iqr", "kk": 1}, "method iqr: with: unknown field(s) kk (allowed: k)"},
		"foreign key":      {map[string]any{"method": "iqr", "threshold": 1}, "method iqr: with: unknown field(s) threshold"},
		"bad direction":    {map[string]any{"method": "iqr", "direction": "sideways"}, `with.direction: "sideways" is not one of both, up, down`},
		"min_points zero":  {map[string]any{"method": "iqr", "min_points": 0}, "with.min_points: must be >= 1"},
		"min_points frac":  {map[string]any{"method": "iqr", "min_points": 2.5}, "with.min_points: must be an integer"},
		"max_anomalies":    {map[string]any{"method": "iqr", "max_anomalies": -1}, "with.max_anomalies: must be >= 0"},
		"detector value":   {map[string]any{"method": "zscore", "threshold": -1}, "with.threshold: must be > 0"},
		"detector textual": {map[string]any{"method": "zscore", "threshold": "three"}, `with.threshold: "three" is not a number`},
		"min_delta":        {map[string]any{"method": "iqr", "min_delta": -1}, "with.min_delta: must be >= 0 (0 = off), got -1"},
		"min_rel_delta":    {map[string]any{"method": "iqr", "min_rel_delta": "half"}, `with.min_rel_delta: "half" is not a number`},
	}
	for name, c := range bad {
		err := p.Validate(c.with)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestRunReport(t *testing.T) {
	data := matrix(
		row(map[string]any{"pod": "spiky"}, flat(30, 1, map[int]float64{17: 10})...),
		row(map[string]any{"pod": "short"}, 1, 2, 3),
		row(map[string]any{"pod": "flat"}, flat(30, 1, nil)...),
	)
	out := run(t, map[string]any{"method": "zscore"}, data)
	want := map[string]any{"method": "zscore", "series_total": 3, "series_analyzed": 2, "series_skipped": 1, "total_anomalies": 1}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %#v, want %#v", k, out[k], v)
		}
	}
	list := out["series"].([]any)
	if len(list) != 3 {
		t.Fatalf("series = %#v", list)
	}
	spiky := list[0].(map[string]any)
	if spiky["metric"].(map[string]any)["pod"] != "spiky" || spiky["points"] != 30 || spiky["skipped"] != false || spiky["anomaly_count"] != 1 {
		t.Errorf("spiky = %#v", spiky)
	}
	stats := spiky["stats"].(map[string]any)
	if stats["min"] != 1.0 || stats["max"] != 10.0 || stats["threshold"] != 3.0 {
		t.Errorf("stats = %#v", stats)
	}
	if _, has := spiky["expected_values"]; has {
		t.Error("zscore has no per-point model, expected_values must be absent")
	}
	an := spiky["anomalies"].([]any)[0].(map[string]any)
	ts := baseTS + 17*60
	if an["timestamp"] != ts || an["value"] != 10.0 || an["direction"] != "up" || an["score"].(float64) <= 3 {
		t.Errorf("anomaly = %#v", an)
	}
	if an["time"] != time.Unix(int64(ts), 0).UTC().Format(time.RFC3339) {
		t.Errorf("time = %v", an["time"])
	}
	short := list[1].(map[string]any)
	if short["skipped"] != true || short["anomaly_count"] != 0 || len(short["anomalies"].([]any)) != 0 || !strings.Contains(short["reason"].(string), "fewer than 10 points") {
		t.Errorf("short = %#v", short)
	}
	if _, has := short["stats"]; has {
		t.Error("skipped series must not carry stats")
	}
	flatEntry := list[2].(map[string]any)
	if flatEntry["skipped"] != false || flatEntry["anomaly_count"] != 0 {
		t.Errorf("flat = %#v", flatEntry)
	}

	// The report must survive JSON encoding (no NaN/Inf, only JSON-compatible types).
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("report is not JSON-encodable: %v", err)
	}
}

func TestRunDirectionAndCap(t *testing.T) {
	data := matrix(row(map[string]any{"pod": "p"}, flat(40, 1, map[int]float64{5: 30, 12: -20, 20: 50, 33: 25})...))
	// Only upward anomalies.
	out := run(t, map[string]any{"method": "iqr", "direction": "up"}, data)
	entry := out["series"].([]any)[0].(map[string]any)
	if out["total_anomalies"] != 3 || entry["anomaly_count"] != 3 {
		t.Errorf("up: total = %v entry = %#v", out["total_anomalies"], entry)
	}
	for _, a := range entry["anomalies"].([]any) {
		if a.(map[string]any)["direction"] != "up" {
			t.Errorf("direction filter leaked: %#v", a)
		}
	}
	// Cap keeps the most severe, reported in time order; the count stays complete.
	out = run(t, map[string]any{"method": "iqr", "max_anomalies": "2"}, data)
	entry = out["series"].([]any)[0].(map[string]any)
	got := entry["anomalies"].([]any)
	if entry["anomaly_count"] != 4 || len(got) != 2 {
		t.Fatalf("cap: count = %v anomalies = %#v", entry["anomaly_count"], got)
	}
	if got[0].(map[string]any)["value"] != 30.0 || got[1].(map[string]any)["value"] != 50.0 {
		t.Errorf("cap should keep 30 and 50 in time order: %#v", got)
	}
	// 0 means unlimited.
	out = run(t, map[string]any{"method": "iqr", "max_anomalies": 0}, data)
	if n := len(out["series"].([]any)[0].(map[string]any)["anomalies"].([]any)); n != 4 {
		t.Errorf("unlimited: got %d anomalies", n)
	}
	// min_points from a rendered string.
	out = run(t, map[string]any{"method": "iqr", "min_points": "41"}, data)
	if out["series_skipped"] != 1 {
		t.Errorf("min_points: %#v", out)
	}
}

func TestRunEmptyAndErrors(t *testing.T) {
	out := run(t, map[string]any{"method": "iqr"}, map[string]any{"resultType": "matrix", "result": []any{}})
	if out["series_total"] != 0 || out["total_anomalies"] != 0 || len(out["series"].([]any)) != 0 {
		t.Errorf("empty = %#v", out)
	}
	out = run(t, map[string]any{"method": "iqr"}, map[string]any{"resultType": "matrix", "result": nil})
	if out["series_total"] != 0 {
		t.Errorf("nil result = %#v", out)
	}
	cases := map[string]struct {
		with map[string]any
		data any
		want string
	}{
		"vector input":   {map[string]any{"method": "iqr"}, map[string]any{"resultType": "vector", "result": []any{}}, "request.type: range"},
		"unrendered":     {map[string]any{"method": "iqr", "k": "{{ .s }}"}, matrix(row(nil, flat(12, 1, nil)...)), "with.k: unrendered template"},
		"bad rendered":   {map[string]any{"method": "zscore", "threshold": "abc"}, matrix(row(nil, flat(12, 1, nil)...)), `method zscore: with.threshold: "abc" is not a number`},
		"unknown method": {map[string]any{"method": "x"}, matrix(), "unknown detector"},
	}
	for name, c := range cases {
		_, err := New().Run(context.Background(), c.with, c.data, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(ctx, map[string]any{"method": "iqr"}, matrix(row(nil, flat(12, 1, nil)...)), nil); err == nil {
		t.Error("cancelled context should abort")
	}
}

func TestCapBySeverity(t *testing.T) {
	in := []Anomaly{{Index: 1, Score: 2}, {Index: 2, Score: 9}, {Index: 3, Score: 5}, {Index: 4, Score: 9}}
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

func TestExpectedValuesPassThrough(t *testing.T) {
	s := Series{Points: toPoints([]float64{1, 2, 3})}
	got := expectedToAny(s, []float64{1.5, 2.5, 3.5, 99})
	if len(got) != 3 || !reflect.DeepEqual(got[1], []any{baseTS + 60, 2.5}) {
		t.Errorf("expected_values = %#v", got)
	}
}

// quiet is a nearly idle series around 0.2 whose spread is a few hundredths.
func quiet(n int, overrides map[int]float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 0.2 + 0.005*float64((i*7)%5-2)
		if o, ok := overrides[i]; ok {
			out[i] = o
		}
	}
	return out
}

func TestRunMinDelta(t *testing.T) {
	// A blip of +0.1 on a nearly idle series is far outside its fences (the
	// spread is 0.01) but meaningless in absolute terms; the jump to 8 is not.
	data := matrix(row(map[string]any{"instance": "vm-1"}, quiet(40, map[int]float64{10: 0.3, 30: 8})...))
	plain := run(t, map[string]any{"method": "iqr", "k": 3}, data)
	s0 := plain["series"].([]any)[0].(map[string]any)
	if s0["anomaly_count"] != 2 {
		t.Fatalf("without min_delta both samples are anomalies: %v", s0["anomalies"])
	}
	if _, has := s0["below_min_delta"]; has {
		t.Errorf("below_min_delta must be absent when the filter is off: %v", s0)
	}
	out := run(t, map[string]any{"method": "iqr", "k": 3, "min_delta": "5"}, data)
	s1 := out["series"].([]any)[0].(map[string]any)
	if out["total_anomalies"] != 1 || s1["anomaly_count"] != 1 || s1["below_min_delta"] != 1 {
		t.Fatalf("min_delta 5: %v", s1)
	}
	if a := s1["anomalies"].([]any)[0].(map[string]any); a["value"] != 8.0 {
		t.Errorf("kept %v, want the jump to 8", a)
	}
	// The relative threshold applies when it is larger: at a level of 100 a
	// move of 3 is below 20% of it, a move of 30 is not.
	level := flat(40, 100, map[int]float64{5: 100.5, 12: 99.5, 20: 103, 33: 130})
	out = run(t, map[string]any{"method": "iqr", "k": 3, "min_delta": 1, "min_rel_delta": 0.2}, matrix(row(nil, level...)))
	s2 := out["series"].([]any)[0].(map[string]any)
	if s2["anomaly_count"] != 1 || s2["anomalies"].([]any)[0].(map[string]any)["value"] != 130.0 {
		t.Errorf("min_rel_delta 0.2 at a level of 100: %v", s2)
	}
	// zscore compares against the mean, which the report shows as expected.
	out = run(t, map[string]any{"method": "zscore", "threshold": 3, "min_delta": 5}, data)
	s3 := out["series"].([]any)[0].(map[string]any)
	if s3["anomaly_count"] != 1 || s3["below_min_delta"] != 0 {
		t.Errorf("zscore: %v", s3)
	}
	// The filter is applied after direction: below_min_delta counts only
	// samples of the requested direction.
	down := quiet(40, map[int]float64{10: 0.1, 30: 8})
	out = run(t, map[string]any{"method": "iqr", "k": 3, "min_delta": 5, "direction": "down"}, matrix(row(nil, down...)))
	if s4 := out["series"].([]any)[0].(map[string]any); s4["anomaly_count"] != 0 || s4["below_min_delta"] != 1 {
		t.Errorf("direction down: %v", s4)
	}
}
