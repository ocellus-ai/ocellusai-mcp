package anomaly

import (
	"math"
	"sort"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Point is one sample of a series (series.Point).
type Point = series.Point

// Series is one time series handed to a detector: finite samples in time
// order (series.Series). outliers hands the same type with the item index
// as T, so a detector must not assume T is a timestamp.
type Series = series.Series

// Anomaly flags one sample of a Series.
type Anomaly struct {
	// Index is the position in Series.Points.
	Index int
	// Score is the method-specific severity; larger means more anomalous, never negative.
	Score float64
	// Expected is the baseline the sample was compared against.
	Expected float64
	// Direction is DirectionUp when the sample is above the baseline, DirectionDown below.
	Direction string
}

// Verdict is what a detector reports for one series.
type Verdict struct {
	// Anomalies in index order.
	Anomalies []Anomaly
	// Stats is a method-specific summary (mean/stddev, quartiles, fences, ...)
	// reported to the agent as-is under "stats".
	Stats map[string]float64
	// Expected optionally holds a per-point baseline (len == len(Points)) for
	// methods that fit a model; nil when the method has no such notion.
	Expected []float64
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
	// Detect analyses one series with rendered params. It is only called with
	// at least one point (see min_points).
	Detect(params map[string]any, s Series) (Verdict, error)
}

// Direction values.
const (
	DirectionUp   = "up"
	DirectionDown = "down"
	DirectionBoth = "both"
)

// detectors is the algorithm registry: with.method → implementation.
var detectors = map[string]Detector{
	"zscore": zscore{},
	"iqr":    iqr{},
}

// Lookup returns the detector registered under method (with.method). Other
// processors that share the algorithms (outliers) resolve them through it.
func Lookup(method string) (Detector, bool) {
	d, ok := detectors[method]
	return d, ok
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

func values(s Series) []float64 {
	out := make([]float64, len(s.Points))
	for i, p := range s.Points {
		out[i] = p.V
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

// stddev is the population standard deviation around m.
func stddev(v []float64, m float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		d := x - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(v)))
}

// quantile returns the q-th quantile (0..1) of an ascending slice using
// linear interpolation between order statistics (R type 7 / numpy default).
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

func minMax(v []float64) (lo, hi float64) {
	if len(v) == 0 {
		return 0, 0
	}
	lo, hi = v[0], v[0]
	for _, x := range v[1:] {
		lo = math.Min(lo, x)
		hi = math.Max(hi, x)
	}
	return lo, hi
}

func direction(v, baseline float64) string {
	if v > baseline {
		return DirectionUp
	}
	return DirectionDown
}
