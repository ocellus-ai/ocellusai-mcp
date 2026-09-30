package tsanomaly

import (
	"fmt"

	"gonum.org/v1/gonum/mat"
)

// ARResidual fits a linear autoregression x_t = b0 + sum_k b_k * x_{t-lag_k}
// by ordinary least squares and scores each point by the robust z-score of
// its one-step-ahead residual. This is the "strong linear baseline" that tops
// point-anomaly leaderboards; it is cheap and works on spikes, drops and level
// shifts. Combine with MatrixProfile for shape anomalies.
//
// The fit is done in two passes: points whose residual z exceeds
// TrimZ in the first pass are excluded from the second fit so that large
// anomalies do not pull the model towards themselves.
//
// Points before the largest lag cannot be scored by the full model; they are
// scored by a fallback model that uses only the lags available there, so a
// daily lag on a 24h batch does not blind the whole series. Imputed points
// get score 0.
//
// Scores are already robust z-scores (Calibrated), so the ensemble does not
// normalise them again.
type ARResidual struct {
	Order int   // use lags 1..Order (ignored if Lags is set)
	Lags  []int // explicit lags, e.g. {1,2,3,96} to add a daily lag at 15-min step
	TrimZ float64
}

func (d *ARResidual) Name() string {
	if d.Lags != nil {
		return fmt.Sprintf("ar(lags=%v)", d.Lags)
	}
	return fmt.Sprintf("ar(p=%d)", d.Order)
}

func (d *ARResidual) lags() []int {
	if len(d.Lags) > 0 {
		return d.Lags
	}
	p := d.Order
	if p <= 0 {
		p = 1
	}
	l := make([]int, p)
	for i := range l {
		l[i] = i + 1
	}
	return l
}

// Calibrated reports that Score already returns robust z-scores.
func (d *ARResidual) Calibrated() bool { return true }

func (d *ARResidual) Score(x []float64) ([]float64, error) {
	y, imputed := ImputeMask(x)
	n := len(y)
	lags := d.lags()
	for _, l := range lags {
		if l <= 0 {
			return nil, fmt.Errorf("ar: lags must be positive, got %d", l)
		}
	}
	trimZ := d.TrimZ
	if trimZ <= 0 {
		trimZ = 4
	}
	full, err := arScore(y, imputed, lags, trimZ)
	if err != nil {
		return nil, err
	}
	// fallback model on the short lags for the prefix the full model cannot see
	maxLag := 0
	for _, l := range lags {
		maxLag = maxInt(maxLag, l)
	}
	var short []int
	for _, l := range lags {
		if l < maxLag {
			short = append(short, l)
		}
	}
	if len(short) > 0 && maxLag > 1 {
		if fb, err := arScore(y, imputed, short, trimZ); err == nil {
			for t := 0; t < maxLag && t < n; t++ {
				full[t] = fb[t]
			}
		}
	}
	return maskScores(full, imputed), nil
}

// arScore fits one AR model and returns robust-z residual scores; the first
// maxLag points are 0. Imputed points are excluded from the fit.
func arScore(y []float64, imputed []bool, lags []int, trimZ float64) ([]float64, error) {
	n := len(y)
	maxLag := 0
	for _, l := range lags {
		maxLag = maxInt(maxLag, l)
	}
	rows := n - maxLag
	p := len(lags) + 1
	if rows < 3*p {
		return nil, fmt.Errorf("ar: series too short (n=%d) for max lag %d", n, maxLag)
	}

	// design matrix
	X := mat.NewDense(rows, p, nil)
	Y := mat.NewVecDense(rows, nil)
	for i := 0; i < rows; i++ {
		t := i + maxLag
		X.Set(i, 0, 1)
		for k, l := range lags {
			X.Set(i, k+1, y[t-l])
		}
		Y.SetVec(i, y[t])
	}

	include := make([]bool, rows)
	for i := range include {
		include[i] = !imputed[i+maxLag]
	}
	var resid []float64
	for pass := 0; pass < 2; pass++ {
		beta := olsFit(X, Y, include)
		resid = make([]float64, rows)
		for i := 0; i < rows; i++ {
			pred := 0.0
			for k := 0; k < p; k++ {
				pred += beta[k] * X.At(i, k)
			}
			resid[i] = Y.AtVec(i) - pred
		}
		if pass == 0 {
			z := RobustZ(resid)
			cnt := 0
			for i := range z {
				include[i] = include[i] && z[i] <= trimZ
				if include[i] {
					cnt++
				}
			}
			if cnt < 3*p { // trimmed too much; keep first-pass result
				break
			}
		}
	}

	z := RobustZ(resid)
	out := make([]float64, n)
	for i := 0; i < rows; i++ {
		out[i+maxLag] = z[i]
	}
	return out, nil
}

// olsFit solves least squares on the subset of rows flagged in include.
func olsFit(x *mat.Dense, y *mat.VecDense, include []bool) []float64 {
	rows, p := x.Dims()
	cnt := 0
	for _, inc := range include {
		if inc {
			cnt++
		}
	}
	A := mat.NewDense(cnt, p, nil)
	b := mat.NewVecDense(cnt, nil)
	r := 0
	for i := 0; i < rows; i++ {
		if !include[i] {
			continue
		}
		for k := 0; k < p; k++ {
			A.Set(r, k, x.At(i, k))
		}
		b.SetVec(r, y.AtVec(i))
		r++
	}
	var beta mat.VecDense
	if err := beta.SolveVec(A, b); err != nil {
		// singular design (e.g. constant series): fall back to intercept-only
		beta = *mat.NewVecDense(p, nil)
		m := 0.0
		for i := 0; i < cnt; i++ {
			m += b.AtVec(i)
		}
		beta.SetVec(0, m/float64(cnt))
	}
	out := make([]float64, p)
	for k := 0; k < p; k++ {
		v := beta.AtVec(k)
		if !isFinite(v) {
			v = 0
		}
		out[k] = v
	}
	return out
}
