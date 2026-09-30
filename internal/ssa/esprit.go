// ESPRIT for a chosen group of SSA or MSSA components, the same estimate as
// py-ssa-lib's estimate_ESPRIT.
//
// P holds the selected left singular vectors as columns. One step of the
// window acts on this subspace by a matrix Φ,
//
//	P[:-1] · Φ ≈ P[1:],
//
// solved in the least-squares sense. The eigenvalues of Φ are the roots
// μ = ρ·e^{iω}. ρ is the growth per step and ω the angular frequency in
// radians: a pure harmonic is a conjugate pair on the unit circle (ρ = 1),
// a trend is a real root near 1. Roots are ordered by increasing |ω|, and by
// ω itself when the absolute values are equal.
//
// The forecast does not use these roots. LForecast builds its recurrence from
// the last row of P directly.

package ssa

import (
	"cmp"
	"fmt"
	"math"
	"math/cmplx"
	"slices"

	"gonum.org/v1/gonum/mat"
)

// Esprit is the rotational-invariance estimate of one component group.
type Esprit struct {
	Mu    []complex128 // roots μ, ordered by increasing |ω|
	Rho   []float64    // |μ|, growth per step
	Omega []float64    // arg(μ), radians, in (−π, π]
}

// Esprit estimates the roots of the subspace spanned by group.
func (s *SSA) Esprit(group []int) (Esprit, error) {
	return esprit(s.U, group)
}

// Esprit estimates the roots of the subspace spanned by group. MSSA uses the
// same left vectors as SSA: the shift acts along the window, not across channels.
func (m *MSSA) Esprit(group []int) (Esprit, error) {
	return esprit(m.U, group)
}

func esprit(vecs [][]float64, group []int) (Esprit, error) {
	if err := checkGroup(len(vecs), group); err != nil {
		return Esprit{}, fmt.Errorf("esprit: %w", err)
	}
	L := len(vecs[group[0]])
	if L < 2 {
		return Esprit{}, fmt.Errorf("esprit: window shorter than 2")
	}
	r := len(group)
	a := mat.NewDense(L-1, r, nil)
	b := mat.NewDense(L-1, r, nil)
	for p, comp := range group {
		u := vecs[comp]
		for i := 0; i < L-1; i++ {
			a.Set(i, p, u[i])
			b.Set(i, p, u[i+1])
		}
	}
	var phi mat.Dense
	if err := phi.Solve(a, b); err != nil {
		return Esprit{}, fmt.Errorf("esprit: shift equation: %w", err)
	}
	var eig mat.Eigen
	if !eig.Factorize(&phi, mat.EigenNone) {
		return Esprit{}, fmt.Errorf("esprit: eigenvalues failed")
	}
	mu := eig.Values(nil)
	slices.SortStableFunc(mu, func(p, q complex128) int {
		ap, aq := math.Abs(cmplx.Phase(p)), math.Abs(cmplx.Phase(q))
		if c := cmp.Compare(ap, aq); c != 0 {
			return c
		}
		return cmp.Compare(cmplx.Phase(p), cmplx.Phase(q))
	})
	out := Esprit{
		Mu:    mu,
		Rho:   make([]float64, len(mu)),
		Omega: make([]float64, len(mu)),
	}
	for i, z := range mu {
		out.Rho[i] = cmplx.Abs(z)
		out.Omega[i] = cmplx.Phase(z)
	}
	return out, nil
}
