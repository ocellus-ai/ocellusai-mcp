package clusterevents

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const baseTS = 1757600000.0

// ev builds an event of one instance from offsets in seconds.
func ev(inst string, start, end float64, signals ...string) map[string]any {
	m := map[string]any{"start": baseTS + start, "end": baseTS + end}
	if inst != "" {
		m["labels"] = map[string]any{"instance": inst}
	}
	if signals != nil {
		list := make([]any, 0, len(signals))
		for _, s := range signals {
			list = append(list, s)
		}
		m["signals"] = list
	}
	return m
}

func list(evs ...map[string]any) []any {
	out := make([]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, e)
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

func clusters(out map[string]any) []map[string]any {
	var cs []map[string]any
	for _, c := range out["clusters"].([]any) {
		cs = append(cs, c.(map[string]any))
	}
	return cs
}

func instances(c map[string]any) []string {
	var names []string
	for _, s := range c["series"].([]any) {
		names = append(names, s.(map[string]any)["instance"].(string))
	}
	return names
}

func TestName(t *testing.T) {
	if New().Name() != "cluster_events" {
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
		{"all keys", map[string]any{"gap": "5m", "min_series": 3, "same_signal": true, "max_clusters": 0}, ""},
		{"strings", map[string]any{"gap": "300", "min_series": "2", "same_signal": "false", "max_clusters": "5"}, ""},
		{"templates deferred", map[string]any{"gap": "{{ .g }}", "min_series": "{{ .m }}", "same_signal": "{{ .s }}", "max_clusters": "{{ .c }}"}, ""},
		{"unknown key", map[string]any{"by": "instance"}, "with: unknown field(s) by (allowed: gap, min_series, same_signal, max_clusters)"},
		{"gap negative", map[string]any{"gap": -1}, "with.gap: must be >= 0 seconds"},
		{"gap text", map[string]any{"gap": "soon"}, `with.gap: expected seconds or a duration such as 30s, got "soon"`},
		{"min_series zero", map[string]any{"min_series": 0}, "with.min_series: must be >= 1"},
		{"min_series fraction", map[string]any{"min_series": 1.5}, "with.min_series: must be an integer"},
		{"max_clusters negative", map[string]any{"max_clusters": -1}, "with.max_clusters: must be >= 0"},
		{"same_signal text", map[string]any{"same_signal": "yes"}, `with.same_signal: "yes" is not a boolean`},
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
	data := list(ev("a", 0, 60))
	for _, with := range []map[string]any{{"gap": "{{ .g }}"}, {"min_series": "{{ .m }}"}, {"same_signal": "{{ .s }}"}, {"max_clusters": "{{ .c }}"}} {
		if _, err := New().Run(context.Background(), with, data, nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
			t.Errorf("with %v: error = %v, want unrendered template", with, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		data any
		err  string
	}{
		{"object", map[string]any{"events": []any{}}, "expected a list of events [{start, end, labels?, signals?, score?}, ...] (shape the report with a preceding fn: jq step), got object"},
		{"item not object", []any{1}, "events[0]: expected an object {start, end, labels?, signals?, score?}, got number"},
		{"missing start", []any{map[string]any{"end": 1}}, "events[0].start: required (unix seconds or an RFC 3339 time)"},
		{"missing end", []any{map[string]any{"start": 1}}, "events[0].end: required"},
		{"bad time", []any{map[string]any{"start": "yesterday", "end": 1}}, `events[0].start: expected unix seconds or an RFC 3339 time, got "yesterday"`},
		{"time object", []any{map[string]any{"start": map[string]any{}, "end": 1}}, "events[0].start: expected unix seconds or an RFC 3339 time, got map[]"},
		{"end before start", []any{map[string]any{"start": baseTS + 60, "end": baseTS}}, "events[0]: end (2025-09-11T14:13:20Z) before start (2025-09-11T14:14:20Z)"},
		{"labels not object", []any{map[string]any{"start": 1, "end": 2, "labels": "a"}}, "events[0].labels: must be an object, got string"},
		{"signals not list", []any{map[string]any{"start": 1, "end": 2, "signals": "cpu"}}, "events[0].signals: must be a list of strings, got string"},
		{"signal not string", []any{map[string]any{"start": 1, "end": 2, "signals": []any{1}}}, "events[0].signals[0]: must be a string, got number"},
		{"score text", []any{map[string]any{"start": 1, "end": 2, "score": "high"}}, `events[0].score: "high" is not a number`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Run(context.Background(), map[string]any{}, tc.data, nil)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestOverlapChain(t *testing.T) {
	// A-B and B-C overlap, A-C do not: one cluster of three series; D is alone.
	data := list(ev("vm1", 0, 100, "cpu"), ev("vm2", 90, 200, "cpu", "disk"), ev("vm3", 190, 300, "disk"), ev("vm1", 1000, 1100, "mem"))
	out := run(t, map[string]any{}, data)
	cs := clusters(out)
	if len(cs) != 1 || out["clusters_total"] != 1 || out["clustered_events"] != 3 || out["events_total"] != 4 || out["series_total"] != 3 {
		t.Fatalf("clusters = %v, totals %v/%v/%v/%v", cs, out["clusters_total"], out["clustered_events"], out["events_total"], out["series_total"])
	}
	c := cs[0]
	if c["series_count"] != 3 || c["event_count"] != 3 || c["start"] != baseTS || c["end"] != baseTS+300 || c["duration"] != 300.0 {
		t.Errorf("cluster = %v", c)
	}
	if c["start_time"] != "2025-09-11T14:13:20Z" || c["end_time"] != "2025-09-11T14:18:20Z" {
		t.Errorf("times = %v .. %v", c["start_time"], c["end_time"])
	}
	if got := instances(c); !reflect.DeepEqual(got, []string{"vm1", "vm2", "vm3"}) {
		t.Errorf("series = %v", got)
	}
	// signals: cpu and disk on two series each, cpu first by name
	want := []any{map[string]any{"signal": "cpu", "series": 2}, map[string]any{"signal": "disk", "series": 2}}
	if !reflect.DeepEqual(c["signals"], want) {
		t.Errorf("signals = %v", c["signals"])
	}
	if c["score"] != nil {
		t.Errorf("score without scores = %v, want null", c["score"])
	}
	singles := out["singles"].([]any)
	if len(singles) != 1 || singles[0].(map[string]any)["start"] != baseTS+1000 {
		t.Errorf("singles = %v", singles)
	}
}

func TestGap(t *testing.T) {
	data := list(ev("vm1", 0, 100), ev("vm2", 160, 200))
	if out := run(t, map[string]any{}, data); len(clusters(out)) != 0 || len(out["singles"].([]any)) != 2 {
		t.Errorf("gap 0: %v", out)
	}
	if out := run(t, map[string]any{"gap": "1m"}, data); len(clusters(out)) != 1 {
		t.Errorf("gap 1m: %v", out)
	}
	if out := run(t, map[string]any{"gap": 59}, data); len(clusters(out)) != 0 {
		t.Errorf("gap 59s: %v", out)
	}
}

func TestLongEarlyEvent(t *testing.T) {
	// one long event covers three short ones on other series, one series twice
	data := list(ev("vm1", 0, 1000, "net"), ev("vm2", 200, 210, "cpu"), ev("vm3", 500, 510, "cpu"), ev("vm2", 900, 910, "disk"))
	out := run(t, map[string]any{}, data)
	cs := clusters(out)
	if len(cs) != 1 || cs[0]["series_count"] != 3 || cs[0]["event_count"] != 4 {
		t.Fatalf("clusters = %v", cs)
	}
	want := []any{map[string]any{"signal": "cpu", "series": 2}, map[string]any{"signal": "disk", "series": 1}, map[string]any{"signal": "net", "series": 1}}
	if !reflect.DeepEqual(cs[0]["signals"], want) {
		t.Errorf("signals = %v", cs[0]["signals"])
	}
	events := cs[0]["events"].([]any)
	if len(events) != 4 || events[3].(map[string]any)["start"] != baseTS+900 {
		t.Errorf("events not in start order: %v", events)
	}
}

func TestSameSignal(t *testing.T) {
	data := list(ev("vm1", 0, 100, "cpu"), ev("vm2", 50, 150, "mem"))
	if out := run(t, map[string]any{}, data); len(clusters(out)) != 1 {
		t.Errorf("without same_signal: %v", out)
	}
	if out := run(t, map[string]any{"same_signal": true}, data); len(clusters(out)) != 0 || len(out["singles"].([]any)) != 2 {
		t.Errorf("same_signal: %v", out)
	}
	// a third event sharing a signal with each links them transitively
	data = append(data, ev("vm3", 60, 120, "cpu", "mem"))
	out := run(t, map[string]any{"same_signal": true}, data)
	if cs := clusters(out); len(cs) != 1 || cs[0]["series_count"] != 3 {
		t.Errorf("transitive: %v", cs)
	}
}

func TestMinSeries(t *testing.T) {
	data := list(ev("vm1", 0, 100, "cpu"), ev("vm1", 50, 150, "mem"))
	out := run(t, map[string]any{}, data)
	if len(clusters(out)) != 0 || len(out["singles"].([]any)) != 2 || out["clusters_total"] != 0 {
		t.Errorf("same series twice must not form a cluster: %v", out)
	}
	out = run(t, map[string]any{"min_series": 1}, data)
	if cs := clusters(out); len(cs) != 1 || cs[0]["series_count"] != 1 || cs[0]["event_count"] != 2 {
		t.Errorf("min_series 1: %v", cs)
	}
	data = list(ev("vm1", 0, 100), ev("vm2", 0, 100), ev("vm3", 0, 100))
	if out := run(t, map[string]any{"min_series": 4}, data); len(clusters(out)) != 0 {
		t.Errorf("min_series 4: %v", out)
	}
}

func TestMaxClusters(t *testing.T) {
	data := list(
		ev("vm1", 0, 100), ev("vm2", 0, 100), // cluster 1: two series
		ev("vm1", 1000, 1100), ev("vm2", 1000, 1100), ev("vm3", 1000, 1100), // cluster 2: three series
		ev("vm1", 2000, 2100), ev("vm2", 2000, 2100), ev("vm2", 2050, 2100), // cluster 3: two series, three events
	)
	out := run(t, map[string]any{}, data)
	if cs := clusters(out); len(cs) != 3 || out["clusters_total"] != 3 || out["clustered_events"] != 8 {
		t.Fatalf("all: %v", out)
	}
	out = run(t, map[string]any{"max_clusters": 2}, data)
	cs := clusters(out)
	if len(cs) != 2 || out["clusters_total"] != 3 || out["clustered_events"] != 8 {
		t.Fatalf("capped: %v", out)
	}
	// the strongest two (three series; two series with three events), in time order
	if cs[0]["start"] != baseTS+1000 || cs[1]["start"] != baseTS+2000 {
		t.Errorf("kept %v and %v, want the 1000 and 2000 clusters", cs[0]["start"], cs[1]["start"])
	}
	if out := run(t, map[string]any{"max_clusters": 0}, data); len(clusters(out)) != 3 {
		t.Errorf("max_clusters 0 must keep all: %v", out)
	}
}

func TestPassthroughAndTimes(t *testing.T) {
	a := map[string]any{"start": "2025-09-11T14:13:20Z", "end": "2025-09-11T14:15:20Z", "labels": map[string]any{"instance": "vm1", "job": "node"}, "kind": "single", "score": "12.5", "values": map[string]any{"cpu": 99.0}}
	b := map[string]any{"start": "1757600060", "end": baseTS + 90, "labels": map[string]any{"job": "node", "instance": "vm2"}, "score": 3}
	out := run(t, map[string]any{}, []any{a, b})
	cs := clusters(out)
	if len(cs) != 1 {
		t.Fatalf("clusters = %v", cs)
	}
	c := cs[0]
	if c["score"] != 12.5 || c["start"] != baseTS || c["end"] != baseTS+120 {
		t.Errorf("cluster = %v", c)
	}
	e := c["events"].([]any)[0].(map[string]any)
	if e["kind"] != "single" || e["start"] != baseTS || e["start_time"] != "2025-09-11T14:13:20Z" || e["end_time"] != "2025-09-11T14:15:20Z" {
		t.Errorf("event = %v", e)
	}
	if !reflect.DeepEqual(e["values"], map[string]any{"cpu": 99.0}) {
		t.Errorf("passthrough lost: %v", e)
	}
	if a["start"] != "2025-09-11T14:13:20Z" || a["start_time"] != nil {
		t.Errorf("input mutated: %v", a)
	}
	// series identity ignores label order
	if c["series_count"] != 2 || seriesKey(a["labels"].(map[string]any)) != `{"instance":"vm1","job":"node"}` {
		t.Errorf("series = %v, key = %q", c["series"], seriesKey(a["labels"].(map[string]any)))
	}
}

func TestAnonymousSeries(t *testing.T) {
	data := list(ev("", 0, 100), ev("", 50, 150), ev("vm1", 60, 120))
	out := run(t, map[string]any{}, data)
	cs := clusters(out)
	if len(cs) != 1 || cs[0]["series_count"] != 2 || out["series_total"] != 2 {
		t.Fatalf("clusters = %v, series_total %v", cs, out["series_total"])
	}
	if s := cs[0]["series"].([]any); !reflect.DeepEqual(s[0], map[string]any{}) {
		t.Errorf("anonymous series labels = %v, want {}", s[0])
	}
}

func TestRunEmpty(t *testing.T) {
	out := run(t, map[string]any{}, []any{})
	if len(out["clusters"].([]any)) != 0 || len(out["singles"].([]any)) != 0 || out["events_total"] != 0 || out["series_total"] != 0 {
		t.Errorf("empty: %v", out)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Error(err)
	}
}

func TestRunJSON(t *testing.T) {
	out := run(t, map[string]any{"gap": "1m"}, list(ev("vm1", 0, 100, "cpu"), ev("vm2", 120, 200, "cpu"), ev("vm3", 900, 950)))
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back["clusters"].([]any)) != 1 || len(back["singles"].([]any)) != 1 {
		t.Errorf("round trip = %s", b)
	}
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Run(ctx, map[string]any{}, list(ev("vm1", 0, 100)), nil); err == nil {
		t.Error("expected a context error")
	}
}
