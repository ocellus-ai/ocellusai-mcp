package tsanomaly

import (
	"math"
	"sort"
)

// Impute returns a copy of x with NaN/Inf replaced by linear interpolation
// between the nearest finite neighbours; leading/trailing gaps are filled with
// the nearest finite value. If nothing is finite the result is all zeros.
func Impute(x []float64) []float64 {
	y, _ := ImputeMask(x)
	return y
}

// ImputeMask is Impute that also returns which indices were filled in.
// Detectors zero their scores on imputed points and ignore windows that are
// mostly imputed: a linear fill is not an observation and must not become a
// "unique shape" or a perfectly predictable segment.
func ImputeMask(x []float64) (y []float64, imputed []bool) {
	y = make([]float64, len(x))
	copy(y, x)
	n := len(y)
	imputed = make([]bool, n)
	prev := -1
	for i := 0; i < n; i++ {
		if !isFinite(y[i]) {
			imputed[i] = true
			continue
		}
		if prev == -1 {
			for k := 0; k < i; k++ { // leading gap
				y[k] = y[i]
			}
		} else if i-prev > 1 {
			step := (y[i] - y[prev]) / float64(i-prev)
			for k := prev + 1; k < i; k++ {
				y[k] = y[prev] + step*float64(k-prev)
			}
		}
		prev = i
	}
	if prev == -1 {
		for i := range y {
			y[i] = 0
		}
		return y, imputed
	}
	for k := prev + 1; k < n; k++ { // trailing gap
		y[k] = y[prev]
	}
	return y, imputed
}

// maskScores zeroes scores on imputed points.
func maskScores(scores []float64, imputed []bool) []float64 {
	for i := range scores {
		if i < len(imputed) && imputed[i] {
			scores[i] = 0
		}
	}
	return scores
}

// windowImputedFrac returns, for every window start i, the fraction of
// imputed points inside [i, i+w).
func windowImputedFrac(imputed []bool, w int) []float64 {
	n := len(imputed)
	m := n - w + 1
	out := make([]float64, m)
	cnt := 0
	for i := 0; i < n; i++ {
		if imputed[i] {
			cnt++
		}
		if i >= w && imputed[i-w] {
			cnt--
		}
		if i >= w-1 {
			out[i-w+1] = float64(cnt) / float64(w)
		}
	}
	return out
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Median returns the median of x (x is not modified).
func Median(x []float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), x...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 0 {
		return (s[m-1] + s[m]) / 2
	}
	return s[m]
}

// MAD returns median and the scaled median absolute deviation (consistent
// with sigma for a normal distribution). If MAD is zero it falls back to the
// mean absolute deviation, then to a tiny epsilon.
func MAD(x []float64) (med, mad float64) {
	med = Median(x)
	dev := make([]float64, len(x))
	for i, v := range x {
		dev[i] = math.Abs(v - med)
	}
	mad = 1.4826 * Median(dev)
	if mad == 0 {
		s := 0.0
		for _, d := range dev {
			s += d
		}
		mad = 1.2533 * s / float64(len(dev))
	}
	if mad == 0 || !isFinite(mad) {
		mad = 1e-12
	}
	return
}

// RobustZ maps x to |x - median| / MAD.
func RobustZ(x []float64) []float64 {
	med, mad := MAD(x)
	z := make([]float64, len(x))
	for i, v := range x {
		z[i] = math.Abs(v-med) / mad
	}
	return z
}

// Quantile returns the q-quantile (0..1) of x using linear interpolation.
func Quantile(x []float64, q float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), x...)
	sort.Float64s(s)
	if q <= 0 {
		return s[0]
	}
	if q >= 1 {
		return s[len(s)-1]
	}
	pos := q * float64(len(s)-1)
	lo := int(pos)
	frac := pos - float64(lo)
	if lo+1 >= len(s) {
		return s[lo]
	}
	return s[lo]*(1-frac) + s[lo+1]*frac
}

