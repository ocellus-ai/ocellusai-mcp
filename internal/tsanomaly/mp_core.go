package tsanomaly

import (
	"math"
	"runtime"
	"sort"
	"sync"
)

// This file implements the matrix profile self-join without external
// dependencies. It follows STOMP (Zhu et al. 2016): the sliding dot product
// between subsequence i and all subsequences j is updated in O(n) from row
// i-1, so the whole profile is O(n^2) time and O(n) memory per worker. Rows
// are split into chunks processed in parallel; each chunk computes its first
// row directly, and every refreshEvery rows the dot products are recomputed
// exactly to stop floating-point drift from accumulating.
//
// Numerics: every series is first shifted/scaled by its global median/MAD, so
// dot products of "1e9-scale" counters do not cancel against w*mu_i*mu_j, and
// the sliding variance is computed on centred data.
//
// Constant subsequences (extremely common in infrastructure metrics: flat
// zero, plateau) have no shape; z-normalising them would compare noise. They
// are flagged and handled like stumpy does: two constant subsequences are at
// distance 0, a constant vs a non-constant one at sqrt(w) (a "middle"
// distance, so a plateau inside a busy series is neither hidden nor a
// discord by itself).
//
// Extensions over the vanilla profile:
//   - kNN aggregation: profile[i] = mean of the KNN smallest distances instead
//     of the single nearest neighbour, which suppresses "twin" anomalies
//     (an anomaly that happens twice would otherwise match itself).
//   - multidimensional (mSTOMP, Yeh et al. 2017): for each (i,j) the per-dimension
//     distances are combined: mean over all, mean of the K best, or the worst.
//   - masks: subsequences whose imputed fraction exceeds a limit are neither
//     scored nor used as neighbours.

const refreshEvery = 512

// slidingMeanStd returns the mean and std of every length-w window, and a
// flag for windows whose std is below sigFloor (constant subsequences).
func slidingMeanStd(x []float64, w int, sigFloor float64) (mu, sig []float64, flat []bool) {
	n := len(x)
	m := n - w + 1
	mu = make([]float64, m)
	sig = make([]float64, m)
	flat = make([]bool, m)
	fw := float64(w)
	s := 0.0
	for i := 0; i < w; i++ {
		s += x[i]
	}
	for i := 0; i < m; i++ {
		if i > 0 {
			s += x[i+w-1] - x[i-1]
		}
		mu[i] = s / fw
	}
	// centred variance per window: O(n*w), negligible next to the O(n^2) join
	for i := 0; i < m; i++ {
		v := 0.0
		for k := 0; k < w; k++ {
			d := x[i+k] - mu[i]
			v += d * d
		}
		sd := math.Sqrt(v / fw)
		if sd < sigFloor {
			flat[i] = true
			sd = sigFloor
		}
		sig[i] = sd
	}
	return
}

// dotRow computes dot products of subsequence starting at i with every
// subsequence directly, O(n*w).
func dotRow(x []float64, w, i int, out []float64) {
	m := len(x) - w + 1
	xi := x[i : i+w]
	for j := 0; j < m; j++ {
		xj := x[j : j+w]
		s := 0.0
		for k := 0; k < w; k++ {
			s += xi[k] * xj[k]
		}
		out[j] = s
	}
}

// updateDot advances dot products from row i-1 to row i in place.
func updateDot(x []float64, w, i int, dot, first []float64) {
	m := len(x) - w + 1
	for j := m - 1; j > 0; j-- {
		dot[j] = dot[j-1] - x[i-1]*x[j-1] + x[i+w-1]*x[j+w-1]
	}
	dot[0] = first[i] // dot(subseq i, subseq 0) by symmetry of row 0
}

// zDist converts a dot product to the z-normalised Euclidean distance.
func zDist(dot, w, mui, sigi, muj, sigj float64, flati, flatj bool) float64 {
	if flati && flatj {
		return 0
	}
	if flati || flatj {
		return math.Sqrt(w)
	}
	c := (dot - w*mui*muj) / (w * sigi * sigj)
	if c > 1 {
		c = 1
	}
	if c < -1 {
		c = -1
	}
	return math.Sqrt(2 * w * (1 - c))
}

type mpInput struct {
	dims    [][]float64 // globally robust-scaled series
	w       int
	kDims   int    // >0: mean of k best dims; 0: all dims; -1: worst dim
	knn     int    // neighbours averaged per row
	njobs   int    // 0 = NumCPU
	exclude []bool // subsequences excluded (mostly imputed); nil = none
}

