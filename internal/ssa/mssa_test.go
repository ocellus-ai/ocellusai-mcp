package ssa

import (
	"math"
	"slices"
	"testing"
)

func geom(n int, ratio, scale float64) []float64 {
	x := make([]float64, n)
	p := scale
	for t := range x {
		x[t] = p
		p *= ratio
	}
	return x
}

func TestMSSAValidation(t *testing.T) {
	x := sine(20, 5)
	cases := []struct {
		name   string
		series [][]float64
		L      int
		w      []float64
	}{
		{"no series", nil, 5, nil},
		{"short", [][]float64{{1, 2}}, 2, nil},
		{"unequal", [][]float64{x, sine(19, 5)}, 5, nil},
		{"L low", [][]float64{x}, 1, nil},
		{"L high", [][]float64{x}, 11, nil},
		{"weights", [][]float64{x, x}, 5, []float64{1}},
		{"weight sign", [][]float64{x}, 5, []float64{-1}},
	}
	for _, c := range cases {
		if _, err := DecomposeMulti(c.series, c.L, 0, 0, c.w); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
	nan := slices.Clone(x)
	nan[3] = math.NaN()
	if _, err := DecomposeMulti([][]float64{nan}, 5, 0, 0, nil); err == nil {
		t.Error("NaN: expected an error")
	}
}

func TestMSSAFullReconstruction(t *testing.T) {
	n, L := 40, 15
	series := [][]float64{
		sine(n, 10),
		make([]float64, n),
		make([]float64, n),
	}
	for t := range n {
		series[1][t] = 0.5 * math.Cos(2*math.Pi*float64(t)/8)
		series[2][t] = 1 + 0.02*float64(t)
	}
	weights := []float64{1, 0.6, 0.2}
	m, err := DecomposeMulti(series, L, 0, 0, weights)
	if err != nil {
		t.Fatal(err)
	}
	if m.Len() != L || m.Channels() != 3 || m.K != n-L+1 {
		t.Fatalf("L=%d K=%d channels=%d components=%d", m.L, m.K, m.Channels(), m.Len())
	}
	var sum float64
	for _, s := range m.Sigma {
		sum += s * s
	}
	if d := relDiff(sum, m.Norm2); d > 1e-8 {
		t.Errorf("sum of sigma^2 vs ||X||^2: rel %.3g", d)
	}
	got, err := m.Reconstruct(seq(m.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for s := range series {
		if d := relL2(got[s], series[s]); d > 1e-8 {
			t.Errorf("channel %d full reconstruction rel %.3g", s, d)
		}
	}
	// Uniform weights and an explicit vector of ones are the same decomposition.
	a, err := DecomposeMulti(series, L, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecomposeMulti(series, L, 0, 0, []float64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.Sigma {
		if d := relDiff(a.Sigma[i], b.Sigma[i]); d > 1e-12 {
			t.Errorf("sigma[%d] uniform vs ones: rel %.3g", i, d)
		}
	}
}

func TestMSSAOneSeriesMatchesDecompose(t *testing.T) {
	x := tones(120, 24, 0.05, 1)
	L := 30
	s, err := Decompose(x, L, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecomposeMulti([][]float64{x}, L, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Sigma {
		if d := relDiff(m.Sigma[i], s.Sigma[i]); d > 1e-10 {
			t.Errorf("sigma[%d]: mssa %.10g ssa %.10g rel %.3g", i, m.Sigma[i], s.Sigma[i], d)
		}
	}
	group := []int{0, 1, 2, 3}
	ys := s.Reconstruct(group)
	ym, err := m.Reconstruct(group)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(ym[0], ys); d > 1e-8 {
		t.Errorf("reconstruction rel %.3g", d)
	}
	ls, err := s.LForecast(group, 8)
	if err != nil {
		t.Fatal(err)
	}
	lm, err := m.LForecast(group, 8)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(lm[0], ls); d > 1e-8 {
		t.Errorf("L-forecast rel %.3g", d)
	}
	ks, err := s.KForecast(group, 8)
	if err != nil {
		t.Fatal(err)
	}
	km, err := m.KForecast(group, 8)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(km[0], ks); d > 1e-8 {
		t.Errorf("K-forecast rel %.3g", d)
	}
}

func TestExponentialForecast(t *testing.T) {
	const n, steps, L = 35, 5, 12
	const growth = 1.03
	x := geom(n, growth, 1)
	truth := geom(n+steps, growth, 1)

	s, err := Decompose(x, L, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Sigma comes from the eigenvalues of X·Xᵀ, so a zero singular value is
	// only resolved to about sqrt(eps)·sigma0 ≈ 1.5e-8·sigma0, and the exact
	// noise depends on the platform's rounding (amd64 vs arm64 with FMA).
	if s.Sigma[1] > 1e-6*s.Sigma[0] {
		t.Fatalf("geometric series is not rank 1: sigma1/sigma0 = %.3g", s.Sigma[1]/s.Sigma[0])
	}
	for _, name := range []string{"L", "K"} {
		var y []float64
		var ferr error
		if name == "L" {
			y, ferr = s.LForecast([]int{0}, steps)
		} else {
			y, ferr = s.KForecast([]int{0}, steps)
		}
		if ferr != nil {
			t.Fatal(ferr)
		}
		if d := relL2(y, truth); d > 1e-8 {
			t.Errorf("%s-forecast rel %.3g", name, d)
		}
	}

	// Two exponentials with different ratios span a shared 2-dimensional
	// column space. Both forecasts of that subspace continue both series.
	x2 := geom(n, 0.97, 3)
	m, err := DecomposeMulti([][]float64{x, x2}, L, 0, 0, []float64{1, 0.4})
	if err != nil {
		t.Fatal(err)
	}
	if m.Sigma[2] > 1e-6*m.Sigma[0] {
		t.Fatalf("two exponentials are not rank 2: sigma2/sigma0 = %.3g", m.Sigma[2]/m.Sigma[0])
	}
	truth2 := geom(n+steps, 0.97, 3)
	lf, err := m.LForecast([]int{0, 1}, steps)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := m.KForecast([]int{0, 1}, steps)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(lf[0], truth); d > 1e-6 {
		t.Errorf("MSSA L-forecast channel 0 rel %.3g", d)
	}
	if d := relL2(lf[1], truth2); d > 1e-6 {
		t.Errorf("MSSA L-forecast channel 1 rel %.3g", d)
	}
	if d := relL2(kf[0], truth); d > 1e-6 {
		t.Errorf("MSSA K-forecast channel 0 rel %.3g", d)
	}
	if d := relL2(kf[1], truth2); d > 1e-6 {
		t.Errorf("MSSA K-forecast channel 1 rel %.3g", d)
	}
}

func TestForecastGroupOrderAndRejection(t *testing.T) {
	x := sine(64, 8)
	s, err := Decompose(x, 16, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.LForecast([]int{0, 1}, 6)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.LForecast([]int{1, 0}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(a, b); d > 1e-12 {
		t.Errorf("L-forecast depends on group order, rel %.3g", d)
	}
	ak, err := s.KForecast([]int{0, 1}, 6)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := s.KForecast([]int{1, 0}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if d := relL2(ak, bk); d > 1e-12 {
		t.Errorf("K-forecast depends on group order, rel %.3g", d)
	}
	if _, err := s.LForecast(nil, 3); err == nil {
		t.Error("empty group: expected an error")
	}
	if _, err := s.KForecast([]int{0}, 0); err == nil {
		t.Error("steps 0: expected an error")
	}
	if _, err := s.LForecast([]int{0, 0}, 3); err == nil {
		t.Error("duplicate: expected an error")
	}
	if _, err := s.LForecast([]int{s.Len()}, 3); err == nil {
		t.Error("out of range: expected an error")
	}
	var nu2 float64
	for _, u := range s.U {
		p := u[s.L-1]
		nu2 += p * p
	}
	if math.Abs(nu2-1) > 1e-8 {
		t.Errorf("full basis nu^2 = %.12g, want 1", nu2)
	}
	if _, err := s.LForecast(seq(s.Len()), 1); err == nil {
		t.Errorf("full basis: L-recurrence expected to be undefined, nu^2 = %.12g", nu2)
	}
}

func absCos(a, b []float64) float64 {
	var ab, aa, bb float64
	for i := range a {
		ab += a[i] * b[i]
		aa += a[i] * a[i]
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return math.Abs(ab) / math.Sqrt(aa*bb)
}

// One channel of weight 1 must reproduce SSA.topK, including the vectors:
// same seed, same oversampling, same orthonormalize, same FFT products.
func TestMSSATopKMatchesDecompose(t *testing.T) {
	x := tones(160, 20, 0.01, 2)
	const L, k, iters = 48, 8, 4
	s, err := Decompose(x, L, k, iters)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecomposeMulti([][]float64{x}, L, k, iters, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Len() != k || s.Len() != k || m.Err == nil {
		t.Fatalf("len mssa %d ssa %d, err nil %v", m.Len(), s.Len(), m.Err == nil)
	}
	for i := range k {
		if d := relDiff(m.Sigma[i], s.Sigma[i]); d > 1e-9 {
			t.Errorf("sigma[%d] rel %.3g", i, d)
		}
		if c := absCos(m.U[i], s.U[i]); c < 1-1e-8 {
			t.Errorf("|cos| u[%d] = %.12g", i, c)
		}
		if d := relDiff(m.Err[i], s.Err[i]); d > 1e-6 && math.Abs(m.Err[i]-s.Err[i]) > 1e-12 {
			t.Errorf("err[%d] mssa %.3g ssa %.3g", i, m.Err[i], s.Err[i])
		}
	}

	// l = k+max(10, k/2) is no shorter than L, so this is the full path.
	wide, err := DecomposeMulti([][]float64{x}, 20, 10, iters, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wide.Len() != 20 || wide.Err != nil {
		t.Fatalf("fallback: len %d, err set %v", wide.Len(), wide.Err != nil)
	}
}

func TestMSSATopKAgreesWithFull(t *testing.T) {
	n := 180
	series := [][]float64{geom(n, 1.01, 1), geom(n, 0.99, 2), sine(n, 12)}
	weights := []float64{1, 0.3, 2}
	const L, k, iters = 40, 6, 8
	full, err := DecomposeMulti(series, L, 0, 0, weights)
	if err != nil {
		t.Fatal(err)
	}
	top, err := DecomposeMulti(series, L, k, iters, weights)
	if err != nil {
		t.Fatal(err)
	}
	if top.Len() != k {
		t.Fatalf("top-k returned %d components", top.Len())
	}
	for i := range 4 {
		if d := relDiff(top.Sigma[i], full.Sigma[i]); d > 1e-6 {
			t.Errorf("sigma[%d] rel %.3g, top %.8g full %.8g", i, d, top.Sigma[i], full.Sigma[i])
		}
		if top.Err[i] > 1e-6 {
			t.Errorf("err[%d] = %.3g", i, top.Err[i])
		}
	}
	group := []int{0, 1, 2, 3}
	yf, err := full.Reconstruct(group)
	if err != nil {
		t.Fatal(err)
	}
	yt, err := top.Reconstruct(group)
	if err != nil {
		t.Fatal(err)
	}
	for s := range series {
		if d := relL2(yt[s], yf[s]); d > 1e-6 {
			t.Errorf("channel %d reconstruction rel %.3g", s, d)
		}
	}
	lf, err := full.LForecast(group, 6)
	if err != nil {
		t.Fatal(err)
	}
	lt, err := top.LForecast(group, 6)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := full.KForecast(group, 6)
	if err != nil {
		t.Fatal(err)
	}
	kt, err := top.KForecast(group, 6)
	if err != nil {
		t.Fatal(err)
	}
	for s := range series {
		if d := relL2(lt[s], lf[s]); d > 1e-5 {
			t.Errorf("channel %d L-forecast rel %.3g", s, d)
		}
		if d := relL2(kt[s], kf[s]); d > 1e-5 {
			t.Errorf("channel %d K-forecast rel %.3g", s, d)
		}
	}
}

func TestWCorrShape(t *testing.T) {
	n := 48
	series := [][]float64{sine(n, 12), make([]float64, n), geom(n, 1, 1)}
	for t := range n {
		series[1][t] = math.Cos(2 * math.Pi * float64(t) / 12)
		series[2][t] = 0.02 * float64(t)
	}
	m, err := DecomposeMulti(series, 18, 0, 0, []float64{1, 0.7, 0.3})
	if err != nil {
		t.Fatal(err)
	}
	W := m.WCorr(8)
	for i := range W {
		if W[i][i] != 1 {
			t.Errorf("W[%d][%d] = %v, want 1", i, i, W[i][i])
		}
		for j := range W {
			if math.Abs(W[i][j]-W[j][i]) > 1e-12 {
				t.Errorf("W not symmetric at %d,%d", i, j)
			}
			if math.Abs(W[i][j]) > 1+1e-9 {
				t.Errorf("|W[%d][%d]| = %v > 1", i, j, W[i][j])
			}
		}
	}
}