// Ranks returns average ranks of x scaled to [0,1] (ties get the mean rank).
func Ranks(x []float64) []float64 {
	n := len(x)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return x[idx[a]] < x[idx[b]] })
	r := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && x[idx[j+1]] == x[idx[i]] {
			j++
		}
		avg := float64(i+j) / 2
		for k := i; k <= j; k++ {
			r[idx[k]] = avg
		}
		i = j + 1
	}
	if n > 1 {
		for i := range r {
			r[i] /= float64(n - 1)
		}
	}
	return r
}

// MovingAverage smooths x with a centred window of length w (w<=1: no-op).
func MovingAverage(x []float64, w int) []float64 {
	if w <= 1 {
		return append([]float64(nil), x...)
	}
	n := len(x)
	out := make([]float64, n)
	half := w / 2
	sum := 0.0
	cnt := 0
	// prefix sums for O(n)
	ps := make([]float64, n+1)
	for i, v := range x {
		ps[i+1] = ps[i] + v
	}
	for i := 0; i < n; i++ {
		lo := i - half
		hi := i + (w - 1 - half)
		if lo < 0 {
			lo = 0
		}
		if hi >= n {
			hi = n - 1
		}
		sum = ps[hi+1] - ps[lo]
		cnt = hi - lo + 1
		out[i] = sum / float64(cnt)
	}
	return out
}

// windowToPoints maps a per-subsequence profile (length n-w+1, entry i refers
// to the subsequence starting at i) to per-point scores of length n: point t
// gets the maximum over all windows that cover it, i in [t-w+1, t]. A discord
// therefore marks its whole extent (no centre-shift), at the cost of ranges
// being up to w-1 points wider than the true event; Ranges reports the peak.
func windowToPoints(prof []float64, n, w int) []float64 {
	out, _ := windowToPointsIdx(prof, n, w)
	return out
}

// windowToPointsIdx is windowToPoints that also returns, per point, the
// index of the window that supplied its score.
func windowToPointsIdx(prof []float64, n, w int) ([]float64, []int) {
	out := make([]float64, n)
	src := make([]int, n)
	m := len(prof)
	// sliding-window maximum with a monotonic deque
	dq := make([]int, 0, w)
	for t := 0; t < n; t++ {
		if t < m {
			for len(dq) > 0 && prof[dq[len(dq)-1]] <= prof[t] {
				dq = dq[:len(dq)-1]
			}
			dq = append(dq, t)
		}
		lo := t - w + 1
		for len(dq) > 0 && dq[0] < lo {
			dq = dq[1:]
		}
		if len(dq) > 0 {
			out[t] = prof[dq[0]]
			src[t] = dq[0]
		} else {
			src[t] = -1
		}
	}
	return out, src
}

// robustScale returns (x - median) / MAD.
func robustScale(x []float64) []float64 {
	med, mad := MAD(x)
	y := make([]float64, len(x))
	for i, v := range x {
		y[i] = (v - med) / mad
	}
	return y
}

// sanitize replaces non-finite values by the largest finite value in the slice
// (in place) and returns the slice.
func sanitize(x []float64) []float64 {
	mx := math.Inf(-1)
	for _, v := range x {
		if isFinite(v) && v > mx {
			mx = v
		}
	}
	if !isFinite(mx) {
		mx = 0
	}
	for i, v := range x {
		if !isFinite(v) {
			x[i] = mx
		}
	}
	return x
}

// standardizeCols scales every column to (x - median) / MAD computed on the
// rows flagged in train (nil = all rows). Median/MAD are used instead of
// mean/std so that the anomaly itself does not inflate the scale and shrink
// its own z-score.
func standardizeCols(cols [][]float64, train []bool) [][]float64 {
	d := len(cols)
	out := make([][]float64, d)
	for j := 0; j < d; j++ {
		c := cols[j]
		sub := c
		if train != nil {
			sub = make([]float64, 0, len(c))
			for i, v := range c {
				if train[i] {
					sub = append(sub, v)
				}
			}
		}
		m, s := MAD(sub)
		if s < 1e-12 {
			s = 1
		}
		out[j] = make([]float64, len(c))
		for i := range c {
			out[j][i] = (c[i] - m) / s
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