// stompProfile computes the (multi)dimensional self-join matrix profile.
// Excluded subsequences get NaN. For multidimensional input it also returns
// dimDist[i][k]: the per-dimension distance between subsequence i and its
// nearest neighbour, which tells which metrics made the match bad.
func stompProfile(in mpInput) (profile []float64, dimDist [][]float64) {
	dims := in.dims
	d := len(dims)
	n := len(dims[0])
	w := in.w
	m := n - w + 1
	kDims := in.kDims
	maxMode := kDims < 0
	if kDims <= 0 || kDims > d {
		kDims = d
	}
	knn := maxInt(in.knn, 1)
	njobs := in.njobs
	if njobs <= 0 {
		njobs = runtime.NumCPU()
	}
	if njobs > m {
		njobs = 1
	}
	excl := maxInt(w/2, 1)

	mu := make([][]float64, d)
	sig := make([][]float64, d)
	flat := make([][]bool, d)
	first := make([][]float64, d)
	for k := 0; k < d; k++ {
		// series are already scaled to unit MAD: a window whose std is below
		// 1e-3 of that carries no shape.
		mu[k], sig[k], flat[k] = slidingMeanStd(dims[k], w, 1e-3)
		first[k] = make([]float64, m)
		dotRow(dims[k], w, 0, first[k])
	}

	profile = make([]float64, m)
	if d > 1 {
		dimDist = make([][]float64, m)
	}
	fw := float64(w)

	chunk := (m + njobs - 1) / njobs
	var wg sync.WaitGroup
	for c := 0; c < njobs; c++ {
		lo := c * chunk
		hi := minInt(lo+chunk, m)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			dot := make([][]float64, d)
			for k := 0; k < d; k++ {
				dot[k] = make([]float64, m)
			}
			comb := make([]float64, m)
			per := make([]float64, d)
			best := make([]float64, knn)
			for i := lo; i < hi; i++ {
				if i == lo || (i-lo)%refreshEvery == 0 {
					for k := 0; k < d; k++ {
						if i == 0 {
							copy(dot[k], first[k])
						} else {
							dotRow(dims[k], w, i, dot[k])
						}
					}
				} else {
					for k := 0; k < d; k++ {
						updateDot(dims[k], w, i, dot[k], first[k])
					}
				}
				if in.exclude != nil && in.exclude[i] {
					profile[i] = math.NaN()
					continue
				}
				for j := 0; j < m; j++ {
					if (j > i-excl && j < i+excl) || (in.exclude != nil && in.exclude[j]) {
						comb[j] = math.Inf(1)
						continue
					}
					if d == 1 {
						comb[j] = zDist(dot[0][j], fw, mu[0][i], sig[0][i], mu[0][j], sig[0][j], flat[0][i], flat[0][j])
						continue
					}
					for k := 0; k < d; k++ {
						per[k] = zDist(dot[k][j], fw, mu[k][i], sig[k][i], mu[k][j], sig[k][j], flat[k][i], flat[k][j])
					}
					if maxMode {
						mx := per[0]
						for k := 1; k < d; k++ {
							if per[k] > mx {
								mx = per[k]
							}
						}
						comb[j] = mx
						continue
					}
					if kDims < d {
						sort.Float64s(per)
					}
					s := 0.0
					for k := 0; k < kDims; k++ {
						s += per[k]
					}
					comb[j] = s / float64(kDims)
				}
				for k := range best {
					best[k] = math.Inf(1)
				}
				nn := -1
				for j := 0; j < m; j++ {
					v := comb[j]
					if v >= best[knn-1] {
						continue
					}
					p := knn - 1
					for p > 0 && best[p-1] > v {
						best[p] = best[p-1]
						p--
					}
					best[p] = v
					if p == 0 {
						nn = j
					}
				}
				if dimDist != nil && nn >= 0 {
					dd := make([]float64, d)
					for k := 0; k < d; k++ {
						dd[k] = zDist(dot[k][nn], fw, mu[k][i], sig[k][i], mu[k][nn], sig[k][nn], flat[k][i], flat[k][nn])
					}
					dimDist[i] = dd
				}
				s, cnt := 0.0, 0
				for _, v := range best {
					if isFinite(v) {
						s += v
						cnt++
					}
				}
				if cnt == 0 {
					profile[i] = math.NaN()
				} else {
					profile[i] = s / float64(cnt)
				}
			}
		}(lo, hi)
	}
	wg.Wait()
	return profile, dimDist
}

// nanToZero replaces NaN/Inf with 0 (excluded windows are "not anomalous").
func nanToZero(x []float64) []float64 {
	for i, v := range x {
		if !isFinite(v) {
			x[i] = 0
		}
	}
	return x
}
