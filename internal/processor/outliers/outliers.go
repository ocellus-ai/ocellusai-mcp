// Package outliers is the `outliers` processor: an outlier detector over a
// set of keyed values that have no time axis — filesystems and their usage,
// pods and their restart counts, the samples of an instant Prometheus vector,
// rows of a REST response. Every item is compared with the other items of
// its series, not with its own history; the time-series counterpart is the
// `anomaly` processor and both share the detectors (zscore, iqr).
//
// Input: a list of items in the canonical form
//
//	[{"key": "/data", "value": 97, "labels": {"host": "a"}}, ...]
//
// value is required (a number or a numeric string); key is an optional
// scalar that names the item in the report; labels is an optional object of
// scalars used by group_by. Other fields are ignored. Any worker result is
// brought into this form by a preceding `fn: jq` step. Non-finite values
// (NaN, ±Inf) are dropped; a missing or non-numeric value is an error.
//
// with:
//
//	method         zscore | iqr, required, a literal. Detector-specific keys
//	               (threshold, k) are passed through to the detector.
//	group_by       label name(s); items are split into one series per
//	               distinct combination of these labels. Default: none, all
//	               items form one series.
//	min_points     series with fewer items are skipped (default 5).
//	direction      both | up | down (default both).
//	max_anomalies  strongest anomalies to report per series, 0 = all
//	               (default 20), most severe first.
//	min_delta      smallest deviation from expected worth reporting, in the
//	               value's units (default 0 = off); applied before the cap.
//	min_rel_delta  the same as a fraction of |expected| (default 0); the
//	               larger of the two applies (anomaly.MinDelta).
//
// Output:
//
//	{method, group_by, series_total, series_analyzed, series_skipped,
//	 total_anomalies, series: [{labels, points, skipped, [reason], [stats],
//	 anomaly_count, [below_min_delta], anomalies: [{index, key, value, score,
//	 expected, direction}]}]}
//
// below_min_delta (present when min_delta or min_rel_delta is set) counts the
// flagged items dropped as too close to expected; anomaly_count excludes them.
//
// index is the item's position in the input list. Without group_by the
// report always holds exactly one series. Small sets deserve care: the
// z-score of n items can never exceed (n-1)/sqrt(n), so with six items a
// threshold of 3 never fires; iqr is the safer method for a handful of items.
package outliers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/anomaly"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: outliers).
const Name = "outliers"

// Defaults of the common settings.
const (
	DefaultMinPoints    = 5
	DefaultMaxAnomalies = 20
)

// Processor implements processor.Processor.
type Processor struct{}

// New creates the outliers processor.
func New() *Processor { return &Processor{} }

// Name returns "outliers".
func (*Processor) Name() string { return Name }

// commonKeys are owned by the processor; every other `with` key belongs to the detector.
var commonKeys = append([]string{"method", "group_by", "min_points", "direction", "max_anomalies"}, anomaly.MinDeltaKeys...)

type settings struct {
	method       string
	detector     anomaly.Detector
	groupBy      []string
	minPoints    int
	direction    string
	maxAnomalies int
	minDelta     anomaly.MinDelta
	// params holds the detector-specific keys.
	params map[string]any
}

// Validate checks the raw `with` section at catalog load time.
func (*Processor) Validate(with map[string]any) error {
	st, err := parseSettings(with, true)
	if err != nil {
		return err
	}
	if err := st.detector.Validate(st.params); err != nil {
		return fmt.Errorf("method %s: %w", st.method, err)
	}
	return nil
}

