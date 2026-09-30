package anomaly

import (
	"fmt"
	"sort"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// iqr flags samples outside the Tukey fences [q1 - k*IQR, q3 + k*IQR].
// Robust to the outliers themselves because quartiles ignore extreme values.
//
// Params: k (> 0, default 1.5; 3 is the classic "far out" setting).
// Stats: q1, median, q3, iqr, lower, upper, k. Expected: median.
// Score: distance beyond the fence in IQR units. When IQR is zero (a flat
// series) the fences collapse onto the quartiles, every different sample is an
// anomaly and the score is the raw distance beyond the fence instead.
type iqr struct{}

func (iqr) Name() string { return "iqr" }

func (iqr) Validate(params map[string]any) error {
	if err := processor.CheckKeys(params, "k"); err != nil {
		return err
	}
	if processor.IsTemplate(params["k"]) {
		return nil
	}
	_, err := iqrK(params)
	return err
}

func iqrK(params map[string]any) (float64, error) {
	k, err := processor.Float(params, "k", 1.5)
	if err != nil {
		return 0, err
	}
	if k <= 0 {
		return 0, fmt.Errorf("with.k: must be > 0, got %v", k)
	}
	return k, nil
}

func (iqr) Detect(params map[string]any, s Series) (Verdict, error) {
	k, err := iqrK(params)
	if err != nil {
		return Verdict{}, err
	}
	v := values(s)
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	q1 := quantile(sorted, 0.25)
	med := quantile(sorted, 0.5)
	q3 := quantile(sorted, 0.75)
	spread := q3 - q1
	lower := q1 - k*spread
	upper := q3 + k*spread
	out := Verdict{Stats: map[string]float64{
		"q1": q1, "median": med, "q3": q3, "iqr": spread, "lower": lower, "upper": upper, "k": k,
	}}
	score := func(dist float64) float64 {
		if spread > 0 {
			return dist / spread
		}
		return dist
	}
	for i, x := range v {
		switch {
		case x > upper:
			out.Anomalies = append(out.Anomalies, Anomaly{Index: i, Score: score(x - upper), Expected: med, Direction: DirectionUp})
		case x < lower:
			out.Anomalies = append(out.Anomalies, Anomaly{Index: i, Score: score(lower - x), Expected: med, Direction: DirectionDown})
		}
	}
	return out, nil
}
