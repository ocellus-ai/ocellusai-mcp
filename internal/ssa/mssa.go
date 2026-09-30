// Multivariate SSA, the multichannel extension of the decomposition in ssa.go.
//
// S series of length n, each with a positive weight wₛ (uniform weights are
// 1), are embedded into Hankel blocks Hₛ and stacked horizontally:
//
//	X = [√w₀·H₀ | √w₁·H₁ | … | √w_{S−1}·H_{S−1}],   size L×(K·S), K = n−L+1.
//
// Two decompositions, the same choice as Decompose:
//
//   - full, k = 0 or k close to L: eigendecomposition of the lag-covariance
//     C = X·Xᵀ = Σₛ wₛ·Hₛ·Hₛᵀ. Each block's covariance is lagCov, so X is
//     never formed. All L components.
//   - top-k: the randomized subspace iteration of SSA.topK. That method itself
//     is hardwired to one Hankel matrix of width K, so it cannot be called
//     on the stacked trajectory. The iteration is the same one — the same
//     seed, the same oversampling l = k + max(10, k/2), the same
//     orthonormalize — and every product with a block is hankel.corr:
//     (X·Xᵀ)q = Σₛ wₛ·Hₛ·(Hₛᵀq). Memory is O(n·k·S) instead of O(L²).
//
// Left singular vectors and singular values come from that decomposition.
// The block of the right vector for series s is √wₛ·(Hₛᵀuᵢ)/σᵢ.
//
// Diagonal averaging cancels the weight: the series-s reconstruction of
// component i is the diagonal average of uᵢ·(Hₛᵀuᵢ)ᵀ, the same convolution
// Reconstruct uses. The sum of all L components of every series is that
// series. A single series with weight 1 is the univariate full decomposition.
//
// The window is the MSSA convention 2 ≤ L ≤ floor((n+1)/2), so L ≤ K. Unlike
// the univariate path, a window longer than n/2 is not folded into its
// complement: with several series the stacked matrix and its transpose are
// different decompositions.

package ssa

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/mat"
)

// MSSA is the decomposition of several series of equal length.
type MSSA struct {
	L, K    int         // window length and K = n−L+1, L ≤ K
	Sigma   []float64   // singular values, descending: all L (full) or the leading k (top-k)
	U       [][]float64 // U[i] is the left singular vector uᵢ, length L
	Weights []float64   // positive weight of each series; uniform weights are 1
	Norm2   float64     // ‖X‖²_F of the weighted trajectory matrix
	Err     []float64   // top-k only: ‖X·Xᵀuᵢ − σᵢ²uᵢ‖/σᵢ², the accuracy of component i

	iters int // top-k: power iterations
	x     [][]float64
	h     []*hankel
	g     [][][]float64 // g[s][i] = Hₛᵀuᵢ, length K, filled on demand
}

// DecomposeMulti computes the MSSA of series, which must all have the same
// length. The k leading components are computed by the randomized method, or
// all of them exactly when k = 0 or k is close to L. iters is the number of
// power iterations. weights is a positive weight per series; nil means weight
// 1 for every series. L is the window length.
func DecomposeMulti(series [][]float64, window, k, iters int, weights []float64) (*MSSA, error) {
	L := window
	if len(series) == 0 {
		return nil, fmt.Errorf("mssa: no series")
	}
	n := len(series[0])
	if n < 3 {
		return nil, fmt.Errorf("mssa: series shorter than 3 points")
	}
	for s, col := range series {
		if len(col) != n {
			return nil, fmt.Errorf("mssa: series %d has length %d, want %d", s, len(col), n)
		}
		for j, v := range col {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("mssa: series %d value %d is %v", s, j, v)
			}
		}
	}
	maxL := (n + 1) / 2
	if L < 2 || L > maxL {
		return nil, fmt.Errorf("mssa: window L = %d must be in [2, %d]", L, maxL)
	}
	w, err := seriesWeights(len(series), weights)
	if err != nil {
		return nil, err
	}

	xs := make([][]float64, len(series))
	hs := make([]*hankel, len(series))
	for s, col := range series {
		xs[s] = slices.Clone(col)
		hs[s] = newHankel(xs[s], L)
	}

	m := &MSSA{L: L, K: hs[0].K, Weights: w, x: xs, h: hs}
	for s := range xs {
		for j, v := range xs[s] {
			m.Norm2 += w[s] * hs[0].w[j] * v * v
		}
	}
	// Same switch as Decompose: top-k only when the oversampled basis is
	// strictly shorter than the window, otherwise the exact eigenpath.
	if k > 0 {
		if l := k + max(10, k/2); l < L {
			return m, m.topK(k, l, max(iters, 0))
		}
	}
	return m, m.full()
}

