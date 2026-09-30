package ssa

// The approximate SVD used by the tool (randomized subspace iteration in
// SSA.topK, reached from Decompose when k > 0) against a full thin SVD of the
// same trajectory matrix from gonum.org/v1/gonum/mat.SVD.
//
// The reference builds X[a][b] = x[a+b] explicitly and factors it as U Σ Vᵀ.
// Nothing in ssa.go is modified. The test is package main so it can call topK
// with an oversampling l other than the one Decompose hard-wires,
// l = k + max(10, k/2).
//
// What is compared, for the leading k components:
//
//   - singular values, relative to gonum, ignoring the numerical tail
//     σ < 1e-8·σ₀ where a relative figure is meaningless
//   - energy deficit (Σ σ_gonum² − Σ σ_approx²) / Σ σ_gonum² over those k
//     values. By the Ky Fan theorem the Ritz values sit below the true ones,
//     so this is the fraction of Frobenius energy the approximation misses.
//     It stays meaningful when a close pair is rotated inside its subspace.
//   - sine of the largest principal angle between left-singular subspaces,
//     on the whole k and on the numerical signal rank (σ > 1e-8·σ₀).
//     Past the true rank the extra vectors are an arbitrary basis of the
//     near-nullspace and need not match gonum vector for vector.
//   - |cos| of individual left and right vectors (sign-invariant). A close
//     pair can score poorly here while the pair's sum is exact.
//   - the SSA reconstruction (diagonal average) against the same average of
//     the gonum factors, per component and for the sum
//   - the script's own Err against a dense Xᵀu, to separate FFT matvec error
//     from the subspace iteration error

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/mat"
)

const (
	// Components below this fraction of σ₀ are the numerical tail.
	tailFrac = 1e-8
	// Consecutive σ closer than this are one cluster (a harmonic pair, or a
	// tighter multiplet). 2% is wider than roundoff and tighter than the gap
	// between a trend and the first harmonic in the fixtures below.
	clusterSep = 0.02
	// A drop by this factor is the cut between structure and a flat plateau.
	// The head is everything before the last such drop in the leading k.
	headGapMin = 5.0
)

// spec is one series and the window / component count used to compare.
type spec struct {
	name string
	x    []float64
	L, k int
}

func fixtures() []spec {
	return []spec{
		{name: "constant", x: constant(160), L: 40, k: 3},
		{name: "sine", x: sine(320, 16), L: 64, k: 4},
		{name: "tones", x: tones(576, 48, 0.05, 1), L: 96, k: 8},
		{name: "vm", x: tones(2016, 288, 0.4, 2), L: 288, k: 30},
		{name: "white", x: ar1(480, 0, 3), L: 60, k: 10},
		{name: "red", x: ar1(640, 0.9, 4), L: 80, k: 10},
		{name: "slow", x: slowDecay(640), L: 80, k: 15},
	}
}

func constant(n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = 1
	}
	return x
}

func sine(n int, period float64) []float64 {
	x := make([]float64, n)
	for t := range x {
		x[t] = math.Sin(2 * math.Pi * float64(t) / period)
	}
	return x
}

// tones is a level, a slow drift, a fundamental and two harmonics, plus white
// noise. period is in points. This is the shape the tool is aimed at (a VM
// metric with a daily wave); vm uses period 288, n = 2016.
func tones(n int, period, noise float64, seed uint64) []float64 {
	rng := rand.New(rand.NewPCG(seed, 9))
	x := make([]float64, n)
	for t := range x {
		tt := float64(t)
		x[t] = 40 + 0.004*tt +
			8*math.Sin(2*math.Pi*tt/period) +
			3*math.Sin(4*math.Pi*tt/period) +
			1.5*math.Sin(6*math.Pi*tt/period) +
			noise*rng.NormFloat64()
	}
	return x
}

// slowDecay has no spectral gap: twenty sines with amplitudes 1/j.
func slowDecay(n int) []float64 {
	x := make([]float64, n)
	for j := 1; j <= 20; j++ {
		amp := 1 / float64(j)
		p := float64(7 + 3*j)
		for t := range x {
			x[t] += amp * math.Sin(2*math.Pi*float64(t)/p+float64(j))
		}
	}
	return x
}

