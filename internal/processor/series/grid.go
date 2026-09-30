package series

import (
	"fmt"
	"math"
	"sort"
)

// Grid is a multivariate series laid on a regular time grid: slot i is at
// Times[i] = Times[0] + i·Step.
type Grid struct {
	Step  float64     // seconds
	Times []float64   // slot timestamps, unix seconds
	Cols  [][]float64 // Cols[d][i]: dimension d at slot i, NaN where no sample fell
	Gaps  int         // slots without a sample
}

// Len returns the number of slots.
func (g *Grid) Len() int { return len(g.Times) }

// Regular places points (dims values each) on a regular grid. With step 0
// the step is the smallest positive interval between consecutive samples.
// Samples are sorted by time first; every sample goes to the nearest slot
// (the last one wins on a collision) and slots without a sample are NaN. A
// non-empty reason means the points cannot be gridded: none at all, a single
// timestamp without an explicit step, or a grid that would need more than
// three slots per sample (the samples are not regular, for instance calls
// with different steps). The input is not modified.
func Regular(points []MultiPoint, dims int, step float64) (*Grid, string) {
	if len(points) == 0 {
		return nil, "no samples"
	}
	pts := append([]MultiPoint(nil), points...)
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	if step <= 0 {
		step = math.Inf(1)
		for i := 1; i < len(pts); i++ {
			if d := pts[i].T - pts[i-1].T; d > 0 && d < step {
				step = d
			}
		}
		if math.IsInf(step, 1) {
			return nil, "cannot infer the step: all samples share one timestamp (set with.step)"
		}
	}
	first := pts[0].T
	n := int(math.Round((pts[len(pts)-1].T-first)/step)) + 1
	if n > 3*len(pts) {
		return nil, fmt.Sprintf("samples are not on a regular grid: a step of %gs (the smallest interval) needs %d slots for %d samples (use the same start/end/step in every call, or set with.step)", step, n, len(pts))
	}
	g := &Grid{Step: step, Times: make([]float64, n), Cols: make([][]float64, dims)}
	for i := range g.Times {
		g.Times[i] = first + float64(i)*step
	}
	for d := range g.Cols {
		g.Cols[d] = make([]float64, n)
		for i := range g.Cols[d] {
			g.Cols[d][i] = math.NaN()
		}
	}
	filled := make([]bool, n)
	for _, p := range pts {
		slot := int(math.Round((p.T - first) / step))
		if slot < 0 || slot >= n {
			continue
		}
		for d := range g.Cols {
			g.Cols[d][slot] = p.V[d]
		}
		filled[slot] = true
	}
	for _, f := range filled {
		if !f {
			g.Gaps++
		}
	}
	return g, ""
}

// ScalarPoints converts the samples of a scalar series to one-dimensional
// points, the input of Regular.
func ScalarPoints(points []Point) []MultiPoint {
	out := make([]MultiPoint, len(points))
	for i, p := range points {
		out[i] = MultiPoint{T: p.T, V: []float64{p.V}}
	}
	return out
}
