package anomalymv

import (
	"math"
	"sort"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Point is one sample of a multivariate series: the values of every
// dimension at one timestamp (series.MultiPoint; V[i] belongs to Dims[i]).
type Point = series.MultiPoint

// Series is one multivariate time series handed to a detector: finite
// samples in the order received, one value per dimension, with Dims naming
// the dimensions (series.MultiSeries).
type Series = series.MultiSeries

// Anomaly flags one sample of a Series.
type Anomaly struct {
	// Index is the position in Series.Points.
	Index int
	// Score is the method-specific severity; larger means more anomalous, never negative.
	Score float64
	// Expected is the per-dimension baseline the sample was compared against.
	Expected []float64
	// Deviation is the per-dimension signed distance from Expected in units
	// the method defines (Mahalanobis: standard deviations). It tells the agent
	// which input drives the anomaly.
	Deviation []float64
}

// Verdict is what a detector reports for one series.
type Verdict struct {
	// Anomalies in index order.
	Anomalies []Anomaly
	// Stats is a method-specific summary reported to the agent as-is under "stats".
	Stats map[string]float64
	// Skip, when non-empty, tells the processor that the series could not be
	// scored (for example a degenerate covariance); it is reported as skipped
	// with Skip as the reason instead of failing the whole tool.
	Skip string
}

// Detector is the algorithm contract. Adding an algorithm means implementing
// this interface in its own file and adding one entry to the detectors table.
type Detector interface {
	// Name is the value of with.method.
	Name() string
	// Validate checks the method-specific params (everything in `with` except
	// the common keys) at catalog load time. Values may still be templates;
	// use processor.IsTemplate to skip checks that need the rendered value.
	Validate(params map[string]any) error
	// Detect analyses one joined series with rendered params. It is only called
	// with at least one point and at least two dimensions. floor is either nil
	// or holds, per dimension (floor[i] belongs to s.Dims[i]), the smallest
	// spread (a standard deviation, in the metric's units) the detector must
	// assume, whatever the series shows: the processor derives it from
	// with.min_delta so that a nearly constant dimension does not turn a
	// negligible move into a huge deviation. 0 leaves a dimension as it is.
	Detect(params map[string]any, s Series, floor []float64) (Verdict, error)
}

// detectors is the algorithm registry: with.method → implementation.
var detectors = map[string]Detector{
	"mahalanobis": mahalanobis{},
}

// Detectors returns the registered method names, sorted.
func Detectors() []string {
	names := make([]string, 0, len(detectors))
	for n := range detectors {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// --- shared statistics helpers -------------------------------------------

// column returns the values of dimension d.
func column(s Series, d int) []float64 {
	out := make([]float64, len(s.Points))
	for i, p := range s.Points {
		out[i] = p.V[d]
	}
	return out
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// means returns the per-dimension mean of the series.
func means(s Series) []float64 {
	out := make([]float64, len(s.Dims))
	for d := range s.Dims {
		out[d] = mean(column(s, d))
	}
	return out
}

// covariance returns the population covariance matrix around mu.
func covariance(s Series, mu []float64) [][]float64 {
	n := len(s.Dims)
	cov := make([][]float64, n)
	for i := range cov {
		cov[i] = make([]float64, n)
	}
	if len(s.Points) == 0 {
		return cov
	}
	for _, p := range s.Points {
		for i := 0; i < n; i++ {
			di := p.V[i] - mu[i]
			for j := i; j < n; j++ {
				cov[i][j] += di * (p.V[j] - mu[j])
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			cov[i][j] /= float64(len(s.Points))
			cov[j][i] = cov[i][j]
		}
	}
	return cov
}

// invert returns the inverse of a square matrix by Gauss-Jordan elimination
// with partial pivoting. ok is false when the matrix is singular (a pivot is
// negligible relative to the largest entry).
func invert(m [][]float64) (inv [][]float64, ok bool) {
	n := len(m)
	a := make([][]float64, n)
	inv = make([][]float64, n)
	scale := 0.0
	for i := range m {
		a[i] = append([]float64(nil), m[i]...)
		inv[i] = make([]float64, n)
		inv[i][i] = 1
		for _, x := range m[i] {
			scale = math.Max(scale, math.Abs(x))
		}
	}
	if scale == 0 {
		return nil, false
	}
	eps := scale * 1e-12
	for col := 0; col < n; col++ {
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) <= eps {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		inv[col], inv[pivot] = inv[pivot], inv[col]
		p := a[col][col]
		for j := 0; j < n; j++ {
			a[col][j] /= p
			inv[col][j] /= p
		}
		for r := 0; r < n; r++ {
			if r == col || a[r][col] == 0 {
				continue
			}
			f := a[r][col]
			for j := 0; j < n; j++ {
				a[r][j] -= f * a[col][j]
				inv[r][j] -= f * inv[col][j]
			}
		}
	}
	return inv, true
}

// quadratic returns dᵀ·m·d.
func quadratic(m [][]float64, d []float64) float64 {
	sum := 0.0
	for i := range d {
		row := 0.0
		for j := range d {
			row += m[i][j] * d[j]
		}
		sum += d[i] * row
	}
	return sum
}
