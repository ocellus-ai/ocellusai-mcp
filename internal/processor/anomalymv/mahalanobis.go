package anomalymv

import (
	"fmt"
	"math"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// DefaultThreshold is the default Mahalanobis distance above which a sample is anomalous.
const DefaultThreshold = 3.0

// mahalanobis scores every sample by its Mahalanobis distance from the series'
// mean vector: d = sqrt((x-μ)ᵀ Σ⁻¹ (x-μ)) with Σ the population covariance of
// the series. Unlike a per-metric z-score it accounts for the correlation
// between the inputs, so a sample that is ordinary on every axis but breaks
// their usual relation still scores high. A sample is anomalous when d exceeds
// `threshold` (default 3). A singular covariance (a constant or perfectly
// collinear input) cannot be scored and the series is reported as skipped.
//
// A noise floor s (see Detector.Detect) is added to the diagonal: Σ + diag(s²)
// is exactly the covariance the series would have with independent noise of
// that size added to every dimension, so a dimension whose own spread is far
// below its floor stops producing huge distances from negligible moves, and a
// constant dimension with a floor no longer makes Σ singular. Deviation is
// then in units of sqrt(σᵢ² + sᵢ²); the reported stddev and corr stay those of
// the data.
type mahalanobis struct{}

func (mahalanobis) Name() string { return "mahalanobis" }

func (mahalanobis) Validate(params map[string]any) error {
	if err := processor.CheckKeys(params, "threshold"); err != nil {
		return err
	}
	if processor.IsTemplate(params["threshold"]) {
		return nil
	}
	_, err := mahalanobisThreshold(params)
	return err
}

func mahalanobisThreshold(params map[string]any) (float64, error) {
	th, err := processor.Float(params, "threshold", DefaultThreshold)
	if err != nil {
		return 0, err
	}
	if th <= 0 {
		return 0, fmt.Errorf("with.threshold: must be > 0, got %v", th)
	}
	return th, nil
}

func (mahalanobis) Detect(params map[string]any, s Series, floor []float64) (Verdict, error) {
	th, err := mahalanobisThreshold(params)
	if err != nil {
		return Verdict{}, err
	}
	n := len(s.Dims)
	mu := means(s)
	cov := covariance(s, mu)
	stats := map[string]float64{"threshold": th}
	sigma := make([]float64, n)
	for i, dim := range s.Dims {
		sigma[i] = math.Sqrt(cov[i][i])
		stats["mean_"+dim] = mu[i]
		stats["stddev_"+dim] = sigma[i]
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if sigma[i] > 0 && sigma[j] > 0 {
				stats["corr_"+s.Dims[i]+"_"+s.Dims[j]] = cov[i][j] / (sigma[i] * sigma[j])
			}
		}
	}
	for i := 0; i < n && i < len(floor); i++ {
		cov[i][i] += floor[i] * floor[i]
		sigma[i] = math.Sqrt(cov[i][i])
	}
	inv, ok := invert(cov)
	if !ok {
		reason := "covariance matrix is singular (a constant or perfectly collinear input)"
		if floor == nil {
			reason += "; with.min_delta gives every dimension a noise floor"
		}
		return Verdict{Stats: stats, Skip: reason}, nil
	}
	v := Verdict{Stats: stats}
	diff := make([]float64, n)
	maxD := 0.0
	for idx, p := range s.Points {
		for i := range diff {
			diff[i] = p.V[i] - mu[i]
		}
		d := math.Sqrt(math.Max(quadratic(inv, diff), 0))
		maxD = math.Max(maxD, d)
		if d <= th {
			continue
		}
		dev := make([]float64, n)
		for i := range dev {
			dev[i] = diff[i] / sigma[i]
		}
		v.Anomalies = append(v.Anomalies, Anomaly{
			Index:     idx,
			Score:     d,
			Expected:  append([]float64(nil), mu...),
			Deviation: dev,
		})
	}
	stats["max_distance"] = maxD
	return v, nil
}
