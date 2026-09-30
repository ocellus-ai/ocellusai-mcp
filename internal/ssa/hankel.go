package ssa

import (
	"math/cmplx"
	"runtime"
	"sync"
	"sync/atomic"

	"gonum.org/v1/gonum/dsp/fourier"
)

// hankel multiplies by the trajectory matrix X (L×K, X[a][b] = x[a+b]) and by
// Xᵀ through FFT: both products are correlations with x. The FFT length N ≥ n
// keeps the circular correlation from wrapping around.
type hankel struct {
	n, L, K, N int
	xhat       []complex128 // FFT of x zero-padded to N
	w          []float64    // number of elements on each anti-diagonal
	// pool holds one FFT state per worker; pool[0] also serves the
	// sequential code. It starts with one state and grows on the first
	// parallel product, so a full decomposition (and the many small ones of
	// the noise analysis) never allocates GOMAXPROCS FFT buffers.
	pool []*fftWork
}

// fftWork is the FFT state and buffers of one goroutine: fourier.FFT is not
// safe for concurrent use.
type fftWork struct {
	f    *fourier.FFT
	seq  []float64
	coef []complex128
}

func newFFTWork(size int) *fftWork {
	return &fftWork{f: fourier.NewFFT(size), seq: make([]float64, size), coef: make([]complex128, size/2+1)}
}

func newHankel(x []float64, window int) *hankel {
	L := window
	n := len(x)
	h := &hankel{n: n, L: L, K: n - L + 1, N: fftLen(n)}
	h.pool = []*fftWork{newFFTWork(h.N)}
	h.xhat = h.fft(h.pool[0], x, nil)
	h.w = make([]float64, n)
	for j := range h.w {
		h.w[j] = float64(min(j+1, h.L, h.K, n-j))
	}
	return h
}

// grow makes the pool hold one FFT state per worker of a parallel loop over
// m items: min(GOMAXPROCS, m). Called from sequential code only.
func (h *hankel) grow(m int) {
	for len(h.pool) < min(runtime.GOMAXPROCS(0), m) {
		h.pool = append(h.pool, newFFTWork(h.N))
	}
}

// fftLen returns the smallest N ≥ n with prime factors 2, 3 and 5 only, the
// lengths fourier.FFT handles fast.
func fftLen(n int) int {
	for m := n; ; m++ {
		r := m
		for _, p := range []int{2, 3, 5} {
			for r%p == 0 {
				r /= p
			}
		}
		if r == 1 {
			return m
		}
	}
}

// fft returns in dst the FFT of v zero-padded to N; dst == nil allocates.
func (h *hankel) fft(w *fftWork, v []float64, dst []complex128) []complex128 {
	copy(w.seq, v)
	clear(w.seq[len(v):])
	return w.f.Coefficients(dst, w.seq)
}

// corr sets dst[j] = Σₐ x[a+j]·v[a]: Xᵀ·v for len(v) = L and len(dst) = K,
// X·v for len(v) = K and len(dst) = L. dst and v may be the same slice only
// if they have the same length.
func (h *hankel) corr(w *fftWork, dst, v []float64) {
	c := h.fft(w, v, w.coef)
	for f, xf := range h.xhat {
		c[f] = xf * cmplx.Conj(c[f])
	}
	w.f.Sequence(w.seq, c)
	inv := 1 / float64(h.N)
	for j := range dst {
		dst[j] = w.seq[j] * inv
	}
}

// forEach calls fn(w, j) for j < m on parallel workers, each with its own
// FFT state.
func (h *hankel) forEach(m int, fn func(w *fftWork, j int)) {
	h.grow(m)
	var next atomic.Int64
	var wg sync.WaitGroup
	for _, w := range h.pool[:min(len(h.pool), m)] {
		wg.Go(func() {
			for j := int(next.Add(1)) - 1; j < m; j = int(next.Add(1)) - 1 {
				fn(w, j)
			}
		})
	}
	wg.Wait()
}

// reconstructPairs is the diagonal average of Σᵢ uᵢ·gᵢᵀ with gᵢ = Xᵀuᵢ, one
// FFT pair per component and one inverse FFT: the anti-diagonal sums of
// uᵢ·gᵢᵀ are the convolution uᵢ ∗ gᵢ.
func reconstructPairs(h *hankel, u, g [][]float64) []float64 {
	w := h.pool[0]
	acc := make([]complex128, h.N/2+1)
	uh := make([]complex128, len(acc))
	for i := range u {
		h.fft(w, u[i], uh)
		gh := h.fft(w, g[i], w.coef)
		for f := range acc {
			acc[f] += uh[f] * gh[f]
		}
	}
	w.f.Sequence(w.seq, acc)
	y := make([]float64, h.n)
	for j := range y {
		y[j] = w.seq[j] / (float64(h.N) * h.w[j])
	}
	return y
}

// powerSpectrum returns |FFT(y)|² of y zero-padded to N, bins 0 … N/2.
func (h *hankel) powerSpectrum(y []float64) []float64 {
	w := h.pool[0]
	c := h.fft(w, y, w.coef)
	out := make([]float64, len(c))
	for k, v := range c {
		out[k] = real(v)*real(v) + imag(v)*imag(v)
	}
	return out
}
