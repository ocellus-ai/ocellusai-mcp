package anomaly

import (
	"fmt"
	"math"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// zscore flags samples whose distance from the mean exceeds `threshold`
// population standard deviations. Simple and fast, but a large spike inflates
// the standard deviation and can mask smaller ones; prefer iqr on noisy data.
//
// Params: threshold (> 0, default 3).
// Stats: mean, stddev, threshold. Score: |z| = |v - mean| / stddev.
// A series with zero standard deviation has no anomalies.
type zscore struct{}

func (zscore) Name() string { return "zscore" }

func (zscore) Validate(params map[string]any) error {
	if err := processor.CheckKeys(params, "threshold"); err != nil {
		return err
	}
	if processor.IsTemplate(params["threshold"]) {
		return nil
	}
	_, err := zscoreThreshold(params)
	return err
}

func zscoreThreshold(params map[string]any) (float64, error) {
	t, err := processor.Float(params, "threshold", 3)
	if err != nil {
		return 0, err
	}
	if t <= 0 {
		return 0, fmt.Errorf("with.threshold: must be > 0, got %v", t)
	}
	return t, nil
}

func (zscore) Detect(params map[string]any, s Series) (Verdict, error) {
	t, err := zscoreThreshold(params)
	if err != nil {
		return Verdict{}, err
	}
	v := values(s)
	m := mean(v)
	sd := stddev(v, m)
	out := Verdict{Stats: map[string]float64{"mean": m, "stddev": sd, "threshold": t}}
	if sd == 0 {
		return out, nil
	}
	for i, x := range v {
		z := (x - m) / sd
		if math.Abs(z) > t {
			out.Anomalies = append(out.Anomalies, Anomaly{Index: i, Score: math.Abs(z), Expected: m, Direction: direction(x, m)})
		}
	}
	return out, nil
}
