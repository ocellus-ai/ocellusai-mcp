// Package ssa is Singular Spectrum Analysis of one series and of several
// series together (MSSA), with reconstruction of components, two forecasts,
// ESPRIT roots and an interpreted analysis of the decomposition. It is the
// library of the ssa and ssa_mv processors, ported from the user's ssa
// command (the CLI and CSV input/output are left out; the processors play
// their role).
//
// The series x of length n is embedded into its L×K trajectory matrix
// X[a][b] = x[a+b], K = n−L+1, split into rank-one elementary components
// σᵢ·uᵢ·vᵢᵀ, and grouped components are turned back into series by diagonal
// averaging. Typical groups: a slow trend (one or two leading components), a
// harmonic (a pair of close σ), noise (the rest).
//
// Window: about n/3 and a multiple of the main period; for VM metrics a day in
// points (288 at a 5-minute step, 1440 at a minute step) on ≥ 2 weeks of data.
// Bounded metrics (CPU 0–100%) go in the original scale and the groups that
// contain the level (component 0) are clipped afterwards; a logit transform
// instead of clipping distorts the daily wave into false harmonics.
//
// Two decompositions:
//
//   - full: eigendecomposition of the lag-covariance matrix C = X·Xᵀ (L×L),
//     built by a recurrence in O(L·K + L²). All L components, exact; their
//     sum is the series. O(L³) time and O(L²) memory.
//   - top-k: randomized subspace iteration (Halko, Martinsson, Tropp 2011) for
//     the k leading components only. Products with X and Xᵀ are correlations
//     with x and go through FFT in O(n log n), so neither X nor C is built:
//     memory O(n·k). Each component carries its accuracy
//     ‖X·Xᵀuᵢ − σᵢ²uᵢ‖/σᵢ² in Err.
//
// Reconstruction goes through FFT in both modes: the diagonal average of
// uᵢ·(Xᵀuᵢ)ᵀ is a convolution, O(n log n) per component instead of O(L·K).
//
// Analyze interprets a decomposition: the noise level and the floor it
// gives, what each leading component is (trend, harmonic with its period,
// slow cycle), suggested groups and findings; Analysis.Check describes and
// checks a set of groups. The noise is modelled as AR(1), so slow random
// wander is not taken for structure, and the floor is the largest σ such
// noise produces at this n and L (Monte Carlo SSA); a harmonic is a pair of
// neighbours with high w-correlation, its period comes from ESPRIT. The
// analysis of MSSA is the same one over every channel: the noise is fitted
// per channel and simulated with the channel weights, w-correlations and
// periods sum over the channels.
package ssa

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/mat"
)

// SSA is the decomposition of one series.
type SSA struct {
	L, K  int         // window length and K = n−L+1, L ≤ K
	Sigma []float64   // singular values in descending order: all L (full) or the leading k (top-k)
	U     [][]float64 // U[i] is the left singular vector uᵢ, length L
	Norm2 float64     // ‖X‖²_F, the sum of σᵢ² over all L components
	Err   []float64   // top-k only: ‖X·Xᵀuᵢ − σᵢ²uᵢ‖/σᵢ², the accuracy of component i

	iters int // top-k: power iterations
	x     []float64
	h     *hankel
	g     [][]float64 // cache of Xᵀuᵢ = σᵢ·vᵢ
}

// Decompose computes the SSA of x with window L: the k leading components by
// the randomized method, or all of them exactly when k = 0 or k is close to L.
// iters is the number of power iterations of the randomized method. A window
// larger than n/2 is replaced by n−L+1: the trajectory matrix is then
// transposed and the decomposition is the same.
func Decompose(x []float64, window, k, iters int) (*SSA, error) {
	L := window
	n := len(x)
	if n < 3 {
		return nil, errors.New("series shorter than 3 points")
	}
	if L < 2 || L > n-1 {
		return nil, fmt.Errorf("window L = %d must be in [2, %d]", L, n-1)
	}
	for j, v := range x {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("value %d is %v: SSA needs a series without gaps", j, v)
		}
	}
	if L > n-L+1 {
		L = n - L + 1
	}
	x = slices.Clone(x)
	h := newHankel(x, L)
	s := &SSA{L: L, K: h.K, x: x, h: h}
	for j, v := range x {
		s.Norm2 += h.w[j] * v * v
	}
	if k > 0 {
		if l := oversample(k); l < L {
			return s, s.topK(k, l, max(iters, 0))
		}
	}
	return s, s.full()
}

