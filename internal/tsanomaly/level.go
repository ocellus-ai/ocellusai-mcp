package tsanomaly

import (
	"fmt"
	"math"
	"sort"
)

// LevelShift scores each point by how far it sits from the median of the
// preceding Window real samples: baseline_t = median(x[t-W .. t-1]),
// residual = x_t - baseline_t, score = robust z of the residual over the
// series. A level the series climbed to slowly is invisible to the one-step
// AR residual (every increment is small) and diluted by the Matrix Profile
// (a shape shorter than the window), but it sits far above a trailing
// median for up to Window/2 points, after which the median catches up; the
// return to the old level is scored the same way. Only real samples enter
// the window, so a gap neither hides nor creates a shift, and a point with
// fewer than MinPoints real samples behind it is not scored.
//
// Scores are Calibrated (robust z) and point-wise: the ensemble fits a
// peaks-over-threshold model to them like to the AR residual. Because a
// shift is a plateau of equal scores, that fit lands its threshold above
// a modest plateau (up to the POT cap, about 3 sigma-equivalents of the
// risk level, 11 at risk 1e-3), so only shifts of a dozen MAD units and
// more are flagged at the default risk; a fixed floor instead (the
// Subsequence treatment of the Matrix Profile) was tried and produced
// hundreds of false segments on bursty network and disk signals, whose
// residuals have heavy tails. Lower the risk to 0.01 to catch subtler
// levels around a known moment.
type LevelShift struct {
	Window    int // trailing window in points
	MinPoints int // real samples needed in the window to score a point (default max(4, Window/4))
}

func (d *LevelShift) Name() string     { return fmt.Sprintf("level(w=%d)", d.Window) }
func (d *LevelShift) Calibrated() bool { return true }

func (d *LevelShift) Score(x []float64) ([]float64, error) {
	n, w := len(x), d.Window
	if w < 2 {
		return nil, fmt.Errorf("level: window must be at least 2 points, got %d", w)
	}
	if n < 4 || w >= n {
		return nil, fmt.Errorf("level: series too short (n=%d) for window %d", n, w)
	}
	minPts := d.MinPoints
	if minPts <= 0 {
		minPts = maxInt(4, w/4)
	}
	y, imputed := ImputeMask(x)
	resid := make([]float64, n)
	valid := make([]bool, n)
	win := make([]float64, 0, w) // the real samples of [t-w, t-1], sorted
	for t := 0; t < n; t++ {
		if len(win) >= minPts {
			resid[t] = y[t] - sortedMedian(win)
			valid[t] = true
		}
		if !imputed[t] {
			win = insertSorted(win, y[t])
		}
		if u := t - w; u >= 0 && !imputed[u] {
			win = removeSorted(win, y[u])
		}
	}
	sub := make([]float64, 0, n)
	for t := range resid {
		if valid[t] && !imputed[t] {
			sub = append(sub, resid[t])
		}
	}
	if len(sub) < 3 {
		return nil, fmt.Errorf("level: not enough points to score")
	}
	med, mad := MAD(sub)
	out := make([]float64, n)
	for t := range out {
		if valid[t] {
			out[t] = math.Abs(resid[t]-med) / mad
		}
	}
	return maskScores(out, imputed), nil
}

// sortedMedian is the median of an ascending, non-empty slice.
func sortedMedian(s []float64) float64 {
	m := len(s) / 2
	if len(s)%2 == 0 {
		return (s[m-1] + s[m]) / 2
	}
	return s[m]
}

// insertSorted adds v to an ascending slice, keeping it sorted.
func insertSorted(s []float64, v float64) []float64 {
	i := sort.SearchFloat64s(s, v)
	s = append(s, 0)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// removeSorted drops one occurrence of v from an ascending slice.
func removeSorted(s []float64, v float64) []float64 {
	i := sort.SearchFloat64s(s, v)
	if i < len(s) && s[i] == v {
		return append(s[:i], s[i+1:]...)
	}
	return s
}
