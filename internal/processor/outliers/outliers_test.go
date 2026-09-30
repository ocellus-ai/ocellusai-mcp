package outliers

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// items builds the canonical input: one item per value, keyed k0, k1, ...
func items(vals ...float64) []any {
	out := make([]any, 0, len(vals))
	for i, v := range vals {
		out = append(out, map[string]any{"key": "k" + string(rune('0'+i)), "value": v})
	}
	return out
}

func labeled(key string, value any, labels map[string]any) map[string]any {
	return map[string]any{"key": key, "value": value, "labels": labels}
}

func run(t *testing.T, with map[string]any, data any) map[string]any {
	t.Helper()
	out, err := New().Run(context.Background(), with, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with map[string]any
		err  string
	}{
		{"iqr", map[string]any{"method": "iqr", "k": 3}, ""},
		{"zscore", map[string]any{"method": "zscore", "threshold": "2.5", "group_by": []any{"host", "zone"}}, ""},
		{"templates deferred", map[string]any{"method": "iqr", "k": "{{ .k }}", "min_points": "{{ .n }}", "direction": "{{ .d }}", "group_by": []any{"{{ .g }}"}}, ""},
		{"group_by string", map[string]any{"method": "iqr", "group_by": "host"}, ""},
		{"min_delta", map[string]any{"method": "iqr", "min_delta": 5, "min_rel_delta": "{{ .r }}"}, ""},
		{"min_delta negative", map[string]any{"method": "iqr", "min_delta": -5}, "with.min_delta: must be >= 0"},
		{"missing method", map[string]any{}, "with.method: required (one of: iqr, zscore)"},
		{"template method", map[string]any{"method": "{{ .m }}"}, "with.method: must be a literal"},
		{"unknown method", map[string]any{"method": "mad"}, `unknown detector "mad" (available: iqr, zscore)`},
		{"detector key", map[string]any{"method": "iqr", "threshold": 3}, "method iqr: with: unknown field(s) threshold (allowed: k)"},
		{"bad k", map[string]any{"method": "iqr", "k": 0}, "with.k: must be > 0"},
		{"min_points", map[string]any{"method": "iqr", "min_points": 0}, "with.min_points: must be >= 1"},
		{"direction", map[string]any{"method": "iqr", "direction": "sideways"}, `with.direction: "sideways" is not one of both, up, down`},
		{"max_anomalies", map[string]any{"method": "iqr", "max_anomalies": -1}, "with.max_anomalies: must be >= 0"},
		{"group_by type", map[string]any{"method": "iqr", "group_by": 1}, "with.group_by: must be a list of strings, got number"},
		{"group_by element", map[string]any{"method": "iqr", "group_by": []any{"host", 2}}, "with.group_by[1]: must be a string"},
		{"group_by empty", map[string]any{"method": "iqr", "group_by": []any{""}}, "with.group_by[0]: must not be empty"},
		{"group_by duplicate", map[string]any{"method": "iqr", "group_by": []any{"host", "host"}}, `with.group_by: duplicate label "host"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := New().Validate(c.with)
			if c.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
		})
	}
}

func TestRunReport(t *testing.T) {
	// Seven filesystems around 40% and one at 97%.
	data := items(41, 38, 40, 97, 42, 39, 40, 43)
	out := run(t, map[string]any{"method": "iqr", "k": "1.5"}, data)
	want := map[string]any{"method": "iqr", "series_total": 1, "series_analyzed": 1, "series_skipped": 0, "total_anomalies": 1}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %#v, want %#v", k, out[k], v)
		}
	}
	if !reflect.DeepEqual(out["group_by"], []any{}) {
		t.Errorf("group_by = %#v", out["group_by"])
	}
	list := out["series"].([]any)
	if len(list) != 1 {
		t.Fatalf("series = %#v", list)
	}
	s := list[0].(map[string]any)
	if !reflect.DeepEqual(s["labels"], map[string]any{}) || s["points"] != 8 || s["skipped"] != false || s["anomaly_count"] != 1 {
		t.Errorf("series = %#v", s)
	}
	stats := s["stats"].(map[string]any)
	if stats["min"] != 38.0 || stats["max"] != 97.0 || stats["median"] != 40.5 || stats["k"] != 1.5 {
		t.Errorf("stats = %#v", stats)
	}
	an := s["anomalies"].([]any)[0].(map[string]any)
	if an["index"] != 3 || an["key"] != "k3" || an["value"] != 97.0 || an["direction"] != "up" || an["expected"] != 40.5 || an["score"].(float64) <= 0 {
		t.Errorf("anomaly = %#v", an)
	}
	for _, absent := range []string{"timestamp", "time"} {
		if _, has := an[absent]; has {
			t.Errorf("anomaly must not carry %s", absent)
		}
	}
	// The report must survive JSON encoding.
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("report is not JSON-encodable: %v", err)
	}
}

func TestRunGroupBy(t *testing.T) {
	data := []any{
		labeled("a1", 10, map[string]any{"host": "a"}),
		labeled("a2", 11, map[string]any{"host": "a"}),
		labeled("a3", 9, map[string]any{"host": "a"}),
		labeled("a4", 10, map[string]any{"host": "a"}),
		labeled("a5", 60, map[string]any{"host": "a"}),
		labeled("b1", 1, map[string]any{"host": "b", "zone": 2}),
		labeled("b2", 1, map[string]any{"host": "b"}),
		labeled("n1", 5, nil),
		labeled("n2", 5, map[string]any{"zone": "x"}),
	}
	out := run(t, map[string]any{"method": "iqr", "group_by": "host"}, data)
	if !reflect.DeepEqual(out["group_by"], []any{"host"}) || out["series_total"] != 3 || out["series_analyzed"] != 1 || out["series_skipped"] != 2 || out["total_anomalies"] != 1 {
		t.Errorf("header = %#v", out)
	}
	list := out["series"].([]any)
	unl := list[0].(map[string]any)
	if unl["skipped"] != true || unl["points"] != 2 || unl["reason"] != "missing group_by label(s) host" || !reflect.DeepEqual(unl["labels"], map[string]any{}) {
		t.Errorf("unlabeled = %#v", unl)
	}
	a := list[1].(map[string]any)
	if !reflect.DeepEqual(a["labels"], map[string]any{"host": "a"}) || a["points"] != 5 || a["anomaly_count"] != 1 {
		t.Errorf("host a = %#v", a)
	}
	an := a["anomalies"].([]any)[0].(map[string]any)
	if an["key"] != "a5" || an["index"] != 4 {
		t.Errorf("anomaly = %#v", an)
	}
	b := list[2].(map[string]any)
	// Only group_by labels are echoed; zone is not part of the key.
	if !reflect.DeepEqual(b["labels"], map[string]any{"host": "b"}) || b["points"] != 2 || b["skipped"] != true || !strings.Contains(b["reason"].(string), "fewer than 5 items") {
		t.Errorf("host b = %#v", b)
	}
	if _, has := b["stats"]; has {
		t.Error("skipped series must not carry stats")
	}
	// Two labels: the group key is the combination, in sorted order.
	out = run(t, map[string]any{"method": "iqr", "group_by": []any{"host", "zone"}, "min_points": 1}, data[5:6])
	s := out["series"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(s["labels"], map[string]any{"host": "b", "zone": "2"}) {
		t.Errorf("labels = %#v", s["labels"])
	}
}

func TestRunDirectionAndCap(t *testing.T) {
	data := items(10, 10, 10, 10, 10, 10, 10, 10, 30, 0, 100)
	up := run(t, map[string]any{"method": "iqr", "direction": "up"}, data)
	s := up["series"].([]any)[0].(map[string]any)
	if s["anomaly_count"] != 2 || up["total_anomalies"] != 2 {
		t.Errorf("up = %#v", s)
	}
	// Most severe first: 100 beats 30.
	list := s["anomalies"].([]any)
	if list[0].(map[string]any)["value"] != 100.0 || list[1].(map[string]any)["value"] != 30.0 {
		t.Errorf("order = %#v", list)
	}
	down := run(t, map[string]any{"method": "iqr", "direction": "down"}, data)
	s = down["series"].([]any)[0].(map[string]any)
	if s["anomaly_count"] != 1 || s["anomalies"].([]any)[0].(map[string]any)["value"] != 0.0 {
		t.Errorf("down = %#v", s)
	}
	capped := run(t, map[string]any{"method": "iqr", "max_anomalies": 1}, data)
	s = capped["series"].([]any)[0].(map[string]any)
	// anomaly_count counts before the cap; the strongest survives.
	if s["anomaly_count"] != 3 || len(s["anomalies"].([]any)) != 1 || s["anomalies"].([]any)[0].(map[string]any)["value"] != 100.0 {
		t.Errorf("capped = %#v", s)
	}
	all := run(t, map[string]any{"method": "iqr", "max_anomalies": 0}, data)
	if len(all["series"].([]any)[0].(map[string]any)["anomalies"].([]any)) != 3 {
		t.Errorf("max_anomalies 0 must keep every anomaly")
	}
}

func TestRunItems(t *testing.T) {
	// Numeric strings, numeric keys, NaN/Inf dropped, extra fields ignored, key optional.
	data := []any{
		map[string]any{"key": 1, "value": "10", "extra": true},
		map[string]any{"key": 2, "value": 10},
		map[string]any{"key": 3, "value": "NaN"},
		map[string]any{"key": 4, "value": "+Inf"},
		map[string]any{"value": 10},
		map[string]any{"key": 6, "value": 11},
		map[string]any{"key": 7, "value": 9},
		map[string]any{"key": 8, "value": 50},
	}
	out := run(t, map[string]any{"method": "iqr"}, data)
	s := out["series"].([]any)[0].(map[string]any)
	if s["points"] != 6 || s["anomaly_count"] != 1 {
		t.Errorf("series = %#v", s)
	}
	an := s["anomalies"].([]any)[0].(map[string]any)
	if an["key"] != 8 || an["index"] != 7 {
		t.Errorf("anomaly = %#v", an)
	}
	// Empty input is one skipped series, not an error.
	out = run(t, map[string]any{"method": "zscore"}, []any{})
	s = out["series"].([]any)[0].(map[string]any)
	if out["series_total"] != 1 || s["skipped"] != true || s["points"] != 0 {
		t.Errorf("empty = %#v", out)
	}

	for _, c := range []struct {
		name string
		data any
		err  string
	}{
		{"not a list", map[string]any{"resultType": "vector"}, "expected a list of items [{key, value, labels?}, ...] (shape the data with a preceding fn: jq step), got object"},
		{"null", nil, "got null"},
		{"item not object", []any{1}, "items[0]: expected an object {key, value, labels?}, got number"},
		{"value missing", []any{map[string]any{"key": "x"}}, "items[0] (key x).value: required"},
		{"value null", []any{map[string]any{"value": nil}}, "items[0].value: required"},
		{"value text", []any{map[string]any{"key": "x", "value": "lots"}}, `items[0] (key x).value: "lots" is not a number`},
		{"value object", []any{map[string]any{"value": map[string]any{}}}, "items[0].value: expected a number, got object"},
		{"key object", []any{map[string]any{"key": []any{}, "value": 1}}, "items[0].key: must be a scalar, got array"},
		{"labels type", []any{map[string]any{"key": "x", "value": 1, "labels": "host=a"}}, "items[0] (key x).labels: must be an object, got string"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := New().Run(context.Background(), map[string]any{"method": "iqr"}, c.data, nil)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("err = %v, want %q", err, c.err)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	data := items(1, 2, 3, 4, 5)
	for _, c := range []struct {
		name string
		with map[string]any
		err  string
	}{
		{"unrendered", map[string]any{"method": "iqr", "k": "{{ .k }}"}, "with.k: unrendered template"},
		{"unrendered group_by", map[string]any{"method": "iqr", "group_by": []any{"{{ .g }}"}}, "with.group_by[0]: unrendered template"},
		{"bad rendered k", map[string]any{"method": "iqr", "k": "-1"}, "method iqr: with.k: must be > 0"},
		{"bad rendered direction", map[string]any{"method": "iqr", "direction": "any"}, "with.direction"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := New().Run(context.Background(), c.with, data, nil)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("err = %v, want %q", err, c.err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(ctx, map[string]any{"method": "iqr"}, data, nil); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestZScoreSmallSet(t *testing.T) {
	// The z-score of n items is bounded by (n-1)/sqrt(n): with six items and
	// threshold 3 nothing can fire, with threshold 2 the extreme one does.
	data := items(1, 1, 1, 1, 1, 100)
	out := run(t, map[string]any{"method": "zscore", "threshold": 3}, data)
	if out["total_anomalies"] != 0 {
		t.Errorf("threshold 3 on six items must never fire: %#v", out)
	}
	out = run(t, map[string]any{"method": "zscore", "threshold": 2}, data)
	if out["total_anomalies"] != 1 {
		t.Errorf("threshold 2: %#v", out)
	}
}

func TestRunMinDelta(t *testing.T) {
	// Eight mount points around 40% and one at 41.5%: the spread is 0.2 points,
	// so 41.5 is an outlier by the fences but not worth reporting; 97% is.
	data := items(40, 40.1, 39.9, 40, 40.2, 39.8, 40, 41.5, 97)
	plain := run(t, map[string]any{"method": "iqr"}, data)
	if s := plain["series"].([]any)[0].(map[string]any); s["anomaly_count"] != 2 {
		t.Fatalf("without min_delta: %v", s["anomalies"])
	}
	out := run(t, map[string]any{"method": "iqr", "min_delta": 5}, data)
	s := out["series"].([]any)[0].(map[string]any)
	if s["anomaly_count"] != 1 || s["below_min_delta"] != 1 || out["total_anomalies"] != 1 {
		t.Fatalf("min_delta 5: %v", s)
	}
	if a := s["anomalies"].([]any)[0].(map[string]any); a["key"] != "k8" || a["value"] != 97.0 {
		t.Errorf("kept %v, want k8 = 97", a)
	}
}