// oversample is the number of random vectors the randomized method iterates
// to get k components: the extra ones speed up the convergence of the k-th.
func oversample(k int) int { return k + max(10, k/2) }

// Len returns the number of computed components.
func (s *SSA) Len() int { return len(s.Sigma) }

// full computes all L components from the eigendecomposition of C = X·Xᵀ.
// The price is a squared condition number: components with σᵢ < ~1e-7·σ₀
// lose accuracy, which does not matter above the noise level.
func (s *SSA) full() error {
	sigma, U, err := eigenBasis(lagCov(s.x, s.L))
	if err != nil {
		return err
	}
	s.Sigma, s.U = sigma, U
	s.g = make([][]float64, s.L)
	return nil
}

// topK computes the k leading components by randomized subspace iteration with
// Rayleigh–Ritz extraction: an orthonormal basis Q of l > k random vectors is
// multiplied by X·Xᵀ iters times, re-orthonormalized after each product; the
// eigenpairs (λ, w) of the small matrix QᵀX·XᵀQ = ZᵀZ, Z = XᵀQ, then give
// σᵢ² = λᵢ, uᵢ = Q·wᵢ and, for free, Xᵀuᵢ = Z·wᵢ. The l − k extra vectors
// speed up the convergence of the k-th component.
func (s *SSA) topK(k, l, iters int) error {
	h := s.h
	s.iters = iters
	rng := rand.New(rand.NewPCG(1, 2))
	Q := make([][]float64, l)
	Z := make([][]float64, l)
	for j := range Q {
		Q[j] = make([]float64, s.L)
		for a := range Q[j] {
			Q[j][a] = rng.NormFloat64()
		}
		Z[j] = make([]float64, s.K)
	}
	orthonormalize(Q, rng)
	for range iters {
		h.forEach(l, func(w *fftWork, j int) {
			h.corr(w, Z[j], Q[j]) // Z = Xᵀ·Q
			h.corr(w, Q[j], Z[j]) // Q = X·Z
		})
		orthonormalize(Q, rng)
	}
	h.forEach(l, func(w *fftWork, j int) { h.corr(w, Z[j], Q[j]) })

	B := mat.NewSymDense(l, nil)
	for i := range l {
		for j := i; j < l; j++ {
			B.SetSym(i, j, floats.Dot(Z[i], Z[j]))
		}
	}
	var es mat.EigenSym
	if !es.Factorize(B, true) {
		return errors.New("eigendecomposition failed")
	}
	vals := es.Values(nil) // ascending
	var W mat.Dense
	es.VectorsTo(&W)

	s.Sigma = make([]float64, k)
	s.U = make([][]float64, k)
	s.g = make([][]float64, k)
	for i := range k {
		src := l - 1 - i
		s.Sigma[i] = math.Sqrt(max(vals[src], 0))
		s.U[i] = make([]float64, s.L)
		s.g[i] = make([]float64, s.K)
		for j := range l {
			c := W.At(j, src)
			floats.AddScaled(s.U[i], c, Q[j])
			floats.AddScaled(s.g[i], c, Z[j])
		}
	}

	s.Err = make([]float64, k)
	h.forEach(k, func(w *fftWork, i int) {
		lam := s.Sigma[i] * s.Sigma[i]
		if lam == 0 {
			return
		}
		r := make([]float64, s.L)
		h.corr(w, r, s.g[i]) // X·Xᵀuᵢ
		floats.AddScaled(r, -lam, s.U[i])
		s.Err[i] = floats.Norm(r, 2) / lam
	})
	return nil
}

