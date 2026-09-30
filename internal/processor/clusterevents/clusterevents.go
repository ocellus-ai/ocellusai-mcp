// Package clusterevents is the `cluster_events` processor: it groups the
// anomalous events of many series that overlap in time into clusters, so
// that "iowait, disk and cpu on five nodes between 11:05 and 11:13" is one
// line instead of fifteen. Analysis over a strict list of events: the report
// of anomaly_ensemble (or of anomaly / anomaly_mv) is flattened into that
// list by a preceding fn: jq step, which also decides which events count.
//
// Input: a list of events
//
//	[{"start": 1757600000, "end": 1757600480, "labels": {"instance": "a"},
//	  "signals": ["cpu", "iowait"], "score": 41.2, ...}, ...]
//
// start and end are required: unix seconds (a number or a numeric string)
// or an RFC 3339 time, end not before start. labels is an optional object
// identifying the series the event belongs to (events without labels belong
// to one anonymous series); signals an optional list of names of what moved;
// score an optional number. Every other field is kept and returned with the
// event.
//
// with:
//
//	gap           two events link when they overlap or lie at most gap
//	              apart: seconds or a duration such as 5m (default 0).
//	min_series    a group needs events from at least this many distinct
//	              series to be a cluster (default 2); the events of smaller
//	              groups are returned one by one under singles.
//	same_signal   true: two events link only when they share a signal
//	              (default false). Linking is transitive either way: when A
//	              links B and B links C, A and C are in one cluster.
//	max_clusters  strongest clusters to return, by series_count then
//	              event_count, output in time order; 0 = all (default 20).
//
// Output:
//
//	{clusters: [{start, end, start_time, end_time, duration, series_count,
//	             event_count, score, series: [labels], signals: [{signal, series}],
//	             events: [event + start, end, start_time, end_time]}],
//	 singles: [event + start, end, start_time, end_time],
//	 events_total, series_total, clusters_total, clustered_events}
//
// A cluster spans the earliest start and the latest end of its events
// (duration = end - start); score is the largest event score; series lists
// the distinct label sets in order of first appearance; signals counts, per
// signal, the distinct series that reported it, most common first.
// clusters_total and clustered_events are counted before the max_clusters
// cut; clusters and singles are in time order.
package clusterevents

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: cluster_events).
const Name = "cluster_events"

// Defaults of the `with` settings.
const (
	DefaultMinSeries   = 2
	DefaultMaxClusters = 20
)

// allowedKeys are the `with` keys, all optional.
var allowedKeys = []string{"gap", "min_series", "same_signal", "max_clusters"}

// Processor implements processor.Processor.
type Processor struct{}

// New creates the event clustering processor.
func New() *Processor { return &Processor{} }

// Name returns "cluster_events".
func (*Processor) Name() string { return Name }

type settings struct {
	gap         float64 // seconds
	minSeries   int
	sameSignal  bool
	maxClusters int
}

// Validate checks the raw `with` section at catalog load time. Values that
// are still templates are checked after rendering, in Run.
func (*Processor) Validate(with map[string]any) error {
	_, err := parseSettings(with, true)
	return err
}

// parseSettings reads every key. At load time a value that is still a
// template is skipped; at run time everything must be a final value.
func parseSettings(with map[string]any, load bool) (*settings, error) {
	if err := processor.CheckKeys(with, allowedKeys...); err != nil {
		return nil, err
	}
	st := &settings{minSeries: DefaultMinSeries, maxClusters: DefaultMaxClusters}
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }
	var err error
	if !deferred("gap") {
		if st.gap, err = processor.Seconds(with, "gap", 0); err != nil {
			return nil, err
		}
	}
	if !deferred("min_series") {
		if st.minSeries, err = processor.Int(with, "min_series", DefaultMinSeries); err != nil {
			return nil, err
		}
		if st.minSeries < 1 {
			return nil, fmt.Errorf("with.min_series: must be >= 1, got %d", st.minSeries)
		}
	}
	if !deferred("same_signal") {
		if st.sameSignal, err = processor.Bool(with, "same_signal", false); err != nil {
			return nil, err
		}
	}
	if !deferred("max_clusters") {
		if st.maxClusters, err = processor.Int(with, "max_clusters", DefaultMaxClusters); err != nil {
			return nil, err
		}
		if st.maxClusters < 0 {
			return nil, fmt.Errorf("with.max_clusters: must be >= 0 (0 = all), got %d", st.maxClusters)
		}
	}
	return st, nil
}

// event is one parsed input event.
type event struct {
	start, end float64
	key        string         // canonical labels; "" for the anonymous series
	labels     map[string]any // nil for the anonymous series
	signals    []string
	score      float64
	hasScore   bool
	raw        map[string]any
}

