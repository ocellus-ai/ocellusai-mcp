package tsanomaly

import "fmt"

// SeasonalResidual scores each point by how far it is from the typical value
// at the same phase of the season: baseline_t = median over cycles of
// x_{t - k*Season}, residual = x_t - baseline_t, score = robust z of the
// residual. With at least two full cycles it is a seasonal-naive detector;
// with fewer it degrades to a single-lag comparison. It catches "normal value
// but not at 3am" and amplitude changes that z-normalised Matrix Profile
// cannot see, and unlike a daily lag inside ARResidual it scores every point
// after the first cycle.
//
// Season is in points (86400/step for a daily cycle). Scores are Calibrated.
type SeasonalResidual struct {
	Season int
	// Cycles limits how many past/future cycles are used for the baseline
	// (default: all available, both sides).
	Cycles int
}

func (d *SeasonalResidual) Name() string     { return fmt.Sprintf("seasonal(S=%d)", d.Season) }
func (d *SeasonalResidual) Calibrated() bool { return true }

func (d *SeasonalResidual) Score(x []float64) ([]float64, error) {
	n := len(x)
	S := d.Season
	if S <= 1 || S >= n {
		return nil, fmt.Errorf("seasonal: season must be in (1, n) (season=%d, n=%d)", S, n)
	}
	y, imputed := ImputeMask(x)
	cyc := d.Cycles
	if cyc <= 0 {
		cyc = n/S + 1
	}
	resid := make([]float64, n)
	valid := make([]bool, n)
	buf := make([]float64, 0, 2*cyc)
	for t := 0; t < n; t++ {
		buf = buf[:0]
		for k := 1; k <= cyc; k++ {
			if u := t - k*S; u >= 0 && !imputed[u] {
				buf = append(buf, y[u])
			}
			if u := t + k*S; u < n && !imputed[u] {
				buf = append(buf, y[u])
			}
		}
		if len(buf) == 0 {
			continue
		}
		resid[t] = y[t] - Median(buf)
		valid[t] = true
	}
	// robust z on valid residuals only
	sub := make([]float64, 0, n)
	for t := range resid {
		if valid[t] && !imputed[t] {
			sub = append(sub, resid[t])
		}
	}
	if len(sub) < 3 {
		return nil, fmt.Errorf("seasonal: not enough points to score")
	}
	med, mad := MAD(sub)
	out := make([]float64, n)
	for t := range out {
		if valid[t] {
			out[t] = abs(resid[t]-med) / mad
		}
	}
	return maskScores(out, imputed), nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
