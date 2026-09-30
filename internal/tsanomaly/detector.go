// Package tsanomaly implements batch anomaly detection for univariate and
// multivariate time series (e.g. metrics pulled from VictoriaMetrics/Prometheus).
//
// The package is the standalone tsanomaly module brought into ocellusai-mcp: its
// Prometheus client and CLI are left out (the prometheus worker and the
// pipeline take their place, see processor/ensemble), Options gained
// SkipSeasonal and SkipLevel, the attribution ranks metrics that are
// anomalous on their own first (see attribute), and LevelShift (a
// trailing-median residual for levels reached slowly, which the one-step
// AR residual misses) joined the per-metric ensemble. The original
// detectors are unchanged.
//
// All detectors return one anomaly score per input point (higher = more
// anomalous). Scores of different detectors live on different scales, so use
// CombineScores to combine them and Flags/Ranges to turn scores into events.
package tsanomaly

// UnivariateDetector scores a single series.
type UnivariateDetector interface {
	Name() string
	// Score returns len(x) scores. NaN in x is imputed internally.
	Score(x []float64) ([]float64, error)
}

// MultivariateDetector scores an aligned set of series.
// cols[j] is the j-th series; all columns must have equal length n.
type MultivariateDetector interface {
	Name() string
	// ScoreMulti returns n scores, one per time index.
	ScoreMulti(cols [][]float64) ([]float64, error)
}

// Range is a contiguous anomalous segment [Start, End] (inclusive point indices).
type Range struct {
	Start, End int
	Peak       int     // index of the max score inside the range
	PeakScore  float64 // score at Peak
}