// svdRef is the thin gonum SVD of the trajectory matrix, singular values descending.
type svdRef struct {
	L, K  int
	s     []float64
	U, V  *mat.Dense // U is L×L, V is K×L (L ≤ K)
	X     *mat.Dense
	norm  float64 // ||X||²_F = Σ σ²
	svd   time.Duration
	build time.Duration
}

func trajectory(x []float64, window int) *mat.Dense {
	L := window
	n := len(x)
	K := n - L + 1
	data := make([]float64, L*K)
	for a := range L {
		copy(data[a*K:(a+1)*K], x[a:a+K])
	}
	return mat.NewDense(L, K, data)
}

func newRef(x []float64, window int) (*svdRef, error) {
	L := window
	t0 := time.Now()
	X := trajectory(x, L)
	build := time.Since(t0)
	var svd mat.SVD
	t1 := time.Now()
	if !svd.Factorize(X, mat.SVDThin) {
		return nil, fmt.Errorf("gonum SVD failed")
	}
	took := time.Since(t1)
	Ldim, K := X.Dims()
	s := svd.Values(nil)
	var U, V mat.Dense
	svd.UTo(&U)
	svd.VTo(&V)
	var norm float64
	for _, v := range s {
		norm += v * v
	}
	return &svdRef{L: Ldim, K: K, s: s, U: &U, V: &V, X: X, norm: norm, svd: took, build: build}, nil
}

// series is the diagonal average of Σ_{i in comp} σᵢ uᵢ vᵢᵀ.
func (r *svdRef) series(comp []int) []float64 {
	n := r.L + r.K - 1
	y := make([]float64, n)
	um, vm := r.U.RawMatrix(), r.V.RawMatrix()
	for _, i := range comp {
		si := r.s[i]
		for a := range r.L {
			ua := um.Data[a*um.Stride+i] * si
			for b := range r.K {
				y[a+b] += ua * vm.Data[b*vm.Stride+i]
			}
		}
	}
	for j := range y {
		y[j] /= float64(min(j+1, r.L, r.K, n-j))
	}
	return y
}

func (r *svdRef) left(i int) []float64 {
	u := make([]float64, r.L)
	for a := range u {
		u[a] = r.U.At(a, i)
	}
	return u
}

func (r *svdRef) right(i int) []float64 {
	v := make([]float64, r.K)
	for b := range v {
		v[b] = r.V.At(b, i)
	}
	return v
}

// xtu computes Xᵀu with a dense product, the reference for the FFT matvec.
func (r *svdRef) xtu(u []float64) []float64 {
	rm := r.X.RawMatrix()
	g := make([]float64, r.K)
	for a := range r.L {
		row := rm.Data[a*rm.Stride : a*rm.Stride+r.K]
		ua := u[a]
		for b, xv := range row {
			g[b] += ua * xv
		}
	}
	return g
}

func prepareSSA(x []float64, window int) *SSA {
	L := window
	x = slicesClone(x)
	h := newHankel(x, L)
	s := &SSA{L: L, K: h.K, x: x, h: h}
	for j, v := range x {
		s.Norm2 += h.w[j] * v * v
	}
	return s
}

func slicesClone(x []float64) []float64 { return append([]float64(nil), x...) }

func runTopK(x []float64, window, k, l, iters int) (*SSA, error) {
	L := window
	s := prepareSSA(x, L)
	if err := s.topK(k, l, iters); err != nil {
		return nil, err
	}
	return s, nil
}

type compRow struct {
	i           int
	sg, sa      float64
	rel         float64
	cosU, cosV  float64
	recon       float64
	significant bool
	isolated    bool
}

