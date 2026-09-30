// Package anomalymv is the `anomaly_mv` processor: multivariate anomaly
// detection over series whose samples are vectors (the CPU and memory of a
// VM at the same instant). A detector scores every sample as a whole rather
// than each dimension on its own, so it catches a sample that is ordinary
// on every axis but breaks the usual relation between them.
//
// Input: the multivariate series form of package series,
//
//	{"dims": ["cpu", "mem"], "series": [{"labels": {...}, "points": [[ts, v1, v2], ...], "reason": "..."}]}
//
// as produced by the `join` transform from several Prometheus matrices, or
// by any other transform or fn: jq step. At least two dims are required. A
// series carrying a reason (an object join could not build) is reported as
// skipped with that reason, like a series shorter than min_points.
//
// The processor owns the common settings (method, min_points,
// max_anomalies, min_delta, min_rel_delta) and the report skeleton; the
// maths lives in Detector implementations (mahalanobis.go). Single-metric
// detection stays in the `anomaly` processor.
//
// min_delta (a number for every dim or an object {dim: number}) and
// min_rel_delta (a fraction of each dim's mean) name the smallest change
// worth reporting, in the metric's units: unit = max(min_delta,
// min_rel_delta*|mean|). A quarter of it (processor.NoiseFloorShare) is the
// noise floor handed to the detector, which assumes at least that spread in
// every dimension, so a nearly constant dimension cannot turn a negligible
// move into a huge distance. Both default to 0 (no floor).
//
// Output:
//
//	{method, dims, series_total, series_analyzed, series_skipped, total_anomalies,
//	 series: [{labels, points, skipped, [reason], [stats], [noise_floor{dim}], anomaly_count,
//	           anomalies: [{timestamp, time, values{dim}, expected{dim}, deviation{dim}, dominant, score}]}]}
package anomalymv

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: anomaly_mv).
const Name = "anomaly_mv"

// Defaults of the common settings.
const (
	DefaultMinPoints    = 10
	DefaultMaxAnomalies = 20
)

// Processor implements processor.Processor.
type Processor struct{}

// New creates the multivariate anomaly processor.
func New() *Processor { return &Processor{} }

// Name returns "anomaly_mv".
func (*Processor) Name() string { return Name }

// commonKeys are owned by the processor; every other `with` key belongs to the detector.
var commonKeys = []string{"method", "min_points", "max_anomalies", "min_delta", "min_rel_delta"}

type settings struct {
	method       string
	detector     Detector
	minPoints    int
	maxAnomalies int
	minDelta     processor.DimFloats
	minRelDelta  float64
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
	st := &settings{method: method, detector: det, minPoints: DefaultMinPoints, maxAnomalies: DefaultMaxAnomalies}
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
	if !deferred("max_anomalies") {
		st.maxAnomalies, err = processor.Int(with, "max_anomalies", DefaultMaxAnomalies)
		if err != nil {
			return nil, err
		}
		if st.maxAnomalies < 0 {
			return nil, fmt.Errorf("with.max_anomalies: must be >= 0 (0 = unlimited), got %d", st.maxAnomalies)
		}
	}
	if st.minDelta, err = processor.ParseDimFloats(with, "min_delta", load); err != nil {
		return nil, err
	}
	if !deferred("min_rel_delta") {
		if st.minRelDelta, err = processor.Float(with, "min_rel_delta", 0); err != nil {
			return nil, err
		}
		if st.minRelDelta < 0 {
			return nil, fmt.Errorf("with.min_rel_delta: must be >= 0 (0 = off), got %v", st.minRelDelta)
		}
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

// Run analyses every series of a multivariate set and returns the report
// described in the package documentation.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	m, err := series.ParseMulti(data)
	if err != nil {
		return nil, err
	}
	if len(m.Dims) < 2 {
		return nil, fmt.Errorf("need at least 2 dims to detect multivariate anomalies, got dims [%s] (join at least two inputs)", strings.Join(m.Dims, ", "))
	}
	if err := st.minDelta.Check("min_delta", m.Dims); err != nil {
		return nil, err
	}

	report := make([]any, 0, len(m.Series))
	analyzed, skipped, total := 0, 0, 0
	skip := func(entry map[string]any, reason string) {
		skipped++
		entry["skipped"] = true
		entry["reason"] = reason
		report = append(report, entry)
	}
	for _, s := range m.Series {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := map[string]any{
			"labels":        series.LabelsToAny(s.Labels),
			"points":        len(s.Points),
			"skipped":       false,
			"anomaly_count": 0,
			"anomalies":     []any{},
		}
		if s.Reason != "" {
			skip(entry, s.Reason)
			continue
		}
		if len(s.Points) < st.minPoints {
			skip(entry, fmt.Sprintf("fewer than %d points (min_points)", st.minPoints))
			continue
		}
		floor := st.floor(s)
		if floor != nil {
			entry["noise_floor"] = byDim(s.Dims, floor)
		}
		verdict, err := st.detector.Detect(st.params, s, floor)
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", st.method, err)
		}
		if verdict.Stats != nil {
			entry["stats"] = statsToAny(verdict.Stats)
		}
		if verdict.Skip != "" {
			skip(entry, verdict.Skip)
			continue
		}
		analyzed++
		total += len(verdict.Anomalies)
		entry["anomaly_count"] = len(verdict.Anomalies)
		entry["anomalies"] = anomaliesToAny(s, capBySeverity(verdict.Anomalies, st.maxAnomalies))
		report = append(report, entry)
	}
	return map[string]any{
		"method":          st.method,
		"dims":            toAnyList(m.Dims),
		"series_total":    len(report),
		"series_analyzed": analyzed,
		"series_skipped":  skipped,
		"total_anomalies": total,
		"series":          report,
	}, nil
}

// floor returns the per-dim noise floor of s (NoiseFloorShare of the
// smallest significant change), or nil when min_delta and min_rel_delta are
// both unset.
func (st *settings) floor(s Series) []float64 {
	mu := means(s)
	out := make([]float64, len(s.Dims))
	set := false
	for i, dim := range s.Dims {
		unit := math.Max(st.minDelta.Of(dim), st.minRelDelta*math.Abs(mu[i]))
		out[i] = processor.NoiseFloorShare * unit
		set = set || out[i] > 0
	}
	if !set {
		return nil
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

func anomaliesToAny(s Series, in []Anomaly) []any {
	out := make([]any, 0, len(in))
	for _, a := range in {
		p := s.Points[a.Index]
		dominant, dominantDev := "", 0.0
		for i, d := range a.Deviation {
			if math.Abs(d) > math.Abs(dominantDev) || dominant == "" {
				dominant, dominantDev = s.Dims[i], d
			}
		}
		out = append(out, map[string]any{
			"timestamp": p.T,
			"time":      series.FormatTime(p.T),
			"values":    byDim(s.Dims, p.V),
			"expected":  byDim(s.Dims, a.Expected),
			"deviation": byDim(s.Dims, a.Deviation),
			"dominant":  dominant,
			"score":     a.Score,
		})
	}
	return out
}

func byDim(dims []string, vals []float64) map[string]any {
	out := make(map[string]any, len(dims))
	for i, d := range dims {
		if i < len(vals) {
			out[d] = vals[i]
		}
	}
	return out
}

func statsToAny(stats map[string]float64) map[string]any {
	out := make(map[string]any, len(stats))
	for k, v := range stats {
		out[k] = v
	}
	return out
}

func toAnyList(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
