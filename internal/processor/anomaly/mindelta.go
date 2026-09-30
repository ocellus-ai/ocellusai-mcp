package anomaly

import (
	"fmt"
	"math"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// MinDelta is the absolute significance filter shared by the anomaly and
// outliers processors (with.min_delta, with.min_rel_delta): a flagged sample
// is kept only when it deviates from the detector's expected value by at
// least max(Abs, Rel*|expected|), in the metric's own units. The detectors
// measure deviations in units of the series' own spread, which collapses on
// nearly constant or nearly idle series (a VM at 0.2% CPU is anomalous at
// 0.3%); the filter states which change is worth reporting at all. It is
// applied before max_anomalies, so the cap is not spent on such blips.
type MinDelta struct {
	Abs float64 // with.min_delta, metric units; 0 = off
	Rel float64 // with.min_rel_delta, a fraction of |expected|; 0 = off
}

// MinDeltaKeys are the `with` keys MinDelta owns.
var MinDeltaKeys = []string{"min_delta", "min_rel_delta"}

// ParseMinDelta reads min_delta and min_rel_delta (both >= 0, default 0).
// At load time a value that is still a template is skipped.
func ParseMinDelta(with map[string]any, load bool) (MinDelta, error) {
	var md MinDelta
	for _, k := range []struct {
		key string
		dst *float64
	}{{"min_delta", &md.Abs}, {"min_rel_delta", &md.Rel}} {
		if load && processor.IsTemplate(with[k.key]) {
			continue
		}
		v, err := processor.Float(with, k.key, 0)
		if err != nil {
			return MinDelta{}, err
		}
		if v < 0 {
			return MinDelta{}, fmt.Errorf("with.%s: must be >= 0 (0 = off), got %v", k.key, v)
		}
		*k.dst = v
	}
	return md, nil
}

// Active reports whether the filter drops anything.
func (m MinDelta) Active() bool { return m.Abs > 0 || m.Rel > 0 }

// Filter keeps the anomalies of s whose sample moved enough from its expected
// value and returns how many it dropped.
func (m MinDelta) Filter(in []Anomaly, s Series) (kept []Anomaly, dropped int) {
	if !m.Active() {
		return in, 0
	}
	kept = in[:0:0]
	for _, a := range in {
		if math.Abs(s.Points[a.Index].V-a.Expected) >= math.Max(m.Abs, m.Rel*math.Abs(a.Expected)) {
			kept = append(kept, a)
		}
	}
	return kept, len(in) - len(kept)
}