type compResult struct {
	rows                          []compRow
	sumA, sumG                    float64
	maxRelSig, maxAbsTail         float64
	sinSignal, sinK               float64
	reconSum                      float64
	minCosAll, minCosIso, minCosV float64
	nIso                          int
	fftRel, maxErr                float64
	orthoOff, orthoNorm           float64
	signalM                       int
	notes                         []string
	// head is the leading run that sits above the last large gap in the top k
	// (ratio ≥ headGapMin). Past that gap the singular values are a plateau:
	// individual vectors there are not uniquely defined, and a comparison of
	// the whole k mixes that plateau into the components SSA would group.
	headOK             bool
	headM              int
	headGap, headRel   float64
	headSin, headRecon float64
}

// compareOpt selects the expensive checks. Cosines of the left vectors and the
// singular-value figures are always computed. Vectors also checks the right
// vector, the FFT matvec and per-component reconstructions. Recon is the
// summed diagonal average of all k components.
type compareOpt struct {
	vectors bool
	recon   bool
}

func relDiff(got, ref float64) float64 {
	den := math.Abs(ref)
	if den < 1e-30 {
		return math.Abs(got - ref)
	}
	return math.Abs(got-ref) / den
}

func relL2(a, b []float64) float64 {
	var num, den float64
	for i := range a {
		d := a[i] - b[i]
		num += d * d
		den += b[i] * b[i]
	}
	if den == 0 {
		return math.Sqrt(num)
	}
	return math.Sqrt(num / den)
}

func cosAbs(a, b []float64) float64 {
	na, nb := floats.Norm(a, 2), floats.Norm(b, 2)
	if na == 0 || nb == 0 {
		return 0
	}
	c := math.Abs(floats.Dot(a, b)) / (na * nb)
	if c > 1 {
		return 1
	}
	return c
}

func principalSin(a, b [][]float64) float64 {
	m := len(a)
	if m == 0 {
		return 0
	}
	G := mat.NewDense(m, m, nil)
	for i := range m {
		for j := range m {
			G.Set(i, j, floats.Dot(a[i], b[j]))
		}
	}
	var svd mat.SVD
	if !svd.Factorize(G, mat.SVDNone) {
		return math.NaN()
	}
	maxSin := 0.0
	for _, c := range svd.Values(nil) {
		c = math.Abs(c)
		if c > 1 {
			c = 1
		}
		if sn := math.Sqrt(1 - c*c); sn > maxSin {
			maxSin = sn
		}
	}
	return maxSin
}

func orthoErr(vecs [][]float64) (maxOff, maxNorm float64) {
	for i, u := range vecs {
		if d := math.Abs(floats.Norm(u, 2) - 1); d > maxNorm {
			maxNorm = d
		}
		for _, v := range vecs[i+1:] {
			if d := math.Abs(floats.Dot(u, v)); d > maxOff {
				maxOff = d
			}
		}
	}
	return maxOff, maxNorm
}

// headCut returns the number of leading components before the last drop of
// headGapMin or more. Gaps inside the numerical tail (σ ≤ 1e-8·σ₀) are
// ignored: two roundoff singular values can differ by many orders while both
// are zero for every practical purpose. ok is false when the leading k has
// no such drop.
func headCut(s []float64, k int) (m int, gap float64, ok bool) {
	k = min(k, len(s))
	if k == 0 || s[0] == 0 {
		return 0, 0, false
	}
	floor := tailFrac * s[0]
	for i := 0; i+1 < k; i++ {
		if s[i] <= floor {
			break
		}
		g := math.Inf(1)
		if s[i+1] > 0 {
			g = s[i] / s[i+1]
		}
		if g >= headGapMin {
			m, gap, ok = i+1, g, true
		}
	}
	return m, gap, ok
}

func signalCount(s []float64, k int) int {
	m := 0
	for m < k && s[m] > tailFrac*s[0] {
		m++
	}
	return m
}

func isIsolated(s []float64, i int) bool {
	if s[i] <= tailFrac*s[0] {
		return false
	}
	if i > 0 && (s[i-1]-s[i])/s[i-1] < clusterSep {
		return false
	}
	if i+1 < len(s) && s[i] > 0 && (s[i]-s[i+1])/s[i] < clusterSep {
		return false
	}
	return true
}

