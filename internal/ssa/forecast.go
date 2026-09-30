// L-forecast and K-forecast for a univariate SSA and for an MSSA.
//
// Both continue the reconstruction of a chosen group of components, not the
// raw series. The returned slice has length n+steps: indices [0, n) are that
// reconstruction and [n, n+steps) are the forecast. This is py-ssa-lib's
// L_forecast / K_forecast with return_full=True.
//
// L-forecast is the linear recurrent continuation along the window (Golyandina).
// With P the selected left singular vectors as columns, π the last row of P
// and ν² = ‖π‖² < 1,
//
//	R = P[:-1] · π / (1−ν²),     ŷ[t] = Σ_{j=1}^{L−1} R[j−1] · ŷ[t−L+j].
//
// The same coefficients apply to every channel: the recurrence lives in the
// shared column space of the trajectory matrix. The recurrence does not exist
// when the subspace is vertical (ν² = 1), which is the case for the full basis
// of all L components.
//
// K-forecast is the dual continuation along the rows of the trajectory matrix,
// from the right singular vectors. Split each selected right vector into the
// per-channel blocks of length K, let W be the last entry of every block
// (S×r) and Q the remaining entries ((K−1)·S × r). Then
//
//	F = (I − W·Wᵀ)⁻¹ · W·Qᵀ
//
// advances the last K−1 weighted observations of all channels,
// z = (√wₛ · ŷₛ[t−K+1 : t]) stacked by channel. The forecast is computed in
// that weighted scale and divided by √wₛ afterwards. For one series and
// weight 1 this is the K-forecast of ordinary SSA. Right vectors come from
// gvec: vᵢ = (Hᵀuᵢ)/σᵢ, and for channel s the block is √wₛ·vᵢ.

package ssa

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/mat"
)

// LForecast continues the reconstruction of group by steps points with the
// L-recurrence. The first len(x) values are the reconstruction.
func (s *SSA) LForecast(group []int, steps int) ([]float64, error) {
	if err := validateForecast(s.Len(), group, steps); err != nil {
		return nil, err
	}
	R, err := lrr(s.U, group)
	if err != nil {
		return nil, err
	}
	return continueL([][]float64{s.Reconstruct(group)}, R, steps)[0], nil
}

// KForecast continues the reconstruction of group by steps points with the
// K-forecast. The first len(x) values are the reconstruction.
func (s *SSA) KForecast(group []int, steps int) ([]float64, error) {
	if err := validateForecast(s.Len(), group, steps); err != nil {
		return nil, err
	}
	blocks := [][][]float64{make([][]float64, len(group))}
	for p, i := range group {
		v, err := rightVector(s.gvec(i), s.Sigma[i], 1)
		if err != nil {
			return nil, fmt.Errorf("forecast: component %d: %w", i, err)
		}
		blocks[0][p] = v
	}
	F, err := kOperator(blocks)
	if err != nil {
		return nil, err
	}
	return continueK([][]float64{s.Reconstruct(group)}, []float64{1}, F, s.K, steps)[0], nil
}

// LForecast continues every channel by steps points with the shared
// L-recurrence. Y[s] has length n+steps; the first n values are the
// reconstruction of group on channel s.
func (m *MSSA) LForecast(group []int, steps int) ([][]float64, error) {
	if err := validateForecast(m.Len(), group, steps); err != nil {
		return nil, err
	}
	R, err := lrr(m.U, group)
	if err != nil {
		return nil, err
	}
	hist, err := m.Reconstruct(group)
	if err != nil {
		return nil, err
	}
	return continueL(hist, R, steps), nil
}

// KForecast continues every channel by steps points with the K-forecast.
// Y[s] has length n+steps; the first n values are the reconstruction of
// group on channel s.
func (m *MSSA) KForecast(group []int, steps int) ([][]float64, error) {
	if err := validateForecast(m.Len(), group, steps); err != nil {
		return nil, err
	}
	S := m.Channels()
	blocks := make([][][]float64, S)
	for s := range S {
		blocks[s] = make([][]float64, len(group))
		sw := m.Weights[s]
		for p, i := range group {
			v, err := rightVector(m.gvec(s, i), m.Sigma[i], sw)
			if err != nil {
				return nil, fmt.Errorf("forecast: component %d: %w", i, err)
			}
			blocks[s][p] = v
		}
	}
	F, err := kOperator(blocks)
	if err != nil {
		return nil, err
	}
	hist, err := m.Reconstruct(group)
	if err != nil {
		return nil, err
	}
	return continueK(hist, m.Weights, F, m.K, steps), nil
}

func validateForecast(nComp int, group []int, steps int) error {
	if steps < 1 {
		return fmt.Errorf("forecast: steps must be positive")
	}
	if err := checkGroup(nComp, group); err != nil {
		return fmt.Errorf("forecast: %w", err)
	}
	return nil
}