// Run clusters the events. params is unused: every setting comes from with.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	events, err := parseEvents(data)
	if err != nil {
		return nil, err
	}
	groups, err := cluster(ctx, events, st)
	if err != nil {
		return nil, err
	}

	var clusters []*group
	var singles []int
	seriesSeen := map[string]bool{}
	for _, e := range events {
		seriesSeen[e.key] = true
	}
	clusteredEvents := 0
	for _, members := range groups {
		g := newGroup(events, members)
		if len(g.seriesKeys) >= st.minSeries {
			clusters = append(clusters, g)
			clusteredEvents += len(members)
		} else {
			singles = append(singles, members...)
		}
	}
	clustersTotal := len(clusters)
	clusters = capClusters(clusters, st.maxClusters)
	sort.SliceStable(singles, func(a, b int) bool { return events[singles[a]].start < events[singles[b]].start })

	outClusters := make([]any, 0, len(clusters))
	for _, g := range clusters {
		outClusters = append(outClusters, g.toAny(events))
	}
	outSingles := make([]any, 0, len(singles))
	for _, i := range singles {
		outSingles = append(outSingles, eventToAny(events[i]))
	}
	return map[string]any{
		"clusters":         outClusters,
		"singles":          outSingles,
		"events_total":     len(events),
		"series_total":     len(seriesSeen),
		"clusters_total":   clustersTotal,
		"clustered_events": clusteredEvents,
	}, nil
}

// parseEvents reads the canonical input list; every defect is an error
// naming the event.
func parseEvents(data any) ([]event, error) {
	list, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list of events [{start, end, labels?, signals?, score?}, ...] (shape the report with a preceding fn: jq step), got %s", processor.JSONType(data))
	}
	out := make([]event, 0, len(list))
	for i, raw := range list {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("events[%d]: expected an object {start, end, labels?, signals?, score?}, got %s", i, processor.JSONType(raw))
		}
		e := event{raw: obj}
		var err error
		if e.start, err = timeOf(obj, "start"); err != nil {
			return nil, fmt.Errorf("events[%d].start: %w", i, err)
		}
		if e.end, err = timeOf(obj, "end"); err != nil {
			return nil, fmt.Errorf("events[%d].end: %w", i, err)
		}
		if e.end < e.start {
			return nil, fmt.Errorf("events[%d]: end (%s) before start (%s)", i, series.FormatTime(e.end), series.FormatTime(e.start))
		}
		if l, present := obj["labels"]; present && l != nil {
			lm, ok := l.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("events[%d].labels: must be an object, got %s", i, processor.JSONType(l))
			}
			e.labels = lm
			e.key = seriesKey(lm)
		}
		if s, present := obj["signals"]; present && s != nil {
			sl, ok := s.([]any)
			if !ok {
				return nil, fmt.Errorf("events[%d].signals: must be a list of strings, got %s", i, processor.JSONType(s))
			}
			for j, x := range sl {
				name, ok := x.(string)
				if !ok {
					return nil, fmt.Errorf("events[%d].signals[%d]: must be a string, got %s", i, j, processor.JSONType(x))
				}
				e.signals = append(e.signals, name)
			}
		}
		if v, present := obj["score"]; present && v != nil {
			f, err := processor.Number(v)
			if err != nil {
				return nil, fmt.Errorf("events[%d].score: %w", i, err)
			}
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				e.score, e.hasScore = f, true
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// timeOf reads a required time field: unix seconds or an RFC 3339 time.
func timeOf(obj map[string]any, key string) (float64, error) {
	v, present := obj[key]
	if !present || v == nil {
		return 0, fmt.Errorf("required (unix seconds or an RFC 3339 time)")
	}
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			return float64(t.UnixNano()) / 1e9, nil
		}
	}
	f, err := processor.Number(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		if s, ok := v.(string); ok {
			return 0, fmt.Errorf("expected unix seconds or an RFC 3339 time, got %q", s)
		}
		return 0, fmt.Errorf("expected unix seconds or an RFC 3339 time, got %v", v)
	}
	return f, nil
}

// seriesKey is the canonical form of a label set: JSON with sorted keys, so
// two objects with the same labels in any order share a key and different
// label sets never collide.
func seriesKey(labels map[string]any) string {
	b, err := json.Marshal(labels)
	if err != nil {
		return fmt.Sprint(labels)
	}
	return string(b)
}