// compare measures the leading k components of s against the gonum SVD.
func compare(s *SSA, r *svdRef, k int, opt compareOpt) compResult {
	k = min(k, s.Len(), len(r.s))
	out := compResult{minCosAll: 1, minCosIso: 1, minCosV: 1, signalM: signalCount(r.s, k)}
	out.orthoOff, out.orthoNorm = orthoErr(s.U[:k])
	var ua, ur [][]float64
	for i := range k {
		sg, sa := r.s[i], s.Sigma[i]
		out.sumG += sg * sg
		out.sumA += sa * sa
		row := compRow{i: i, sg: sg, sa: sa, rel: relDiff(sa, sg), significant: sg > tailFrac*r.s[0], isolated: isIsolated(r.s, i)}
		if row.significant {
			if row.rel > out.maxRelSig {
				out.maxRelSig = row.rel
			}
		} else if d := math.Abs(sa - sg); d > out.maxAbsTail {
			out.maxAbsTail = d
		}
		gu := r.left(i)
		row.cosU = cosAbs(s.U[i], gu)
		if row.significant && row.cosU < out.minCosAll {
			out.minCosAll = row.cosU
		}
		if row.isolated {
			out.nIso++
			if row.cosU < out.minCosIso {
				out.minCosIso = row.cosU
			}
		}
		if opt.vectors {
			gv := s.gvec(i)
			row.cosV = cosAbs(gv, r.right(i))
			row.recon = relL2(s.Reconstruct([]int{i}), r.series([]int{i}))
			if row.significant {
				dense := r.xtu(s.U[i])
				if fr := relL2(gv, dense); fr > out.fftRel {
					out.fftRel = fr
				}
				if row.cosV < out.minCosV {
					out.minCosV = row.cosV
				}
			}
		}
		// Err/σ² on a numerical zero is an empty ratio. Keep the figure for
		// components that actually carry energy.
		if row.significant && s.Err != nil && i < len(s.Err) && s.Err[i] > out.maxErr {
			out.maxErr = s.Err[i]
		}
		out.rows = append(out.rows, row)
		if k <= 40 {
			ua = append(ua, s.U[i])
			ur = append(ur, gu)
		}
	}
	if k <= 40 {
		out.sinK = principalSin(ua, ur)
		if m := out.signalM; m > 0 {
			out.sinSignal = principalSin(ua[:m], ur[:m])
		}
		if m, gap, ok := headCut(r.s, k); ok {
			out.headOK, out.headM, out.headGap = true, m, gap
			out.headSin = principalSin(ua[:m], ur[:m])
			for i := range m {
				if out.rows[i].rel > out.headRel {
					out.headRel = out.rows[i].rel
				}
			}
			if opt.recon {
				out.headRecon = relL2(s.Reconstruct(seq(m)), r.series(seq(m)))
			}
		}
	}
	if opt.recon {
		out.reconSum = relL2(s.Reconstruct(seq(k)), r.series(seq(k)))
	}
	if opt.vectors {
		out.notes = clusterNotes(s, r, k)
	}
	return out
}

// clusterNotes flags close-σ groups whose individual vectors are rotated but
// whose summed reconstruction matches gonum.
func clusterNotes(s *SSA, r *svdRef, k int) []string {
	var notes []string
	for lo := 0; lo < k; {
		hi := lo + 1
		for hi < k && r.s[lo] > tailFrac*r.s[0] && r.s[hi] > tailFrac*r.s[0] && (r.s[hi-1]-r.s[hi])/r.s[hi-1] < clusterSep {
			hi++
		}
		if hi-lo >= 2 {
			idx := seq(hi)[lo:]
			indiv := 0.0
			worst := 1.0
			for i := lo; i < hi; i++ {
				c := cosAbs(s.U[i], r.left(i))
				if c < worst {
					worst = c
				}
				if rr := relL2(s.Reconstruct([]int{i}), r.series([]int{i})); rr > indiv {
					indiv = rr
				}
			}
			group := relL2(s.Reconstruct(idx), r.series(idx))
			if indiv > 1e-4 && indiv > 50*max(group, 1e-16) {
				notes = append(notes, fmt.Sprintf("components %d-%d are a close-σ cluster (σ from %.6g to %.6g): worst individual |cos u| = %.4f, individual reconstruction rel = %.2e, but the sum of the cluster rel = %.2e. The basis is rotated inside the subspace; a group that keeps the cluster together is unaffected.",
					lo, hi-1, r.s[lo], r.s[hi-1], worst, indiv, group))
			}
		}
		lo = hi
	}
	return notes
}