// parseSettings reads the common keys. At load time values that are still
// templates are skipped; at run time everything must be a final value.
func parseSettings(with map[string]any, load bool) (*settings, error) {
	method, err := processor.String(with, "method")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(method) == "" {
		return nil, fmt.Errorf("with.method: required (one of: %s)", strings.Join(anomaly.Detectors(), ", "))
	}
	if processor.IsTemplate(method) {
		return nil, fmt.Errorf("with.method: must be a literal, templates are not allowed (got %q)", method)
	}
	det, ok := anomaly.Lookup(method)
	if !ok {
		return nil, fmt.Errorf("with.method: unknown detector %q (available: %s)", method, strings.Join(anomaly.Detectors(), ", "))
	}
	st := &settings{method: method, detector: det, minPoints: DefaultMinPoints, direction: anomaly.DirectionBoth, maxAnomalies: DefaultMaxAnomalies}
	// deferred reports a key whose value is only known after rendering.
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }

	st.groupBy, err = stringList(with, "group_by", load)
	if err != nil {
		return nil, err
	}
	if dup := duplicate(st.groupBy); dup != "" {
		return nil, fmt.Errorf("with.group_by: duplicate label %q", dup)
	}
	if !deferred("min_points") {
		st.minPoints, err = processor.Int(with, "min_points", DefaultMinPoints)
		if err != nil {
			return nil, err
		}
		if st.minPoints < 1 {
			return nil, fmt.Errorf("with.min_points: must be >= 1, got %d", st.minPoints)
		}
	}
	if !deferred("direction") {
		dir, err := processor.String(with, "direction")
		if err != nil {
			return nil, err
		}
		if dir != "" {
			st.direction = dir
		}
		switch st.direction {
		case anomaly.DirectionBoth, anomaly.DirectionUp, anomaly.DirectionDown:
		default:
			return nil, fmt.Errorf("with.direction: %q is not one of both, up, down", st.direction)
		}
	}
	if !deferred("max_anomalies") {
		st.maxAnomalies, err = processor.Int(with, "max_anomalies", DefaultMaxAnomalies)
		if err != nil {
			return nil, err
		}
		if st.maxAnomalies < 0 {
			return nil, fmt.Errorf("with.max_anomalies: must be >= 0 (0 = unlimited), got %d", st.maxAnomalies)
		}
	}
	if st.minDelta, err = anomaly.ParseMinDelta(with, load); err != nil {
		return nil, err
	}
	st.params = make(map[string]any, len(with))
	for k, v := range with {
		if !isCommon(k) {
			st.params[k] = v
		}
	}
	return st, nil
}

// stringList reads an optional list of strings (a single string is a list of
// one). At load time a templated element makes the whole list unknown (nil).
func stringList(with map[string]any, key string, load bool) ([]string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return nil, nil
	}
	var items []any
	switch x := v.(type) {
	case string:
		items = []any{x}
	case []any:
		items = x
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("with.%s: must be a list of strings, got %s", key, processor.JSONType(v))
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("with.%s[%d]: must be a string, got %s", key, i, processor.JSONType(it))
		}
		if processor.IsTemplate(s) {
			if load {
				return nil, nil
			}
			return nil, fmt.Errorf("with.%s[%d]: unrendered template %q", key, i, s)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("with.%s[%d]: must not be empty", key, i)
		}
		out = append(out, s)
	}
	return out, nil
}

func duplicate(list []string) string {
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		if seen[s] {
			return s
		}
		seen[s] = true
	}
	return ""
}

func isCommon(k string) bool {
	for _, c := range commonKeys {
		if k == c {
			return true
		}
	}
	return false
}

// item is one parsed input element.
type item struct {
	index  int
	key    any // nil when absent, otherwise the scalar as given
	value  float64
	labels map[string]string
}

// group is one series: the items sharing the group_by labels.
type group struct {
	labels map[string]string
	items  []item
}

// Run splits the items into series, runs the detector over every series with
// enough items and returns the report described in the package documentation.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	items, err := parseItems(data)
	if err != nil {
		return nil, err
	}
	groups, unlabeled, missing := groupItems(items, st.groupBy)

	report := make([]any, 0, len(groups)+1)
	analyzed, skipped, total := 0, 0, 0
	if len(unlabeled) > 0 {
		skipped++
		entry := newEntry(map[string]string{}, len(unlabeled))
		entry["skipped"] = true
		entry["reason"] = fmt.Sprintf("missing group_by label(s) %s", strings.Join(missing, ", "))
		report = append(report, entry)
	}
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := newEntry(g.labels, len(g.items))
		if len(g.items) < st.minPoints {
			skipped++
			entry["skipped"] = true
			entry["reason"] = fmt.Sprintf("fewer than %d items (min_points)", st.minPoints)
			report = append(report, entry)
			continue
		}
		s := series.Series{Labels: g.labels, Points: make([]series.Point, len(g.items))}
		for i, it := range g.items {
			s.Points[i] = series.Point{T: float64(it.index), V: it.value}
		}
		verdict, err := st.detector.Detect(st.params, s)
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", st.method, err)
		}
		analyzed++
		anomalies := filterDirection(verdict.Anomalies, st.direction)
		if st.minDelta.Active() {
			var below int
			anomalies, below = st.minDelta.Filter(anomalies, s)
			entry["below_min_delta"] = below
		}
		total += len(anomalies)
		entry["anomaly_count"] = len(anomalies)
		entry["stats"] = statsToAny(g.items, verdict.Stats)
		entry["anomalies"] = anomaliesToAny(g.items, capBySeverity(anomalies, st.maxAnomalies))
		report = append(report, entry)
	}
	return map[string]any{
		"method":          st.method,
		"group_by":        toAnyList(st.groupBy),
		"series_total":    len(report),
		"series_analyzed": analyzed,
		"series_skipped":  skipped,
		"total_anomalies": total,
		"series":          report,
	}, nil
}