// cluster links events that overlap in time (within gap, and sharing a
// signal when required) and returns the connected components, each as the
// indices of its events in start order; components come in the order of
// their first event.
func cluster(ctx context.Context, events []event, st *settings) ([][]int, error) {
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return events[order[a]].start < events[order[b]].start })
	uf := newUnionFind(len(events))
	for a := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ea := &events[order[a]]
		for b := a + 1; b < len(order); b++ {
			eb := &events[order[b]]
			if eb.start > ea.end+st.gap {
				break // starts are sorted: nothing later can touch ea
			}
			if st.sameSignal && !shareSignal(ea.signals, eb.signals) {
				continue
			}
			uf.union(order[a], order[b])
		}
	}
	byRoot := map[int][]int{}
	var roots []int
	for _, i := range order {
		r := uf.find(i)
		if _, seen := byRoot[r]; !seen {
			roots = append(roots, r)
		}
		byRoot[r] = append(byRoot[r], i)
	}
	out := make([][]int, 0, len(roots))
	for _, r := range roots {
		out = append(out, byRoot[r])
	}
	return out, nil
}

func shareSignal(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// unionFind is a disjoint-set forest with path halving.
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

// group is one connected component with its summary.
type group struct {
	members      []int // event indices in start order
	start, end   float64
	seriesKeys   []string         // distinct series in order of first appearance
	seriesLabels []map[string]any // labels of seriesKeys[i]
	signalSeries map[string]map[string]bool
	score        float64
	hasScore     bool
}

func newGroup(events []event, members []int) *group {
	g := &group{members: members, start: math.Inf(1), end: math.Inf(-1), signalSeries: map[string]map[string]bool{}}
	seen := map[string]bool{}
	for _, i := range members {
		e := events[i]
		g.start = math.Min(g.start, e.start)
		g.end = math.Max(g.end, e.end)
		if !seen[e.key] {
			seen[e.key] = true
			g.seriesKeys = append(g.seriesKeys, e.key)
			g.seriesLabels = append(g.seriesLabels, e.labels)
		}
		for _, s := range e.signals {
			if g.signalSeries[s] == nil {
				g.signalSeries[s] = map[string]bool{}
			}
			g.signalSeries[s][e.key] = true
		}
		if e.hasScore && (!g.hasScore || e.score > g.score) {
			g.score, g.hasScore = e.score, true
		}
	}
	return g
}

// capClusters keeps the max strongest clusters (most series, then most
// events, then earliest) and returns them in time order; max 0 = all.
func capClusters(gs []*group, max int) []*group {
	out := append([]*group(nil), gs...)
	if max > 0 && len(out) > max {
		sort.SliceStable(out, func(a, b int) bool {
			if len(out[a].seriesKeys) != len(out[b].seriesKeys) {
				return len(out[a].seriesKeys) > len(out[b].seriesKeys)
			}
			if len(out[a].members) != len(out[b].members) {
				return len(out[a].members) > len(out[b].members)
			}
			return out[a].start < out[b].start
		})
		out = out[:max]
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].start < out[b].start })
	return out
}

func (g *group) toAny(events []event) map[string]any {
	seriesOut := make([]any, 0, len(g.seriesLabels))
	for _, l := range g.seriesLabels {
		if l == nil {
			seriesOut = append(seriesOut, map[string]any{})
		} else {
			seriesOut = append(seriesOut, l)
		}
	}
	type sig struct {
		name  string
		count int
	}
	sigs := make([]sig, 0, len(g.signalSeries))
	for name, set := range g.signalSeries {
		sigs = append(sigs, sig{name, len(set)})
	}
	sort.Slice(sigs, func(a, b int) bool {
		if sigs[a].count != sigs[b].count {
			return sigs[a].count > sigs[b].count
		}
		return sigs[a].name < sigs[b].name
	})
	sigOut := make([]any, 0, len(sigs))
	for _, s := range sigs {
		sigOut = append(sigOut, map[string]any{"signal": s.name, "series": s.count})
	}
	eventsOut := make([]any, 0, len(g.members))
	for _, i := range g.members {
		eventsOut = append(eventsOut, eventToAny(events[i]))
	}
	var score any
	if g.hasScore {
		score = g.score
	}
	return map[string]any{
		"start":        g.start,
		"end":          g.end,
		"start_time":   series.FormatTime(g.start),
		"end_time":     series.FormatTime(g.end),
		"duration":     g.end - g.start,
		"series_count": len(g.seriesKeys),
		"event_count":  len(g.members),
		"score":        score,
		"series":       seriesOut,
		"signals":      sigOut,
		"events":       eventsOut,
	}
}

// eventToAny copies the event with its times in both forms; the input is
// left untouched.
func eventToAny(e event) map[string]any {
	out := make(map[string]any, len(e.raw)+4)
	for k, v := range e.raw {
		out[k] = v
	}
	out["start"] = e.start
	out["end"] = e.end
	out["start_time"] = series.FormatTime(e.start)
	out["end_time"] = series.FormatTime(e.end)
	return out
}