// orthonormalize makes the vectors orthonormal by modified Gram–Schmidt
// applied twice ("twice is enough"). A vector that vanishes, as for a series
// of rank below len(Q), is replaced by a random one.
func orthonormalize(basis [][]float64, rng *rand.Rand) {
	for j, q := range basis {
		for range 10 {
			norm0 := floats.Norm(q, 2)
			for range 2 {
				for _, p := range basis[:j] {
					floats.AddScaled(q, -floats.Dot(p, q), p)
				}
			}
			if norm := floats.Norm(q, 2); norm > 1e-10*norm0 {
				floats.Scale(1/norm, q)
				break
			}
			for a := range q {
				q[a] = rng.NormFloat64()
			}
		}
	}
}

// gvec returns Xᵀuᵢ = σᵢ·vᵢ, computed on demand and cached.
func (s *SSA) gvec(i int) []float64 {
	if s.g[i] == nil {
		s.g[i] = make([]float64, s.K)
		s.h.corr(s.h.pool[0], s.g[i], s.U[i])
	}
	return s.g[i]
}

// Reconstruct returns the sum of the given components as a series: the
// diagonal average of Σᵢ uᵢ·(Xᵀuᵢ)ᵀ = Σᵢ σᵢ·uᵢ·vᵢᵀ. The anti-diagonal sums of
// uᵢ·gᵢᵀ are the convolution uᵢ ∗ gᵢ, so the whole group costs one FFT pair
// per component and one inverse FFT. The sum over all L components of the full
// decomposition is the original series.
func (s *SSA) Reconstruct(group []int) []float64 {
	us := make([][]float64, len(group))
	gs := make([][]float64, len(group))
	for p, i := range group {
		us[p], gs[p] = s.U[i], s.gvec(i)
	}
	return reconstructPairs(s.h, us, gs)
}

// WCorr returns the w-correlations of the first m components:
// ρ = ⟨Fᵢ,Fⱼ⟩_w / (‖Fᵢ‖_w·‖Fⱼ‖_w) with weights wₖ = the number of elements on
// anti-diagonal k. Near-zero |ρ| between groups means they are well separated;
// the two components of a harmonic pair usually have a high |ρ|.
func (s *SSA) WCorr(m int) [][]float64 {
	m = min(m, s.Len())
	F := make([][]float64, m)
	for i := range F {
		F[i] = s.Reconstruct([]int{i})
	}
	R := make([][]float64, m)
	for i := range R {
		R[i] = make([]float64, m)
		for j := range i + 1 {
			R[i][j] = wcorr(s.h.w, F[i], F[j])
			R[j][i] = R[i][j]
		}
	}
	return R
}

// wcorr returns the w-correlation of two series of length n with the
// anti-diagonal weights hw; 0 if either is zero.
func wcorr(hw, a, b []float64) float64 {
	var ab, aa, bb float64
	for j, w := range hw {
		ab += w * a[j] * b[j]
		aa += w * a[j] * a[j]
		bb += w * b[j] * b[j]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// lagCov returns C = X·Xᵀ (L×L) for the trajectory matrix X of x without
// building X: C[i][j] = Σ_{k<K} x[i+k]·x[j+k]. Each diagonal j − i = d starts
// with a direct sum, O(K), and continues with the recurrence
// C[i+1][j+1] = C[i][j] − x[i]·x[j] + x[i+K]·x[j+K], O(1). Total O(L·K + L²)
// instead of O(L²·K) for the product.
func lagCov(x []float64, window int) *mat.SymDense {
	L := window
	K := len(x) - L + 1
	C := mat.NewSymDense(L, nil)
	for d := range L {
		var c float64
		for k := range K {
			c += x[k] * x[d+k]
		}
		C.SetSym(0, d, c)
		for i := 0; i+d+1 < L; i++ {
			c += x[i+K]*x[i+d+K] - x[i]*x[i+d]
			C.SetSym(i+1, i+d+1, c)
		}
	}
	return C
}