func checkGroup(nComp int, group []int) error {
	if len(group) == 0 {
		return fmt.Errorf("empty component group")
	}
	seen := make([]bool, nComp)
	for _, i := range group {
		if i < 0 || i >= nComp {
			return fmt.Errorf("component %d: the decomposition has %d components", i, nComp)
		}
		if seen[i] {
			return fmt.Errorf("component %d listed twice", i)
		}
		seen[i] = true
	}
	return nil
}

// lrr returns the L-recurrence coefficients of the subspace spanned by the
// selected left vectors. The coefficient vector has length L−1.
func lrr(vecs [][]float64, group []int) ([]float64, error) {
	L := len(vecs[group[0]])
	pi := make([]float64, len(group))
	var nu2 float64
	for p, i := range group {
		pi[p] = vecs[i][L-1]
		nu2 += pi[p] * pi[p]
	}
	den := 1 - nu2
	// An orthonormal basis of all of R^L has nu^2 = 1; roundoff leaves a
	// denominator around 1e-16. Anything that close is vertical.
	if !(den > 1e-12) {
		return nil, fmt.Errorf("forecast: L-recurrence undefined, nu^2 = %.6g (the subspace is vertical)", nu2)
	}
	coef := make([]float64, L-1)
	for t := range coef {
		var acc float64
		for p, i := range group {
			acc += vecs[i][t] * pi[p]
		}
		coef[t] = acc / den
	}
	return coef, nil
}

// rightVector returns √w·(Hᵀu)/σ, one channel's block of the right singular
// vector. g = Hᵀu has length K.
func rightVector(g []float64, sigma, weight float64) ([]float64, error) {
	if sigma == 0 {
		return nil, fmt.Errorf("singular value is 0")
	}
	v := make([]float64, len(g))
	scale := math.Sqrt(weight) / sigma
	for t := range v {
		v[t] = scale * g[t]
	}
	return v, nil
}

// kOperator builds F = (I − W·Wᵀ)⁻¹·W·Qᵀ. blocks[s][p] is the length-K block
// of selected component p on channel s.
func kOperator(blocks [][][]float64) (*mat.Dense, error) {
	S := len(blocks)
	r := len(blocks[0])
	K := len(blocks[0][0])
	if K < 2 {
		return nil, fmt.Errorf("forecast: K-recurrence needs K ≥ 2")
	}
	W := mat.NewDense(S, r, nil)
	Q := mat.NewDense((K-1)*S, r, nil)
	for s := range S {
		for p := range r {
			W.Set(s, p, blocks[s][p][K-1])
			for t := range K - 1 {
				Q.Set(s*(K-1)+t, p, blocks[s][p][t])
			}
		}
	}
	var wwt, B mat.Dense
	wwt.Mul(W, W.T())
	A := mat.NewDense(S, S, nil)
	for i := range S {
		for j := range S {
			v := -wwt.At(i, j)
			if i == j {
				v++
			}
			A.Set(i, j, v)
		}
	}
	B.Mul(W, Q.T())
	var F mat.Dense
	if err := F.Solve(A, &B); err != nil {
		return nil, fmt.Errorf("forecast: K-recurrence undefined: %w", err)
	}
	return &F, nil
}

// continueL appends steps L-recurrent points to each channel.
func continueL(hist [][]float64, coef []float64, steps int) [][]float64 {
	S := len(hist)
	n := len(hist[0])
	L := len(coef) + 1
	out := make([][]float64, S)
	for s := range S {
		out[s] = make([]float64, n+steps)
		copy(out[s], hist[s])
	}
	for m := range steps {
		start := n - L + 1 + m
		for s := range S {
			var acc float64
			for t, c := range coef {
				acc += out[s][start+t] * c
			}
			out[s][n+m] = acc
		}
	}
	return out
}

// continueK appends steps K-forecast points. The recurrence runs on the
// weighted series √w·y and the result is scaled back.
func continueK(hist [][]float64, weights []float64, op *mat.Dense, width, steps int) [][]float64 {
	K := width // the K of the trajectory matrix
	S := len(hist)
	n := len(hist[0])
	sqrtw := make([]float64, S)
	for s := range S {
		sqrtw[s] = math.Sqrt(weights[s])
	}
	y := make([][]float64, S)
	for s := range S {
		y[s] = make([]float64, n+steps)
		for t := range n {
			y[s][t] = hist[s][t] * sqrtw[s]
		}
	}
	q := (K - 1) * S
	Z := make([]float64, q)
	zv := mat.NewVecDense(q, Z)
	fs := mat.NewVecDense(S, nil)
	for m := range steps {
		start := n - K + 1 + m
		for s := range S {
			copy(Z[s*(K-1):(s+1)*(K-1)], y[s][start:start+K-1])
		}
		fs.MulVec(op, zv)
		for s := range S {
			y[s][n+m] = fs.AtVec(s)
		}
	}
	for s := range S {
		inv := 1 / sqrtw[s]
		for t := range y[s] {
			y[s][t] *= inv
		}
	}
	return y
}
