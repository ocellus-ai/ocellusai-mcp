package ssa

import (
	"math"
	"testing"
)

func TestEspritSineAndExponential(t *testing.T) {
	const period = 12.0
	n := 72
	x := make([]float64, n)
	for i := range x {
		x[i] = math.Sin(2 * math.Pi * float64(i) / period)
	}
	s, err := Decompose(x, 30, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Esprit([]int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Mu) != 2 {
		t.Fatalf("sine: %d roots", len(e.Mu))
	}
	omega := 2 * math.Pi / period
	for i := range e.Mu {
		if d := math.Abs(math.Abs(e.Omega[i]) - omega); d > 1e-8 {
			t.Errorf("sine |ω|[%d] = %.12g, want %.12g", i, math.Abs(e.Omega[i]), omega)
		}
		if d := math.Abs(e.Rho[i] - 1); d > 1e-8 {
			t.Errorf("sine ρ[%d] = %.12g", i, e.Rho[i])
		}
	}
	// Conjugate order: negative angle first, since |ω| ties.
	if e.Omega[0] > 0 || e.Omega[1] < 0 {
		t.Errorf("sine angles %.6g, %.6g, want −ω then +ω", e.Omega[0], e.Omega[1])
	}

	const growth = 1.03
	g := geom(40, growth, 1)
	sg, err := Decompose(g, 12, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	eg, err := sg.Esprit([]int{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(eg.Mu) != 1 {
		t.Fatalf("exponential: %d roots", len(eg.Mu))
	}
	if d := math.Abs(eg.Rho[0] - growth); d > 1e-8 {
		t.Errorf("exponential ρ = %.12g, want %.12g", eg.Rho[0], growth)
	}
	if math.Abs(eg.Omega[0]) > 1e-8 {
		t.Errorf("exponential ω = %.3g, want 0", eg.Omega[0])
	}

	if _, err := s.Esprit(nil); err == nil {
		t.Error("empty group: expected an error")
	}
	if _, err := s.Esprit([]int{0, 0}); err == nil {
		t.Error("duplicate: expected an error")
	}
}

func TestEspritMSSASameFrequency(t *testing.T) {
	const n, period = 96, 12.0
	a := make([]float64, n)
	b := make([]float64, n)
	for i := range a {
		a[i] = math.Sin(2 * math.Pi * float64(i) / period)
		b[i] = math.Cos(2 * math.Pi * float64(i) / period)
	}
	m, err := DecomposeMulti([][]float64{a, b}, 36, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := m.Esprit([]int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	omega := 2 * math.Pi / period
	if len(e.Mu) != 2 {
		t.Fatalf("%d roots, want 2", len(e.Mu))
	}
	for i := range e.Mu {
		if d := math.Abs(math.Abs(e.Omega[i]) - omega); d > 1e-8 {
			t.Errorf("|ω|[%d] = %.12g, want %.12g", i, math.Abs(e.Omega[i]), omega)
		}
		if d := math.Abs(e.Rho[i] - 1); d > 1e-8 {
			t.Errorf("ρ[%d] = %.12g", i, e.Rho[i])
		}
	}
}
