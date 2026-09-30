// Package anomaly is the `anomaly` processor: it runs an outlier detector over
// every series of a scalar time-series set and reports the anomalous samples.
//
// Input: the scalar series form, a Prometheus range result (matrix); see
// package series. Any other source is shaped into it by a preceding fn: jq
// step. Output: a report, so the step ends the chain (or feeds response.jq).
//
// The processor owns the common settings (method, min_points, direction,
// max_anomalies, min_delta, min_rel_delta), the input parsing and the output
// skeleton; the maths lives in Detector implementations (zscore.go, iqr.go).
package anomaly

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: anomaly).
const Name = "anomaly"

// Defaults of the common settings.
const (
	DefaultMinPoints    = 10
	DefaultMaxAnomalies = 20
)

// Processor implements processor.Processor.
type Processor struct{}

// New creates the anomaly processor.
func New() *Processor { return &Processor{} }

// Name returns "anomaly".
func (*Processor) Name() string { return Name }

// commonKeys are owned by the processor; every other `with` key belongs to the detector.
var commonKeys = append([]string{"method", "min_points", "direction", "max_anomalies"}, MinDeltaKeys...)

type settings struct {
	method       string
	detector     Detector
	minPoints    int
	direction    string
	maxAnomalies int
	minDelta     MinDelta
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
		return nil, fmt.Errorf("with.method: required (one of: %s)", strings.Join(Detectors(), ", "))
	}
	if processor.IsTemplate(method) {
		return nil, fmt.Errorf("with.method: must be a literal, templates are not allowed (got %q)", method)
	}
	det, ok := detectors[method]
	if !ok {
		return nil, fmt.Errorf("with.method: unknown detector %q (available: %s)", method, strings.Join(Detectors(), ", "))
	}
	st := &settings{method: method, detector: det, minPoints: DefaultMinPoints, direction: DirectionBoth, maxAnomalies: DefaultMaxAnomalies}
	// deferred reports a key whose value is only known after rendering.
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }

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
		case DirectionBoth, DirectionUp, DirectionDown:
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
	if st.minDelta, err = ParseMinDelta(with, load); err != nil {
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

func isCommon(k string) bool {
	for _, c := range commonKeys {
		if k == c {
			return true
		}
	}
	return false
}

// Run analyses a scalar series set (a Prometheus matrix) and returns the
// anomaly report, one entry per series.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	all, err := series.ParseMatrix(data)
	if err != nil {
		return nil, err
	}
	report := make([]any, 0, len(all))
	analyzed, skipped, total := 0, 0, 0
	for _, s := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := map[string]any{
			"metric":        series.LabelsToAny(s.Labels),
			"points":        len(s.Points),
			"skipped":       false,
			"anomaly_count": 0,
			"anomalies":     []any{},
		}
		if len(s.Points) < st.minPoints {
			skipped++
			entry["skipped"] = true
			entry["reason"] = fmt.Sprintf("fewer than %d points (min_points)", st.minPoints)
			report = append(report, entry)
			continue
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
		entry["stats"] = statsToAny(s, verdict.Stats)
		entry["anomalies"] = anomaliesToAny(s, capBySeverity(anomalies, st.maxAnomalies))
		if verdict.Expected != nil {
			entry["expected_values"] = expectedToAny(s, verdict.Expected)
		}
		report = append(report, entry)
	}
	return map[string]any{
		"method":          st.method,
		"series_total":    len(all),
		"series_analyzed": analyzed,
		"series_skipped":  skipped,
		"total_anomalies": total,
		"series":          report,
	}, nil
}

func filterDirection(in []Anomaly, dir string) []Anomaly {
	if dir == DirectionBoth {
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

// capBySeverity keeps the max most severe anomalies (0 = unlimited) in time order.
func capBySeverity(in []Anomaly, max int) []Anomaly {
	if max <= 0 || len(in) <= max {
		return in
	}
	sorted := append([]Anomaly(nil), in...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	sorted = sorted[:max]
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	return sorted
}

func statsToAny(s Series, stats map[string]float64) map[string]any {
	out := make(map[string]any, len(stats)+2)
	for k, v := range stats {
		out[k] = v
	}
	lo, hi := minMax(values(s))
	if _, ok := out["min"]; !ok {
		out["min"] = lo
	}
	if _, ok := out["max"]; !ok {
		out["max"] = hi
	}
	return out
}

func anomaliesToAny(s Series, in []Anomaly) []any {
	out := make([]any, 0, len(in))
	for _, a := range in {
		p := s.Points[a.Index]
		out = append(out, map[string]any{
			"timestamp": p.T,
			"time":      series.FormatTime(p.T),
			"value":     p.V,
			"score":     a.Score,
			"expected":  a.Expected,
			"direction": a.Direction,
		})
	}
	return out
}

func expectedToAny(s Series, expected []float64) []any {
	out := make([]any, 0, len(expected))
	for i, e := range expected {
		if i >= len(s.Points) {
			break
		}
		out = append(out, []any{s.Points[i].T, e})
	}
	return out
}
