//go:build speed

package ssa

// Wall time of the two paths in Decompose against a full thin SVD from gonum.
// Not part of the default suite: go test -tags speed -run TestSVDSpeed -v
//
// full is the script's eigendecomposition of XXᵀ. top-k is the randomized
// method with the script's oversampling. gonum is mat.SVD of the explicit
// trajectory matrix, timed only where the dense matrix still fits comfortably.

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"gonum.org/v1/gonum/mat"
)

func TestSVDSpeed(t *testing.T) {
	fmt.Printf("GOMAXPROCS=%d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("%-28s %8s %8s %8s %10s %10s %10s %8s\n",
		"case", "n", "L", "K", "full", "top-k", "gonum", "full/top")
	cases := []speedCase{
		{name: "L=288, script uses full", n: 4032, L: 288, reps: 5, gonum: true},
		{name: "L=288, long series", n: 20160, L: 288, reps: 3, gonum: true},
		{name: "L=720", n: 20160, L: 720, reps: 3, gonum: true},
		{name: "L=1440, script uses top-k", n: 20160, L: 1440, reps: 2, gonum: true},
		{name: "L=1440, n=100k", n: 100000, L: 1440, reps: 1, gonum: false},
		{name: "L=2880", n: 40320, L: 2880, reps: 1, gonum: false},
	}
	for _, cs := range cases {
		full, top, gon := timeCase(t, cs)
		ratio := "·"
		if top > 0 {
			ratio = fmt.Sprintf("%.0fx", float64(full)/float64(top))
		}
		fmt.Printf("%-28s %8d %8d %8d %10s %10s %10s %8s\n",
			cs.name, cs.n, cs.L, cs.n-cs.L+1,
			dur(full), dur(top), dur(gon), ratio)
	}
}

type speedCase struct {
	name       string
	n, L, reps int
	gonum      bool
}

// timeCase returns the median Decompose time for the full path, for top-30
// with 8 iterations, and (optionally) one gonum thin SVD including the build
// of X. k and the iteration count are the script defaults above L = 300.
func timeCase(t *testing.T, cs speedCase) (full, top, gon time.Duration) {
	t.Helper()
	if cs.L > cs.n-cs.L+1 {
		t.Fatalf("%s: L > K", cs.name)
	}
	x := tones(cs.n, 288, 0.4, 1)
	const k, iters = 30, 8
	if oversample(k) >= cs.L {
		t.Fatalf("%s: top-k would fall back to full", cs.name)
	}
	full = medianRun(cs.reps, func() {
		if _, err := Decompose(x, cs.L, 0, 0); err != nil {
			t.Fatal(err)
		}
	})
	top = medianRun(cs.reps, func() {
		s, err := Decompose(x, cs.L, k, iters)
		if err != nil {
			t.Fatal(err)
		}
		if s.Err == nil {
			t.Fatalf("%s: expected the top-k path", cs.name)
		}
	})
	if cs.gonum {
		gon = once(func() {
			X := trajectory(x, cs.L)
			var svd mat.SVD
			if !svd.Factorize(X, mat.SVDThin) {
				t.Fatal("gonum SVD failed")
			}
		})
	}
	return full, top, gon
}

func medianRun(reps int, fn func()) time.Duration {
	ds := make([]time.Duration, reps)
	for i := range ds {
		ds[i] = once(fn)
	}
	// insertion sort, reps is tiny
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds[len(ds)/2]
}

func once(fn func()) time.Duration {
	t0 := time.Now()
	fn()
	return time.Since(t0)
}

func dur(d time.Duration) string {
	if d == 0 {
		return "·"
	}
	if d < 10*time.Millisecond {
		return d.Round(100 * time.Microsecond).String()
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}