// parseItems reads the canonical input list. Non-finite values are dropped;
// every other defect is an error naming the item.
func parseItems(data any) ([]item, error) {
	list, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list of items [{key, value, labels?}, ...] (shape the data with a preceding fn: jq step), got %s", processor.JSONType(data))
	}
	out := make([]item, 0, len(list))
	for i, raw := range list {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("items[%d]: expected an object {key, value, labels?}, got %s", i, processor.JSONType(raw))
		}
		it := item{index: i, labels: map[string]string{}}
		if k, present := obj["key"]; present && k != nil {
			switch k.(type) {
			case string, bool, int, int64, int32, float64, float32:
				it.key = k
			default:
				return nil, fmt.Errorf("items[%d].key: must be a scalar, got %s", i, processor.JSONType(k))
			}
		}
		where := fmt.Sprintf("items[%d]", i)
		if it.key != nil {
			where = fmt.Sprintf("items[%d] (key %v)", i, it.key)
		}
		v, present := obj["value"]
		if !present || v == nil {
			return nil, fmt.Errorf("%s.value: required", where)
		}
		f, err := processor.Number(v)
		if err != nil {
			return nil, fmt.Errorf("%s.value: %w", where, err)
		}
		if l, present := obj["labels"]; present && l != nil {
			lm, ok := l.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s.labels: must be an object, got %s", where, processor.JSONType(l))
			}
			for k, lv := range lm {
				it.labels[k] = stringify(lv)
			}
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		it.value = f
		out = append(out, it)
	}
	return out, nil
}

// groupItems splits items by the group_by labels. Items lacking one of the
// labels are returned separately together with the sorted set of missing
// label names. Without group_by every item lands in a single series.
func groupItems(items []item, groupBy []string) (groups []group, unlabeled []item, missing []string) {
	if len(groupBy) == 0 {
		return []group{{labels: map[string]string{}, items: items}}, nil, nil
	}
	byKey := map[string]*group{}
	var keys []string
	missingSet := map[string]bool{}
	for _, it := range items {
		parts := make([]string, 0, len(groupBy))
		complete := true
		for _, l := range groupBy {
			v, ok := it.labels[l]
			if !ok {
				missingSet[l] = true
				complete = false
				continue
			}
			parts = append(parts, v)
		}
		if !complete {
			unlabeled = append(unlabeled, it)
			continue
		}
		key := strings.Join(parts, "\x1f")
		g, ok := byKey[key]
		if !ok {
			labels := make(map[string]string, len(groupBy))
			for i, l := range groupBy {
				labels[l] = parts[i]
			}
			g = &group{labels: labels}
			byKey[key] = g
			keys = append(keys, key)
		}
		g.items = append(g.items, it)
	}
	sort.Strings(keys)
	groups = make([]group, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, *byKey[k])
	}
	for l := range missingSet {
		missing = append(missing, l)
	}
	sort.Strings(missing)
	return groups, unlabeled, missing
}

func filterDirection(in []anomaly.Anomaly, dir string) []anomaly.Anomaly {
	if dir == anomaly.DirectionBoth {
		return in
	}
	out := in[:0:0]
	for _, a := range in {
		if a.Direction == dir {
			out = append(out, a)
		}
	}
	return out
}

// capBySeverity orders anomalies by descending score (ties by input order)
// and keeps the first max of them (0 = unlimited).
func capBySeverity(in []anomaly.Anomaly, max int) []anomaly.Anomaly {
	sorted := append([]anomaly.Anomaly(nil), in...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return sorted[i].Index < sorted[j].Index
	})
	if max > 0 && len(sorted) > max {
		sorted = sorted[:max]
	}
	return sorted
}

func newEntry(labels map[string]string, points int) map[string]any {
	return map[string]any{
		"labels":        series.LabelsToAny(labels),
		"points":        points,
		"skipped":       false,
		"anomaly_count": 0,
		"anomalies":     []any{},
	}
}

func statsToAny(items []item, stats map[string]float64) map[string]any {
	out := make(map[string]any, len(stats)+2)
	for k, v := range stats {
		out[k] = v
	}
	lo, hi := items[0].value, items[0].value
	for _, it := range items[1:] {
		lo = math.Min(lo, it.value)
		hi = math.Max(hi, it.value)
	}
	if _, ok := out["min"]; !ok {
		out["min"] = lo
	}
	if _, ok := out["max"]; !ok {
		out["max"] = hi
	}
	return out
}

func anomaliesToAny(items []item, in []anomaly.Anomaly) []any {
	out := make([]any, 0, len(in))
	for _, a := range in {
		it := items[a.Index]
		out = append(out, map[string]any{
			"index":     it.index,
			"key":       it.key,
			"value":     it.value,
			"score":     a.Score,
			"expected":  a.Expected,
			"direction": a.Direction,
		})
	}
	return out
}

func toAnyList(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
