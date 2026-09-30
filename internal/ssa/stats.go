package ssa

import (
	"math"
	"math/rand/v2"
	"slices"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/stat"
)

// seq returns the group 0, 1, …, r−1.
func seq(r int) []int {
	g := make([]int, r)
	for i := range g {
		g[i] = i
	}
	return g
}

func median(x []float64) float64 {
	c := slices.Clone(x)
	slices.Sort(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return (c[m-1] + c[m]) / 2
}

// robustSD returns the standard deviation estimated from the median absolute
// deviation, insensitive to spikes; the plain one if the MAD is zero.
func robustSD(x []float64) float64 {
	m := median(x)
	d := make([]float64, len(x))
	for i, v := range x {
		d[i] = math.Abs(v - m)
	}
	if sd := 1.4826 * median(d); sd > 0 {
		return sd
	}
	return stat.PopStdDev(x, nil)
}

// lag1 returns the lag-1 autocorrelation; 0 for a constant series.
func lag1(x []float64) float64 {
	m := stat.Mean(x, nil)
	var num, den float64
	for i, v := range x {
		den += (v - m) * (v - m)
		if i > 0 {
			num += (x[i-1] - m) * (v - m)
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// winsorize returns x with the values farther than band from the median
// pulled back to that distance.
func winsorize(x []float64, band float64) []float64 {
	m := median(x)
	y := make([]float64, len(x))
	for i, v := range x {
		y[i] = min(max(v, m-band), m+band)
	}
	return y
}

// saturation finds a ceiling (floor): the maximum (minimum) of x repeated on
// at least 1% of the points and more often than the values within band of it.
// It returns the number of points at each, 0 where there is none. A constant
// series has neither: it sits at its only value, not at a bound.
func saturation(x []float64, band float64) (atHi, atLo int) {
	hi, lo := floats.Max(x), floats.Min(x)
	if hi == lo {
		return 0, 0
	}
	var onHi, nearHi, onLo, nearLo int
	for _, v := range x {
		switch {
		case v == hi:
			onHi++
		case v >= hi-band:
			nearHi++
		}
		switch {
		case v == lo:
			onLo++
		case v <= lo+band:
			nearLo++
		}
	}
	need := max(5, len(x)/100)
	if onHi >= need && onHi > nearHi {
		atHi = onHi
	}
	if onLo >= need && onLo > nearLo {
		atLo = onLo
	}
	return atHi, atLo
}

// ar1 returns a stationary AR(1) series with unit variance.
func ar1(n int, phi float64, seed uint64) []float64 { return ar1Stream(n, phi, seed, 7) }

// ar1Stream is ar1 drawn from generator stream: channel c of a simulated
// multichannel noise uses stream 7+c, so channel 0 is the univariate series.
func ar1Stream(n int, phi float64, seed, stream uint64) []float64 {
	rng := rand.New(rand.NewPCG(seed, stream))
	e := make([]float64, n)
	e[0] = rng.NormFloat64()
	c := math.Sqrt(1 - phi*phi)
	for t := 1; t < n; t++ {
		e[t] = phi*e[t-1] + c*rng.NormFloat64()
	}
	return e
}