// full computes all L components from the eigendecomposition of
// C = Σₛ wₛ·Hₛ·Hₛᵀ.
func (m *MSSA) full() error {
	C := mat.NewSymDense(m.L, nil)
	for s, col := range m.x {
		Cs := lagCov(col, m.L)
		for i := range m.L {
			for j := i; j < m.L; j++ {
				C.SetSym(i, j, C.At(i, j)+m.Weights[s]*Cs.At(i, j))
			}
		}
	}
	sigma, U, err := eigenBasis(C)
	if err != nil {
		return fmt.Errorf("mssa: %w", err)
	}
	m.Sigma, m.U = sigma, U
	m.g = make([][][]float64, len(m.x))
	for s := range m.x {
		m.g[s] = make([][]float64, len(sigma))
	}
	return nil
}

// topK computes the k leading components by the same randomized subspace
// iteration as SSA.topK. Zₛ = √wₛ·Hₛᵀ·Q, then Q ← Σₛ √wₛ·Hₛ·Zₛ = (X·Xᵀ)·Q,
// and the small eigenproblem is ZᵀZ with Z the channels stacked.
func (m *MSSA) topK(k, l, iters int) error {
	S := len(m.h)
	m.iters = iters
	sw := make([]float64, S)
	for s := range S {
		sw[s] = math.Sqrt(m.Weights[s])
	}
	rng := rand.New(rand.NewPCG(1, 2))
	Q := make([][]float64, l)
	Z := make([][][]float64, l) // Z[j][s] is the block of Xᵀqⱼ for channel s
	for j := range Q {
		Q[j] = make([]float64, m.L)
		for a := range Q[j] {
			Q[j][a] = rng.NormFloat64()
		}
		Z[j] = make([][]float64, S)
		for s := range S {
			Z[j][s] = make([]float64, m.K)
		}
	}
	orthonormalize(Q, rng)
	for range iters {
		m.forProbes(l, func(ws []*fftWork, buf, acc []float64, j int) {
			m.xt(ws, sw, Z[j], Q[j])
			m.xxtInto(ws, sw, Z[j], acc, buf)
			copy(Q[j], acc)
		})
		orthonormalize(Q, rng)
	}
	m.forProbes(l, func(ws []*fftWork, _, _ []float64, j int) {
		m.xt(ws, sw, Z[j], Q[j])
	})

	B := mat.NewSymDense(l, nil)
	for i := range l {
		for j := i; j < l; j++ {
			var d float64
			for s := range S {
				d += floats.Dot(Z[i][s], Z[j][s])
			}
			B.SetSym(i, j, d)
		}
	}
	var es mat.EigenSym
	if !es.Factorize(B, true) {
		return fmt.Errorf("mssa: eigendecomposition failed")
	}
	vals := es.Values(nil) // ascending
	var W mat.Dense
	es.VectorsTo(&W)

	m.Sigma = make([]float64, k)
	m.U = make([][]float64, k)
	m.g = make([][][]float64, S)
	for s := range S {
		m.g[s] = make([][]float64, k)
	}
	for i := range k {
		src := l - 1 - i
		m.Sigma[i] = math.Sqrt(max(vals[src], 0))
		m.U[i] = make([]float64, m.L)
		for s := range S {
			m.g[s][i] = make([]float64, m.K)
		}
		for j := range l {
			c := W.At(j, src)
			floats.AddScaled(m.U[i], c, Q[j])
			for s := range S {
				floats.AddScaled(m.g[s][i], c, Z[j][s])
			}
		}
		// Zₛ stored √wₛ·Hₛᵀuᵢ. The cache and the forecasts want Hₛᵀuᵢ.
		for s := range S {
			floats.Scale(1/sw[s], m.g[s][i])
		}
	}

	m.Err = make([]float64, k)
	m.forProbes(k, func(ws []*fftWork, buf, acc []float64, i int) {
		lam := m.Sigma[i] * m.Sigma[i]
		if lam == 0 {
			return
		}
		clear(acc)
		for s := range S {
			m.h[s].corr(ws[s], buf, m.g[s][i]) // Hₛ·(Hₛᵀuᵢ)
			floats.AddScaled(acc, m.Weights[s], buf)
		}
		floats.AddScaled(acc, -lam, m.U[i])
		m.Err[i] = floats.Norm(acc, 2) / lam
	})
	return nil
}

// xt sets dst[s] = √wₛ·Hₛᵀ·q.
func (m *MSSA) xt(ws []*fftWork, sw []float64, dst [][]float64, q []float64) {
	for s := range m.h {
		m.h[s].corr(ws[s], dst[s], q)
		floats.Scale(sw[s], dst[s])
	}
}

// xxtInto sets acc = Σₛ √wₛ·Hₛ·zₛ, with zₛ = √wₛ·Hₛᵀ·q already stored.
func (m *MSSA) xxtInto(ws []*fftWork, sw []float64, z [][]float64, acc, buf []float64) {
	m.h[0].corr(ws[0], acc, z[0])
	floats.Scale(sw[0], acc)
	for s := 1; s < len(m.h); s++ {
		m.h[s].corr(ws[s], buf, z[s])
		floats.AddScaled(acc, sw[s], buf)
	}
}