func energyDeficit(c compResult) float64 {
	if c.sumG == 0 {
		return 0
	}
	return (c.sumG - c.sumA) / c.sumG
}

func (c compResult) line() string {
	iso := "n/a"
	if c.nIso > 0 {
		iso = fmt.Sprintf("%.6f", c.minCosIso)
	}
	s := fmt.Sprintf("energyDef=%.3e  maxRelσ=%.3e  sinNum=%.3e  sinK=%.3e  reconSum=%.3e  min|cos|sig=%.6f  min|cos|isol=%s  maxErr=%.3e  fft=%.3e  orthoOff=%.1e",
		energyDeficit(c), c.maxRelSig, c.sinSignal, c.sinK, c.reconSum, c.minCosAll, iso, c.maxErr, c.fftRel, c.orthoOff)
	if c.headOK {
		s += fmt.Sprintf("\n         head: first %d (last gap σ[%d]/σ[%d] = %.2f)  maxRelσ=%.3e  sin=%.3e  recon=%.3e",
			c.headM, c.headM-1, c.headM, c.headGap, c.headRel, c.headSin, c.headRecon)
	}
	return s
}

func formatThree(full, top []compRow) string {
	var b strings.Builder
	n := min(len(full), len(top))
	fmt.Fprintf(&b, "  %4s %13s %13s %13s %10s %10s %8s %8s %10s\n",
		"i", "σ_gonum", "σ_full", "σ_topk", "rel_full", "rel_topk", "|cos|f", "|cos|k", "recon_k")
	for i := range n {
		f, k := full[i], top[i]
		fmt.Fprintf(&b, "  %4d %13.6g %13.6g %13.6g %10.3e %10.3e %8.6f %8.6f %10.3e\n",
			i, f.sg, f.sa, k.sa, f.rel, k.rel, f.cosU, k.cosU, k.recon)
	}
	return b.String()
}

// bandReport summarises full-vs-gonum over the whole spectrum, which is too
// long to print row by row when L = 288.
func bandReport(c compResult) string {
	type band struct {
		name   string
		lo, hi float64
		n      int
		maxRel float64
		minCos float64
	}
	bands := []band{
		{"σ/σ₀ ≥ 1e-2", 1e-2, 2, 0, 0, 1},
		{"1e-4 .. 1e-2", 1e-4, 1e-2, 0, 0, 1},
		{"1e-6 .. 1e-4", 1e-6, 1e-4, 0, 0, 1},
		{"1e-8 .. 1e-6", tailFrac, 1e-6, 0, 0, 1},
		{"σ/σ₀ < 1e-8", 0, tailFrac, 0, 0, 1},
	}
	sg0 := c.rows[0].sg
	for _, row := range c.rows {
		frac := row.sg / sg0
		for i := range bands {
			if frac >= bands[i].lo && frac < bands[i].hi {
				bands[i].n++
				if row.rel > bands[i].maxRel {
					bands[i].maxRel = row.rel
				}
				if row.cosU > 0 && row.cosU < bands[i].minCos {
					bands[i].minCos = row.cosU
				}
				break
			}
		}
	}
	var b strings.Builder
	b.WriteString("  full eigen vs gonum, by size of σ:\n")
	for _, bd := range bands {
		if bd.n == 0 {
			continue
		}
		fmt.Fprintf(&b, "    %-16s  n=%4d  max rel σ = %.3e  min |cos u| = %8.6f\n", bd.name, bd.n, bd.maxRel, bd.minCos)
	}
	return b.String()
}