// forProbes calls fn(ws, buf, acc, j) for j < n on parallel workers. ws[s] is
// that worker's FFT state for channel s; buf and acc are private scratch of
// length L. hankel.forEach cannot be used directly: each product touches
// every channel, and a channel's FFT state is not safe to share.
func (m *MSSA) forProbes(n int, fn func(ws []*fftWork, buf, acc []float64, j int)) {
	for _, h := range m.h {
		h.grow(n)
	}
	nWorkers := min(len(m.h[0].pool), n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for t := range nWorkers {
		ws := make([]*fftWork, len(m.h))
		for s := range m.h {
			ws[s] = m.h[s].pool[t]
		}
		buf := make([]float64, m.L)
		acc := make([]float64, m.L)
		wg.Go(func() {
			for j := int(next.Add(1)) - 1; j < n; j = int(next.Add(1)) - 1 {
				fn(ws, buf, acc, j)
			}
		})
	}
	wg.Wait()
}

// Len returns the number of computed components.
func (m *MSSA) Len() int { return len(m.Sigma) }

// Channels returns the number of series.
func (m *MSSA) Channels() int { return len(m.x) }

// Reconstruct returns the sum of the given components, one series per channel.
// The result for channel s has length n.
func (m *MSSA) Reconstruct(group []int) ([][]float64, error) {
	if err := checkGroup(m.Len(), group); err != nil {
		return nil, fmt.Errorf("mssa: %w", err)
	}
	us := make([][]float64, len(group))
	for p, i := range group {
		us[p] = m.U[i]
	}
	out := make([][]float64, len(m.x))
	for s := range m.x {
		gs := make([][]float64, len(group))
		for p, i := range group {
			gs[p] = m.gvec(s, i)
		}
		out[s] = reconstructPairs(m.h[s], us, gs)
	}
	return out, nil
}

// WCorr returns the w-correlations of the first m components. The scalar
// product sums over time and over channels, with the Hankel anti-diagonal
// weights and the channel weights. The value is signed; a display of |ρ|
// matches the matrix py-ssa-lib prints. Near-zero |ρ| between groups means
// they separate.
func (m *MSSA) WCorr(nComp int) [][]float64 {
	nComp = min(nComp, m.Len())
	F := make([][][]float64, nComp)
	for i := range nComp {
		F[i], _ = m.Reconstruct([]int{i})
	}
	hw := m.h[0].w
	R := make([][]float64, nComp)
	for i := range nComp {
		R[i] = make([]float64, nComp)
		R[i][i] = 1
		for j := range i {
			R[i][j] = m.wcorr(F[i], F[j], hw)
			R[j][i] = R[i][j]
		}
	}
	return R
}

// gvec returns Hₛᵀuᵢ, computed on demand and cached.
func (m *MSSA) gvec(s, i int) []float64 {
	if m.g[s][i] == nil {
		m.g[s][i] = make([]float64, m.K)
		m.h[s].corr(m.h[s].pool[0], m.g[s][i], m.U[i])
	}
	return m.g[s][i]
}

func (m *MSSA) wcorr(a, b [][]float64, hw []float64) float64 {
	var ab, aa, bb float64
	for s := range a {
		ws := m.Weights[s]
		for t, wt := range hw {
			ab += wt * ws * a[s][t] * b[s][t]
			aa += wt * ws * a[s][t] * a[s][t]
			bb += wt * ws * b[s][t] * b[s][t]
		}
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// eigenBasis returns every eigenpair of the symmetric L×L lag-covariance,
// singular values descending, the same extraction full uses.
func eigenBasis(cov *mat.SymDense) (sigma []float64, basis [][]float64, err error) {
	L := cov.SymmetricDim()
	var es mat.EigenSym
	if !es.Factorize(cov, true) {
		return nil, nil, errors.New("eigendecomposition failed")
	}
	vals := es.Values(nil) // ascending
	var vecs mat.Dense
	es.VectorsTo(&vecs)
	sigma = make([]float64, L)
	basis = make([][]float64, L)
	for i := range L {
		src := L - 1 - i
		sigma[i] = math.Sqrt(max(vals[src], 0))
		basis[i] = mat.Col(nil, src, &vecs)
	}
	return sigma, basis, nil
}

func seriesWeights(nSeries int, weights []float64) ([]float64, error) {
	w := make([]float64, nSeries)
	if weights == nil {
		for s := range w {
			w[s] = 1
		}
		return w, nil
	}
	if len(weights) != nSeries {
		return nil, fmt.Errorf("mssa: %d weights for %d series", len(weights), nSeries)
	}
	for s, v := range weights {
		if !(v > 0) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("mssa: weight %d = %v, want a positive finite number", s, v)
		}
		w[s] = v
	}
	return w, nil
}