func header(sp spec, r *svdRef) string {
	return fmt.Sprintf("%s  n=%d  L=%d  K=%d  k=%d  l=%d (script oversampling)  ||X||_F=%.6g  gonum SVD %s (matrix build %s)",
		sp.name, len(sp.x), sp.L, r.K, sp.k, oversample(sp.k), math.Sqrt(r.norm), r.svd.Round(time.Microsecond), r.build.Round(time.Microsecond))
}

func TestSVDApproximationAgainstGonum(t *testing.T) {
	var all []spec
	refs := map[string]*svdRef{}
	for _, sp := range fixtures() {
		if sp.L > len(sp.x)-sp.L+1 {
			t.Fatalf("%s: fixture has L > K, Decompose would silently shrink the window", sp.name)
		}
		if oversample(sp.k) >= sp.L {
			t.Fatalf("%s: l = %d is not below L = %d, Decompose would take the full path", sp.name, oversample(sp.k), sp.L)
		}
		r, err := newRef(sp.x, sp.L)
		if err != nil {
			t.Fatal(sp.name, err)
		}
		refs[sp.name] = r
		all = append(all, sp)
	}

	t.Run("harness", func(t *testing.T) {
		testHarness(t, all, refs)
	})
	t.Run("three-way", func(t *testing.T) {
		testThreeWay(t, all, refs)
	})
	t.Run("convergence", func(t *testing.T) {
		testConvergence(t, all, refs)
	})
}

// testHarness checks the reference itself: Frobenius identity, exact constant
// singular value, and that diagonal averaging of the full gonum SVD returns x.
func testHarness(t *testing.T, all []spec, refs map[string]*svdRef) {
	sp := all[0]
	r := refs["constant"]
	want := math.Sqrt(float64(sp.L * (len(sp.x) - sp.L + 1)))
	if d := relDiff(r.s[0], want); d > 1e-12 {
		t.Errorf("constant: gonum σ₀ = %.16g, analytical sqrt(L·K) = %.16g, rel %.3e", r.s[0], want, d)
	}
	if r.s[1] > 1e-10*r.s[0] {
		t.Errorf("constant: gonum σ₁ = %.3e, a constant series has rank 1", r.s[1])
	}

	for _, sp := range all {
		r := refs[sp.name]
		sFull, err := Decompose(sp.x, sp.L, 0, 0)
		if err != nil {
			t.Fatal(sp.name, err)
		}
		if sFull.Err != nil {
			t.Fatalf("%s: k = 0 must take the full eigen path", sp.name)
		}
		if d := relDiff(sFull.Norm2, r.norm); d > 1e-8 {
			t.Errorf("%s: ||X||² from weights = %.16g, from gonum Σσ² = %.16g, rel %.3e", sp.name, sFull.Norm2, r.norm, d)
		}
		var sumE float64
		for _, sig := range sFull.Sigma {
			sumE += sig * sig
		}
		if d := relDiff(sumE, r.norm); d > 1e-8 {
			t.Errorf("%s: Σσ² of the full eigen = %.16g, gonum = %.16g, rel %.3e", sp.name, sumE, r.norm, d)
		}
		yG := r.series(seq(r.L))
		yE := sFull.Reconstruct(seq(sFull.Len()))
		if d := relL2(yG, sp.x); d > 1e-8 {
			t.Errorf("%s: diagonal average of the full gonum SVD vs the series, rel L2 = %.3e", sp.name, d)
		}
		if d := relL2(yE, sp.x); d > 1e-6 {
			t.Errorf("%s: full eigen reconstruction vs the series, rel L2 = %.3e", sp.name, d)
		}
		t.Logf("%s  ||X||² weights/gonum rel %.3e   Σσ²_eigen/gonum rel %.3e   hankelize(gonum) rel %.3e   hankelize(eigen) rel %.3e",
			sp.name, relDiff(sFull.Norm2, r.norm), relDiff(sumE, r.norm), relL2(yG, sp.x), relL2(yE, sp.x))
	}
}

func testThreeWay(t *testing.T, all []spec, refs map[string]*svdRef) {
	for _, sp := range all {
		r := refs[sp.name]
		t0 := time.Now()
		sFull, err := Decompose(sp.x, sp.L, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		tFull := time.Since(t0)
		t0 = time.Now()
		sTop, err := Decompose(sp.x, sp.L, sp.k, 8)
		if err != nil {
			t.Fatal(err)
		}
		tTop := time.Since(t0)
		if sTop.Err == nil || sTop.Len() != sp.k {
			t.Fatalf("%s: Decompose(k=%d, iter=8) did not take top-k (len %d, err-nil %v)", sp.name, sp.k, sTop.Len(), sTop.Err == nil)
		}
		// Same arithmetic as a direct topK call with the script's l: guards the
		// copy of the oversampling formula used by the convergence grid.
		sDirect, err := runTopK(sp.x, sp.L, sp.k, oversample(sp.k), 8)
		if err != nil {
			t.Fatal(err)
		}
		for i := range sp.k {
			if d := relDiff(sDirect.Sigma[i], sTop.Sigma[i]); d > 1e-12 {
				t.Errorf("%s: direct topK σ[%d] differs from Decompose by %.3e", sp.name, i, d)
				break
			}
		}

		opt := compareOpt{vectors: true, recon: true}
		cFull := compare(sFull, r, sp.k, opt)
		cTop := compare(sTop, r, sp.k, opt)
		// The rest of the spectrum: singular values and left-vector cosines only.
		cAll := compare(sFull, r, sFull.Len(), compareOpt{})

		var b strings.Builder
		fmt.Fprintf(&b, "\n%s\n  time: full eigen %s, top-%d iter=8 %s, gonum SVD %s\n",
			header(sp, r), tFull.Round(time.Microsecond), sp.k, tTop.Round(time.Microsecond), r.svd.Round(time.Microsecond))
		b.WriteString(formatThree(cFull.rows, cTop.rows))
		fmt.Fprintf(&b, "  full  %s\n  topk  %s\n", cFull.line(), cTop.line())
		if sFull.Len() > sp.k {
			b.WriteString(bandReport(cAll))
		}
		for _, n := range cTop.notes {
			fmt.Fprintf(&b, "  note: %s\n", n)
		}
		for _, n := range cFull.notes {
			fmt.Fprintf(&b, "  note full: %s\n", n)
		}
		t.Log(b.String())

		// Exact-rank cases must agree with gonum well past grouping precision.
		if sp.name == "constant" || sp.name == "sine" {
			if cTop.maxRelSig > 1e-6 || cFull.maxRelSig > 1e-6 {
				t.Errorf("%s: significant σ rel to gonum, full %.3e topk %.3e, want ≤ 1e-6", sp.name, cFull.maxRelSig, cTop.maxRelSig)
			}
			if cTop.sinSignal > 1e-6 || cFull.sinSignal > 1e-6 {
				t.Errorf("%s: signal-subspace sin θ, full %.3e topk %.3e, want ≤ 1e-6", sp.name, cFull.sinSignal, cTop.sinSignal)
			}
			if cTop.reconSum > 1e-6 || cFull.reconSum > 1e-6 {
				t.Errorf("%s: summed reconstruction rel, full %.3e topk %.3e, want ≤ 1e-6", sp.name, cFull.reconSum, cTop.reconSum)
			}
		}
		// Above the noise plateau the default top-k settings reproduce gonum.
		// The plateau itself is compared in the table, not gated: its vectors
		// are interchangeable and are not what a grouping keeps.
		if sp.name == "vm" || sp.name == "tones" {
			if !cTop.headOK {
				t.Errorf("%s: expected a spectral gap of %.0f or more in the leading %d", sp.name, headGapMin, sp.k)
			} else if cTop.headRel > 1e-8 || cTop.headSin > 1e-6 || cTop.headRecon > 1e-8 {
				t.Errorf("%s: top-k head (m=%d) vs gonum, rel σ %.3e sin %.3e recon %.3e",
					sp.name, cTop.headM, cTop.headRel, cTop.headSin, cTop.headRecon)
			}
			if cTop.fftRel > 1e-8 {
				t.Errorf("%s: FFT Xᵀu vs dense rel %.3e, the matvec is part of the approximation", sp.name, cTop.fftRel)
			}
		}
	}
}

func testConvergence(t *testing.T, all []spec, refs map[string]*svdRef) {
	want := map[string]bool{"sine": true, "vm": true, "white": true, "slow": true}
	var b strings.Builder
	b.WriteString("\nconvergence at the script oversampling l = k+max(10,k/2).\n")
	b.WriteString("  sinNum is the subspace of every σ > 1e-8·σ₀ inside the requested k, so on a flat spectrum it is all k.\n")
	b.WriteString("  head is the run before the last drop of 5× or more; reconHead is that run's diagonal average against gonum.\n")
	fmt.Fprintf(&b, "  %-10s %4s %4s %4s %12s %12s %12s %12s %12s %5s %12s\n",
		"case", "k", "l", "it", "energyDef", "maxRelσ", "sinNum", "reconSum", "maxErr", "head", "reconHead")
	for _, sp := range all {
		if !want[sp.name] {
			continue
		}
		r := refs[sp.name]
		l := oversample(sp.k)
		for _, it := range []int{0, 1, 2, 4, 8, 16} {
			s, err := runTopK(sp.x, sp.L, sp.k, l, it)
			if err != nil {
				t.Fatal(sp.name, err)
			}
			c := compare(s, r, sp.k, compareOpt{recon: true})
			head, reconH := "·", "·"
			if c.headOK {
				head = strconv.Itoa(c.headM)
				reconH = fmt.Sprintf("%12.3e", c.headRecon)
			}
			fmt.Fprintf(&b, "  %-10s %4d %4d %4d %12.3e %12.3e %12.3e %12.3e %12.3e %5s %12s\n",
				sp.name, sp.k, l, it, energyDeficit(c), c.maxRelSig, c.sinSignal, c.reconSum, c.maxErr, head, reconH)
			if sp.name == "sine" && it >= 1 && (c.headRecon > 1e-8 || c.headSin > 1e-6) {
				t.Errorf("sine iter=%d: head recon %.3e sin %.3e", it, c.headRecon, c.headSin)
			}
			if sp.name == "vm" && it >= 2 && c.headOK && (c.headRecon > 1e-6 || c.headRel > 1e-6) {
				t.Errorf("vm iter=%d: head m=%d rel σ %.3e recon %.3e", it, c.headM, c.headRel, c.headRecon)
			}
		}
	}

	b.WriteString("\noversampling at the same k. l=k is subspace iteration with no extra vectors; l=script is what Decompose runs.\n")
	fmt.Fprintf(&b, "  %-10s %4s %4s %4s %12s %12s %12s %12s\n",
		"case", "k", "l", "it", "energyDef", "maxRelσ", "sinSignal", "reconSum")
	for _, sp := range all {
		if !want[sp.name] {
			continue
		}
		r := refs[sp.name]
		ls := []int{sp.k, sp.k + 2, oversample(sp.k)}
		for _, it := range []int{0, 8} {
			for _, l := range ls {
				if l >= sp.L {
					continue
				}
				s, err := runTopK(sp.x, sp.L, sp.k, l, it)
				if err != nil {
					t.Fatal(err)
				}
				c := compare(s, r, sp.k, compareOpt{recon: true})
				label := "k"
				if l == sp.k+2 {
					label = "k+2"
				}
				if l == oversample(sp.k) {
					label = "script"
				}
				fmt.Fprintf(&b, "  %-10s %4d %4s %4d %12.3e %12.3e %12.3e %12.3e\n",
					sp.name, sp.k, label, it, energyDeficit(c), c.maxRelSig, c.sinSignal, c.reconSum)
			}
		}
	}
	t.Log(b.String())
}
