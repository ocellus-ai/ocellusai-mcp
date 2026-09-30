package ssa

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/stat"
)

// Thresholds of the analysis. Every finding states the value, the threshold
// it is compared with and what the comparison means.
const (
	structRatio = 1.5  // σᵢ ≥ 1.5·floor: structure
	borderRatio = 1.2  // 1.2·floor ≤ σᵢ < 1.5·floor: borderline, may be noise
	pairWCorr   = 0.5  // |w-corr| of neighbours ≥ 0.5: two halves of one oscillation
	pairGap     = 0.1  // σ of a pair differing by more than 10%: an impure pair
	slowPeriods = 1.5  // period > 1.5·L: a slow cycle the window sees as trend
	sepGood     = 0.1  // |w-corr| < 0.1 between groups: separated
	sepBad      = 0.3  // |w-corr| ≥ 0.3 between groups: mixed
	spikeSigmas = 4.0  // |residual| > 4σ: a spike
	minCycles   = 3.0  // fewer cycles of the main period in the history: rough estimates
	errMax      = 1e-3 // top-k: err above it means not converged
	surrogates  = 3    // AR(1) series simulated for the noise floor
	maxItems    = 50   // the analysis describes at most this many components
	numFloor    = 1e-7 // the floor is at least 1e-7·σ₀: below it σ are rounding errors
	heavyTails  = 1.2  // plain σ of the residual > 1.2 × its robust σ: heavy-tailed noise (spikes, bursts)
	maxSeed     = 20   // the first estimate removes at most this many leading components
	fullMaxL    = 300  // the noise floor simulation decomposes all components up to this window
)

// Explosive is the growth over a whole forecast from which it is unstable: a
// root ρ with ρ^steps above it. Forecastable leaves such harmonic pairs out
// when they are weak: below WeakShare of the variance of every channel.
const (
	Explosive = 2.0
	WeakShare = 0.05
)

// StructRatio is the ratio σᵢ/floor from which a component is structure,
// and BorderRatio the one from which it is borderline (below: noise).
const (
	StructRatio = structRatio
	BorderRatio = borderRatio
)

// Kinds of components.
const (
	KindTrend       = "trend"
	KindSlow        = "slow cycle"
	KindHarmonic    = "harmonic"
	KindAlternating = "alternating"
	KindUnpaired    = "unpaired oscillation"
)

// Levels of findings.
const (
	LevelOK   = "ok"
	LevelInfo = "info"
	LevelWarn = "warn"
)

// Finding is one statement of the analysis: the value, the threshold it is
// compared with and what that means.
type Finding struct {
	Level string // ok, info or warn
	Text  string
}

// Noise is the noise model: AR(1) noise fitted per channel to the residual
// after the leading structure, and the floor it gives.
type Noise struct {
	// Floor is the largest σ such noise alone produces at this n and L
	// (Monte Carlo SSA), in the units of the singular values (the weighted
	// trajectory matrix for MSSA).
	Floor float64
	// Removed is the number of leading components removed before the
	// residual was measured.
	Removed int
	// Negligible reports a series without noise: the floor is the numerical
	// precision of the decomposition, numFloor·σ₀.
	Negligible bool
	Channels   []ChannelNoise
}

// ChannelNoise is the noise of one channel, in the units of the series.
type ChannelNoise struct {
	SD float64 // robust σ of the residual away from saturation
	// RMS is the plain σ of the same residual: close to SD for Gaussian
	// noise, well above it when rare spikes or bursts carry most of the
	// noise energy. The floor is simulated for the larger of the two.
	RMS float64
	Phi float64 // lag-1 autocorrelation of the residual
	Max float64 // maximum of the series
	Min float64 // minimum of the series
	// AtMax (AtMin) is the number of points exactly at the maximum (minimum)
	// when the series saturates there, 0 otherwise.
	AtMax, AtMin int
}

// Component is one component, or a pair of components that make one
// oscillation, down to the borderline.
type Component struct {
	Comps  []int
	Kind   string
	Sigma  float64 // σ of the first component
	Ratio  float64 // σ of the first component / noise floor
	Period float64 // oscillations: period in points
	Gap    float64 // pair: relative difference of its two σ
	Order  int     // harmonic: number within its family, 1 for the longest period
	Base   float64 // harmonic: the longest period of its family
	// Growth is the largest modulus of the ESPRIT roots of the component's
	// own subspace: its growth per step in a forecast (1 for a stationary
	// oscillation or a level, NaN if the estimate failed).
	Growth float64
	// Share is, per channel, the variance of the component's reconstruction
	// divided by the variance of the channel (NaN for a constant channel):
	// which channels carry it and how much of their movement it is.
	Share []float64
}

// Borderline reports a component between BorderRatio and StructRatio times
// the floor: weak structure or noise.
func (c Component) Borderline() bool { return c.Ratio < structRatio }

// Suggestion is a suggested group of components with a description.
type Suggestion struct {
	Comps []int
	Label string
}

// Analysis is the interpretation of an SSA or MSSA decomposition.
type Analysis struct {
	Noise Noise
	// Components describes the leading components down to BorderRatio·floor,
	// one entry per single component or harmonic pair.
	Components []Component
	// NoiseFrom is the first component below the borderline; Len() when
	// every computed component is above it (top-k).
	NoiseFrom int
	// Structure lists the components at or above StructRatio·floor.
	Structure []int
	// Coarse suggests the trend, one group per harmonic family (a whole
	// cycle) and the rest one by one; Fine the same with every harmonic
	// separately.
	Coarse, Fine []Suggestion
	// Runs are the stretches of the channels away from their structure,
	// chronological per channel; Recurring groups those that recur daily.
	Runs      []Run
	Recurring []Recurrence
	Findings  []Finding

	d     decomp
	names []string  // channel names in findings; nil for a single series
	start time.Time // time of the first point; zero when unknown
	step  time.Duration
	// cycle is the cycle of the usual profile the stretches are measured
	// from, in points (0: none), profile its name for findings.
	cycle    int
	profile  string
	minDelta []float64 // per channel: the smallest stretch worth reporting
	byComp   map[int]*Component
	elem     map[int][][]float64 // elementary reconstructions, per channel
}

// decomp is what the analysis needs from a decomposition: SSA is the
// one-channel case of MSSA with weight 1.
type decomp interface {
	Len() int
	sigmas() []float64
	errs() []float64
	window() (int, int) // L and K
	powerIters() int
	channels() int
	data(c int) []float64
	weight(c int) float64
	hank(c int) *hankel
	right(c, i int) []float64 // Hₛᵀuᵢ of channel c
	recon(group []int) [][]float64
	Esprit(group []int) (Esprit, error)
	// surrogate decomposes simulated noise of the same shape (window,
	// weights) and returns its largest singular value.
	surrogate(noise [][]float64, k, iters int) (float64, error)
}

func (s *SSA) sigmas() []float64          { return s.Sigma }
func (s *SSA) errs() []float64            { return s.Err }
func (s *SSA) window() (int, int)         { return s.L, s.K }
func (s *SSA) powerIters() int            { return s.iters }
func (s *SSA) channels() int              { return 1 }
func (s *SSA) data(int) []float64         { return s.x }
func (s *SSA) weight(int) float64         { return 1 }
func (s *SSA) hank(int) *hankel           { return s.h }
func (s *SSA) right(_, i int) []float64   { return s.gvec(i) }
func (s *SSA) recon(g []int) [][]float64  { return [][]float64{s.Reconstruct(g)} }
func (m *MSSA) sigmas() []float64         { return m.Sigma }
func (m *MSSA) errs() []float64           { return m.Err }
func (m *MSSA) window() (int, int)        { return m.L, m.K }
func (m *MSSA) powerIters() int           { return m.iters }
func (m *MSSA) channels() int             { return len(m.x) }
func (m *MSSA) data(c int) []float64      { return m.x[c] }
func (m *MSSA) weight(c int) float64      { return m.Weights[c] }
func (m *MSSA) hank(c int) *hankel        { return m.h[c] }
func (m *MSSA) right(c, i int) []float64  { return m.gvec(c, i) }
func (m *MSSA) recon(g []int) [][]float64 { y, _ := m.Reconstruct(g); return y }

func (s *SSA) surrogate(noise [][]float64, k, iters int) (float64, error) {
	z, err := Decompose(noise[0], s.L, k, iters)
	if err != nil {
		return 0, err
	}
	return z.Sigma[0], nil
}

func (m *MSSA) surrogate(noise [][]float64, k, iters int) (float64, error) {
	z, err := DecomposeMulti(noise, m.L, k, iters, m.Weights)
	if err != nil {
		return 0, err
	}
	return z.Sigma[0], nil
}

// Analyze interprets the decomposition: the noise model and the floor it
// gives, what each leading component is, the stretches of the series away
// from its structure, suggested groups and findings. step, if not zero, is
// the time between points and turns periods into time units in labels and
// findings.
func (s *SSA) Analyze(step time.Duration) *Analysis { return s.AnalyzeWith(Options{Step: step}) }

// Options are the settings of the analysis beyond the decomposition.
type Options struct {
	// Step is the time between points: periods and lengths in the findings
	// get time units. Zero: points only.
	Step time.Duration
	// Start is the time of the first point: the findings name the stretches
	// away from the usual profile by their UTC times and daily recurrences
	// by the time of day. Zero: by point numbers.
	Start time.Time
	// MinDelta is, per channel, the smallest distance from the usual profile
	// worth reporting, in the units of the series: a stretch must exceed both
	// it and 4 robust sigma. nil or 0: 4 sigma alone.
	MinDelta []float64
}

// AnalyzeWith is Analyze with the options.
func (s *SSA) AnalyzeWith(o Options) *Analysis { return analyze(s, nil, o) }

// Analyze interprets the MSSA like SSA.Analyze does a single series: the
// noise is fitted per channel and simulated with the channel weights, so
// the floor is in the units of the weighted decomposition; w-correlations
// and periods sum over the channels with their weights. names name the
// channels in the findings (nil: ch0, ch1, …).
func (m *MSSA) Analyze(names []string, step time.Duration) *Analysis {
	return m.AnalyzeWith(names, Options{Step: step})
}

// AnalyzeWith is Analyze with the options (MinDelta per channel).
func (m *MSSA) AnalyzeWith(names []string, o Options) *Analysis {
	if len(names) != m.Channels() {
		names = make([]string, m.Channels())
		for c := range names {
			names[c] = "ch" + strconv.Itoa(c)
		}
	}
	return analyze(m, names, o)
}

func analyze(d decomp, names []string, o Options) *Analysis {
	a := &Analysis{d: d, names: names, start: o.Start, step: o.Step, minDelta: o.MinDelta, byComp: map[int]*Component{}, elem: map[int][][]float64{}}
	sig := d.sigmas()
	if sig[0] == 0 {
		a.Noise.Channels = make([]ChannelNoise, d.channels())
		a.note(LevelInfo, "%s identically zero: there is nothing to decompose.", a.subject("The series is", "Every channel is"))
		return a
	}
	a.Noise = a.noise()
	a.classify()
	for i := range a.Components {
		for _, c := range a.Components[i].Comps {
			a.byComp[c] = &a.Components[i]
		}
		if !a.Components[i].Borderline() {
			a.Structure = append(a.Structure, a.Components[i].Comps...)
		}
	}
	a.Coarse, a.Fine = suggest(a.Components, o.Step)
	a.findNoise()
	a.findRuns()
	a.findStructure()
	a.findBounds()
	a.findTopK()
	return a
}

// multi reports whether findings name channels.
func (a *Analysis) multi() bool { return a.names != nil }

// subject picks the wording for one series or for several channels.
func (a *Analysis) subject(one, many string) string {
	if a.multi() {
		return many
	}
	return one
}

func (a *Analysis) note(level, format string, args ...any) {
	a.Findings = append(a.Findings, Finding{Level: level, Text: fmt.Sprintf(format, args...)})
}

// F returns the elementary reconstruction of component i on every channel.
func (a *Analysis) F(i int) [][]float64 {
	if a.elem[i] == nil {
		a.elem[i] = a.d.recon([]int{i})
	}
	return a.elem[i]
}

// sumF returns the reconstruction of comps as a sum of elementary ones.
func (a *Analysis) sumF(comps []int) [][]float64 {
	out := make([][]float64, a.d.channels())
	for c := range out {
		out[c] = make([]float64, len(a.d.data(c)))
		for _, i := range comps {
			floats.Add(out[c], a.F(i)[c])
		}
	}
	return out
}

// residual returns every channel minus the reconstruction of group.
func (a *Analysis) residual(group []int) [][]float64 {
	R := a.d.recon(group)
	for c := range R {
		r := make([]float64, len(R[c]))
		floats.SubTo(r, a.d.data(c), R[c])
		R[c] = r
	}
	return R
}

// wcorr is the w-correlation of two multichannel series: anti-diagonal
// weights over time, channel weights over channels.
func (a *Analysis) wcorr(x, y [][]float64) float64 {
	hw := a.d.hank(0).w
	var ab, aa, bb float64
	for c := range x {
		wc := a.d.weight(c)
		for t, w := range hw {
			ab += w * wc * x[c][t] * y[c][t]
			aa += w * wc * x[c][t] * x[c][t]
			bb += w * wc * y[c][t] * y[c][t]
		}
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// ---- noise ----

// noise fits the noise model. The noise is measured in the residual without
// the leading components, as many as the larger of two estimates: the largest
// gap in the leading spectrum (misses a weak cycle under a huge level), and
// the components above a first floor from an AR(1) fit to differences of the
// series (fooled by short-period cycles, which differences do not remove).
// Removing a noise component too many barely changes the estimate, leaving
// structure in would inflate it. The loop repeats while more components turn
// out to be above structRatio·floor.
//
// Two changes to the ssa command, both for heavy-tailed noise (an idle VM
// with rare bursts: the robust σ sees only the quiet level, while the
// bursts spread their energy over the whole spectrum). The first estimate
// removes at most maxSeed components (and at most half of a full
// decomposition): without the cap a floor from the quiet level let it remove
// every component, the residual vanished and the whole spectrum became
// "structure". And when the plain σ of the residual exceeds the robust one
// by more than heavyTails, the floor is simulated with the plain σ and the
// lag-1 autocorrelation of the raw residual. On a production fleet (2026-09-27,
// 114 VMs × 4 days of cpu, memory and network) the command's model left
// 94, 41 and 83 series with every component "structure"; with the changes
// none, the daily cycle (detrended lag-one-day autocorrelation > 0.3) is
// found in 45/55, 24/29 and 48/63 series, and the structure forecast of the
// held-out day beats "the same as yesterday" on 63, 65 and 27 VMs instead
// of 23, 59 and 21. Gaussian noise is below heavyTails and keeps the
// command's numbers.
func (a *Analysis) noise() Noise {
	d := a.d
	S := d.channels()
	sig := d.sigmas()
	nm := Noise{Channels: make([]ChannelNoise, S)}
	for c := range S {
		x := d.data(c)
		ch := &nm.Channels[c]
		ch.Max, ch.Min = floats.Max(x), floats.Min(x)
		diff := make([]float64, len(x)-1)
		for i := range diff {
			diff[i] = x[i+1] - x[i]
		}
		ch.AtMax, ch.AtMin = saturation(x, robustSD(diff)/math.Sqrt2)
	}
	r, best := 1, 0.0
	for i := 0; i+1 < min(len(sig), 20); i++ {
		gap := math.Inf(1)
		if sig[i+1] > 0 {
			gap = sig[i] / sig[i+1]
		}
		if gap > best {
			best, r = gap, i+1
		}
	}
	sd, phi := make([]float64, S), make([]float64, S)
	for c := range S {
		sd[c], phi[c] = nm.Channels[c].diffNoise(d.data(c))
	}
	seedMax := min(maxSeed, len(sig))
	if d.errs() == nil {
		seedMax = min(seedMax, max(1, len(sig)/2))
	}
	for floor := a.mcFloor(sd, phi); r < seedMax && sig[r] >= structRatio*floor; {
		r++
	}
	for range 5 {
		resid := a.residual(seq(r))
		for c := range S {
			ch := &nm.Channels[c]
			free := ch.free(d.data(c), resid[c])
			ch.SD, ch.RMS = robustSD(free), stat.PopStdDev(free, nil)
			ch.Phi = min(max(lag1(winsorize(resid[c], spikeSigmas*ch.SD)), -0.95), 0.95)
			if ch.HeavyTailed() {
				ch.Phi = min(max(lag1(resid[c]), -0.95), 0.95)
			}
			sd[c], phi[c] = ch.floorSD(), ch.Phi
		}
		nm.Removed = r
		mc := a.mcFloor(sd, phi)
		nm.Floor = max(mc, numFloor*sig[0])
		nm.Negligible = mc < numFloor*sig[0]
		above := 0
		for above < len(sig) && sig[above] >= structRatio*nm.Floor {
			above++
		}
		if above <= r {
			break
		}
		r = above
	}
	return nm
}

// mcFloor returns the largest σ₀ of AR(1) noise with the given per-channel
// σ and coefficient at the length, window and weights of this decomposition:
// the maximum over a few simulated series, so that noise components of the
// data rarely exceed it (Monte Carlo SSA, Allen and Smith 1996).
func (a *Analysis) mcFloor(sd, phi []float64) float64 {
	if floats.Max(sd) == 0 {
		return 0
	}
	L, _ := a.d.window()
	k := 5
	if L <= fullMaxL {
		k = 0
	}
	n := len(a.d.data(0))
	var top float64
	for seed := range uint64(surrogates) {
		noise := make([][]float64, len(sd))
		for c := range sd {
			noise[c] = ar1Stream(n, phi[c], seed, 7+uint64(c))
			floats.Scale(sd[c], noise[c])
		}
		if s0, err := a.d.surrogate(noise, k, 6); err == nil {
			top = max(top, s0)
		}
	}
	return top
}

// diffNoise estimates AR(1) noise from the first and second differences at
// the points not stuck at saturation. The level, a trend and slow cycles
// hardly affect differences, while for AR(1) noise with variance v
// var Δ = 2v(1−φ) and var Δ² = 2v(1−φ)(3−φ): φ = 3 − var Δ²/var Δ.
func (ch ChannelNoise) diffNoise(x []float64) (sd, phi float64) {
	var d1, d2 []float64
	for i := 2; i < len(x); i++ {
		if ch.stuck(x[i]) || ch.stuck(x[i-1]) || ch.stuck(x[i-2]) {
			continue
		}
		d1 = append(d1, x[i]-x[i-1])
		d2 = append(d2, x[i]-2*x[i-1]+x[i-2])
	}
	if len(d1) < 3 {
		return 0, 0
	}
	v1, v2 := math.Pow(robustSD(d1), 2), math.Pow(robustSD(d2), 2)
	if v1 == 0 {
		return 0, 0
	}
	phi = min(max(3-v2/v1, -0.95), 0.95)
	return math.Sqrt(v1 / (2 * (1 - phi))), phi
}

// HeavyTailed reports noise whose plain σ exceeds the robust one by more
// than heavyTails: rare spikes or bursts carry much of its energy.
func (ch ChannelNoise) HeavyTailed() bool { return ch.RMS > heavyTails*ch.SD }

// floorSD is the σ the noise floor is simulated with: the robust one, or
// the plain one for heavy-tailed noise.
func (ch ChannelNoise) floorSD() float64 {
	if ch.HeavyTailed() {
		return ch.RMS
	}
	return ch.SD
}

// stuck reports whether v is a saturation value of the channel.
func (ch ChannelNoise) stuck(v float64) bool {
	return ch.AtMax > 0 && v == ch.Max || ch.AtMin > 0 && v == ch.Min
}

// free returns the residual at the points not stuck at a saturation value:
// there the residual is nearly zero and would shrink the noise estimate.
func (ch ChannelNoise) free(x, resid []float64) []float64 {
	var out []float64
	for i, v := range resid {
		if !ch.stuck(x[i]) {
			out = append(out, v)
		}
	}
	if out == nil {
		return resid
	}
	return out
}

// ---- components ----

// classify describes the components down to borderRatio·floor. Neighbours
// whose elementary reconstructions are strongly w-correlated and whose
// vectors rotate (complex ESPRIT roots) are one oscillation, with the period
// of the rotation. A single component is trend if its reconstruction is
// dominated by periods longer than slowPeriods·L.
func (a *Analysis) classify() {
	sig := a.d.sigmas()
	L, _ := a.d.window()
	n, slow := float64(len(a.d.data(0))), slowPeriods*float64(L)
	last := min(len(sig), maxItems)
	i := 0
	for i < last && sig[i] >= borderRatio*a.Noise.Floor {
		it := Component{Comps: []int{i}, Sigma: sig[i], Ratio: sig[i] / a.Noise.Floor}
		if i+1 < len(sig) && math.Abs(a.wcorr(a.F(i), a.F(i+1))) >= pairWCorr {
			if T, ok := a.esprit2(i, i+1); ok {
				it.Comps, it.Period = []int{i, i + 1}, T
				it.Gap = (sig[i] - sig[i+1]) / sig[i]
				switch {
				case T > n:
					it.Kind = KindTrend // a "cycle" longer than the series is a trend
				case T > slow:
					it.Kind = KindSlow
				default:
					it.Kind = KindHarmonic
				}
			}
		}
		if len(it.Comps) == 1 {
			switch T := a.peakPeriod(a.F(i)); {
			case T >= slow:
				it.Kind = KindTrend
			case T < 2.2:
				it.Kind = KindAlternating
			default:
				it.Kind, it.Period = KindUnpaired, T
			}
		}
		it.Share = a.shares(a.sumF(it.Comps))
		it.Growth = math.NaN()
		if es, err := a.d.Esprit(it.Comps); err == nil {
			it.Growth = slices.Max(es.Rho)
		}
		a.Components = append(a.Components, it)
		i += len(it.Comps)
	}
	a.NoiseFrom = i
	families(a.Components)
}

// shares returns, per channel, the variance of y divided by the variance of
// the channel.
func (a *Analysis) shares(y [][]float64) []float64 {
	out := make([]float64, len(y))
	for c := range y {
		out[c] = math.NaN()
		if vx := stat.PopVariance(a.d.data(c), nil); vx > 0 {
			out[c] = stat.PopVariance(y[c], nil) / vx
		}
	}
	return out
}

// peakPeriod returns the period of the strongest frequency of y, the power
// spectra of the channels summed with their weights; +Inf when the mean
// (zero frequency) dominates.
func (a *Analysis) peakPeriod(y [][]float64) float64 {
	var spec []float64
	for c := range y {
		p := a.d.hank(c).powerSpectrum(y[c])
		if spec == nil {
			spec = make([]float64, len(p))
		}
		floats.AddScaled(spec, a.d.weight(c), p)
	}
	var peak float64
	kPeak := 0
	for k, e := range spec {
		if e > peak {
			peak, kPeak = e, k
		}
	}
	if kPeak == 0 {
		return math.Inf(1)
	}
	return float64(a.d.hank(0).N) / float64(kPeak)
}

// esprit2 estimates the period of the oscillation spanned by components i
// and j from the shift invariance of their right vectors (ESPRIT): the
// vectors shifted by one point are the unshifted ones times a 2×2 matrix M,
// whose eigenvalues are e^{±iω}. ok is false for real eigenvalues: no
// rotation, no oscillation. The right vectors Xᵀuᵢ, of length K ≥ L, hold
// more cycles than uᵢ and give a more precise period; for MSSA the shift
// stays inside each channel's block and the blocks add with the channel
// weights. Scaling a vector does not change the result.
func (a *Analysis) esprit2(i, j int) (period float64, ok bool) {
	var g00, g01, g11, h00, h01, h10, h11 float64
	for c := range a.d.channels() {
		u, v, w := a.d.right(c, i), a.d.right(c, j), a.d.weight(c)
		for t := 0; t+1 < len(u); t++ {
			g00 += w * u[t] * u[t]
			g01 += w * u[t] * v[t]
			g11 += w * v[t] * v[t]
			h00 += w * u[t] * u[t+1]
			h01 += w * u[t] * v[t+1]
			h10 += w * v[t] * u[t+1]
			h11 += w * v[t] * v[t+1]
		}
	}
	// M = G⁻¹·H, G and H the Gram matrices of [u v] with itself and with its shift.
	det := g00*g11 - g01*g01
	if det == 0 {
		return 0, false
	}
	m00 := (g11*h00 - g01*h10) / det
	m01 := (g11*h01 - g01*h11) / det
	m10 := (g00*h10 - g01*h00) / det
	m11 := (g00*h11 - g01*h01) / det
	tr, dt := m00+m11, m00*m11-m01*m10
	disc := tr*tr - 4*dt
	if disc >= 0 {
		return 0, false
	}
	return 2 * math.Pi / math.Atan2(math.Sqrt(-disc), tr), true
}

// families finds harmonics whose periods are whole fractions P/m of a longer
// one P and which are weaker than it: together they make one non-sinusoidal
// cycle, like a daily profile with a working-hours plateau. A stronger cycle
// at P/m (a daily cycle under a weekly one) is a cycle of its own. Sets order
// and base.
func families(items []Component) {
	var h []int // harmonics, longest period first
	for i, it := range items {
		if it.Kind == KindHarmonic {
			h = append(h, i)
		}
	}
	slices.SortStableFunc(h, func(a, b int) int { return cmp.Compare(items[b].Period, items[a].Period) })
	for _, i := range h {
		if items[i].Order > 0 {
			continue
		}
		P := items[i].Period
		items[i].Order, items[i].Base = 1, P
		for _, j := range h {
			if items[j].Order > 0 {
				continue
			}
			if m := math.Round(P / items[j].Period); m >= 2 && math.Abs(P/items[j].Period-m) <= 0.03*m && items[j].Ratio <= items[i].Ratio {
				items[j].Order, items[j].Base = int(m), P
			}
		}
	}
}

// suggest builds groups from the structure items. coarse: the trend, one group
// per harmonic family (the whole cycle), the rest one by one; fine: the same
// with every harmonic separately.
func suggest(items []Component, step time.Duration) (coarse, fine []Suggestion) {
	var trend []int
	var fams []float64
	fam := map[float64][]int{}
	orders := map[float64][]int{}
	var harm, other []Suggestion
	for _, it := range items {
		if it.Borderline() {
			continue
		}
		switch it.Kind {
		case KindTrend, KindSlow:
			trend = append(trend, it.Comps...)
		case KindHarmonic:
			if fam[it.Base] == nil {
				fams = append(fams, it.Base)
			}
			fam[it.Base] = append(fam[it.Base], it.Comps...)
			orders[it.Base] = append(orders[it.Base], it.Order)
			harm = append(harm, Suggestion{it.Comps, "harmonic, period " + FormatPeriod(it.Period, step)})
		default:
			other = append(other, Suggestion{it.Comps, it.Kind})
		}
	}
	if trend != nil {
		coarse = append(coarse, Suggestion{trend, "trend"})
		fine = append(fine, Suggestion{trend, "trend"})
	}
	for _, P := range fams {
		label := "cycle, period " + FormatPeriod(P, step)
		if os := orders[P]; len(os) > 1 {
			slices.Sort(os)
			names := make([]string, len(os))
			for i, o := range os {
				names[i] = strconv.Itoa(o)
			}
			label += ", harmonics " + strings.Join(names, ", ")
		}
		coarse = append(coarse, Suggestion{fam[P], label})
	}
	coarse = append(coarse, other...)
	fine = append(append(fine, harm...), other...)
	return coarse, fine
}

// Forecastable returns the structure components a forecast of steps points
// can continue — the trend, slow cycles and harmonic pairs — and the
// structure components it leaves out: left, an oscillation without a partner
// or a component alternating in sign, which have no stable continuation (on
// a production fleet they made recurrent forecasts grow to the clip bounds within
// hours), and growing, the weak harmonic pairs (below WeakShare of the
// variance of every channel) whose roots grow more than Explosive times over
// the steps (steps <= 0: none).
//
// The recurrent forecast continues every root μ of its subspace as μ^h; the
// extra roots of the minimum-norm recurrence lie inside the unit circle, so
// far ahead the roots of the components decide. A stationary oscillation of
// a bounded metric has its pair on the unit circle; a modulus above 1 is an
// amplitude that changed within the history (a modulated harmonic, two close
// periods beating) or an estimation error, and continuing it multiplies the
// error of the component's current amplitude by ρ^h. Leaving the pair out
// costs at most its amplitude; keeping it costs a growing one (on a production
// fleet a 7.7 h pair with 0.3% of the variance and ρ = 1.002 reached ±7e5
// within a week alone). That trade holds for a weak pair only: at a week the
// same rule dropped the main daily pair of 7 of 114 VMs (up to 76% of the
// variance, its amplitude up 18% a day within the history), and the forecast
// collapsed to the trend. A strong growing pair stays, and the forecast warns
// about its growth; continuing it at its last amplitude (roots moved onto the
// unit circle) would be the better fix. Trends are kept: a
// level can really grow, and the forecast warns about it instead.
func (a *Analysis) Forecastable(steps int) (group, left, growing []int) {
	for _, c := range a.Components {
		if c.Borderline() {
			continue
		}
		switch c.Kind {
		case KindHarmonic:
			if steps > 0 && math.Pow(c.Growth, float64(steps)) > Explosive && c.weak() {
				growing = append(growing, c.Comps...)
			} else {
				group = append(group, c.Comps...)
			}
		case KindTrend, KindSlow:
			group = append(group, c.Comps...)
		default:
			left = append(left, c.Comps...)
		}
	}
	return group, left, growing
}

// weak reports a component below WeakShare of the variance of every channel
// (a constant channel, with a NaN share, does not count).
func (c Component) weak() bool {
	for _, sh := range c.Share {
		if sh >= WeakShare {
			return false
		}
	}
	return true
}

// Describe says what the component is, with its period in points and, given
// the time step, in time units.
func (c Component) Describe(step time.Duration) string {
	switch c.Kind {
	case KindTrend:
		return "trend: slowly varying, no oscillation within the window"
	case KindSlow:
		return fmt.Sprintf("slow cycle, period %s (> %.1f window): part of the trend at this window", FormatPeriod(c.Period, step), slowPeriods)
	case KindHarmonic:
		d := "harmonic, period " + FormatPeriod(c.Period, step)
		if c.Order > 1 {
			d += fmt.Sprintf(" = 1/%d of %.1f", c.Order, c.Base)
		}
		return d
	case KindAlternating:
		return "alternating sign from point to point (period 2)"
	default:
		return fmt.Sprintf("oscillation, period ~%s, without a partner", FormatPeriod(c.Period, step))
	}
}

// ---- findings ----

// channelList formats one value per listed channel: "cpu 0.03, mem 0.05".
func (a *Analysis) channelList(cs []int, format string, value func(c int) any) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = a.names[c] + " " + fmt.Sprintf(format, value(c))
	}
	return strings.Join(parts, ", ")
}

func (a *Analysis) findNoise() {
	nm := a.Noise
	if nm.Negligible {
		a.note(LevelInfo, "%s practically no noise: the residual after the %d leading component(s) is at the level of rounding errors. The noise floor is set to the numerical precision of the decomposition, %.0e x sigma_0 = %.4g.", a.subject("The series has", "The channels have"), nm.Removed, numFloor, nm.Floor)
		return
	}
	var white, red, alt []int
	for c, ch := range nm.Channels {
		switch {
		case math.Abs(ch.Phi) < 0.1:
			white = append(white, c)
		case ch.Phi > 0:
			red = append(red, c)
		default:
			alt = append(alt, c)
		}
	}
	phis := func(cs []int) string {
		if !a.multi() {
			return fmt.Sprintf("phi = %.2f", nm.Channels[0].Phi)
		}
		return "phi of " + a.channelList(cs, "%.2f", func(c int) any { return nm.Channels[c].Phi })
	}
	if white != nil {
		a.note(LevelOK, "Noise %s, |phi| < 0.1: close to white, neighbouring points are practically independent.", phis(white))
	}
	if red != nil {
		a.note(LevelInfo, "Noise %s, phi >= 0.1: red, it wanders slowly, so its leading components are larger than white noise of the same sigma would give. The noise floor is computed for this AR(1) noise, which keeps slow random wander from being taken for a trend or a cycle.", phis(red))
	}
	if alt != nil {
		a.note(LevelInfo, "Noise %s, phi <= -0.1: consecutive deviations tend to alternate in sign (as in a rate derived from a counter sampled with jitter). The noise floor accounts for it.", phis(alt))
	}
	var heavy []int
	for c, ch := range nm.Channels {
		if ch.HeavyTailed() {
			heavy = append(heavy, c)
		}
	}
	if heavy != nil {
		ratio := func(c int) any { return nm.Channels[c].RMS / nm.Channels[c].SD }
		list := fmt.Sprintf("%.1f", ratio(0))
		if a.multi() {
			list = a.channelList(heavy, "%.1f", ratio)
		}
		a.note(LevelInfo, "Heavy-tailed noise: its standard deviation is %s times the robust sigma (more than %.1f): rare spikes or bursts carry most of the noise energy and spread it over the whole spectrum. The noise floor is computed for the full variance, and its autocorrelation from the raw residual, so bursts that do not repeat are not taken for structure; the robust sigma describes the quiet level.", list, heavyTails)
	}
}

func (a *Analysis) findStructure() {
	n := len(a.d.data(0))
	L, _ := a.d.window()
	var trend []int
	var periods []string
	var main *Component // the strongest harmonic
	for i, it := range a.Components {
		if it.Borderline() {
			continue
		}
		switch it.Kind {
		case KindTrend, KindSlow:
			trend = append(trend, it.Comps...)
		case KindHarmonic:
			periods = append(periods, FormatPeriod(it.Period, a.step))
			if main == nil {
				main = &a.Components[i]
			}
		}
	}
	if len(a.Structure) == 0 {
		a.note(LevelInfo, "No component reaches %.1f x the noise floor: at this window %s cannot be told apart from AR(1) noise.", structRatio, a.subject("the series", "the channels"))
	} else {
		var parts []string
		if trend != nil {
			parts = append(parts, "trend "+FormatGroup(trend))
		}
		if periods != nil {
			parts = append(parts, fmt.Sprintf("%d harmonic(s) with period %s", len(periods), strings.Join(periods, ", ")))
		}
		a.note(LevelInfo, "%d component(s) are structure (sigma >= %.1f x the noise floor): %s.", len(a.Structure), structRatio, strings.Join(parts, "; "))
	}

	// The main cycle: enough history, and a window that is a multiple of it.
	switch {
	case main != nil:
		P := main.Base
		c := float64(n) / P
		if c < minCycles {
			a.note(LevelWarn, "The history holds %.1f cycles of the main period %s, fewer than %.0f: its shape and amplitude are rough and the ends of the reconstructions less reliable. Use a longer history (for a daily cycle, two weeks or more).", c, FormatPeriod(P, a.step), minCycles)
		} else {
			a.note(LevelOK, "The history holds %.1f cycles of the main period %s (>= %.0f): enough to estimate its shape.", c, FormatPeriod(P, a.step), minCycles)
		}
		m := float64(L) / P
		best := int(math.Round(P) * max(1, math.Round(m)))
		tol := nearCalendar
		if a.Broken() {
			tol = nearBroken
		}
		calP, calName, calOK := a.calendar(P, tol)
		calM := float64(L) / calP
		fits := m >= 0.95 && math.Abs(m/math.Round(m)-1) <= 0.02
		switch {
		case a.Broken():
			text := "The main period %s is estimated on a history with stretches in another regime (see above) and is distorted by them: fitting with.window to it would chase the distortion."
			if calOK {
				text += fmt.Sprintf(" It is most likely the %s cycle (%s): keep with.window a multiple of it and analyse a history without the stretches.", calName, FormatPoints(int(math.Round(calP)), a.step))
			}
			a.note(LevelWarn, text, FormatPeriod(P, a.step))
		case fits:
			a.note(LevelOK, "The window %s is %.0f x the main period %s (a whole number within 2%%): the best window for separating this cycle.", FormatPoints(L, a.step), math.Round(m), FormatPeriod(P, a.step))
		// A period near a day or a week on a short history: the estimate is
		// biased (4 days of VM metrics gave 22.9-24.8 h), fitting the window to
		// it would chase the bias.
		case calOK && c < snapCycles && math.Abs(calM/math.Round(calM)-1) <= 0.02:
			a.note(LevelOK, "The main period %s is within %.0f%% of the %s cycle %s: most likely that cycle, its estimate biased by the short history (%.2f cycles, fewer than %.0f); the window %s is %.0f x it, right for it.", FormatPeriod(P, a.step), 100*nearCalendar, calName, FormatPoints(int(math.Round(calP)), a.step), c, snapCycles, FormatPoints(L, a.step), math.Round(calM))
		case calOK && c < snapCycles:
			a.note(LevelWarn, "The main period %s is within %.0f%% of the %s cycle %s: most likely that cycle, its estimate biased by the short history (%.2f cycles, fewer than %.0f). The window %s is not a multiple of it: set with.window to %s.", FormatPeriod(P, a.step), 100*nearCalendar, calName, FormatPoints(int(math.Round(calP)), a.step), c, snapCycles, FormatPoints(L, a.step), FormatPoints(int(math.Round(calP*max(1, math.Round(calM)))), a.step))
		case m < 0.95:
			a.note(LevelWarn, "The window %s is shorter than the main period %s: the cycle cannot be separated cleanly. Set with.window to a multiple of the period, e.g. %s.", FormatPoints(L, a.step), FormatPeriod(P, a.step), FormatPoints(int(math.Round(P)), a.step))
		default:
			a.note(LevelWarn, "The window %s is %.2f periods of the main cycle %s, off a whole number by more than 2%%: the cycle and its harmonics leak into neighbouring components and their periods are biased. Set with.window to %s, a multiple of the period.", FormatPoints(L, a.step), m, FormatPeriod(P, a.step), FormatPoints(best, a.step))
		}
	case len(a.Structure) > 0:
		a.note(LevelInfo, "No harmonic reaches %.1f x the noise floor: no periodic component at this window, the structure is trend only. If a cycle is expected, check that with.window is at least its period.", structRatio)
	}

	clean := true
	for _, it := range a.Components {
		if it.Borderline() {
			continue
		}
		c := FormatGroup(it.Comps)
		switch it.Kind {
		case KindHarmonic:
			if it.Gap > pairGap {
				clean = false
				a.note(LevelInfo, "Pair %s: its two sigma differ by %.0f%% (> %.0f%%): an impure harmonic, its amplitude changes over time or it shares energy with a neighbouring component.", c, 100*it.Gap, 100*pairGap)
			}
		case KindSlow:
			a.note(LevelInfo, "Components %s form a slow cycle with period %s, longer than %.1f x the window: at this window it is part of the trend. If it is a real seasonality (e.g. weekly), set with.window to a multiple of it and use a history of %.0f periods or more.", c, FormatPeriod(it.Period, a.step), slowPeriods, minCycles)
		case KindUnpaired:
			advice := "Set with.window to a multiple of the period, or put it into with.groups together with its likely partner."
			if a.Broken() {
				advice = "The stretches in another regime (see above) are the likely cause: analyse a history without them."
			}
			a.note(LevelWarn, "Component %s oscillates (period ~%s) but no neighbour has |w-corr| >= %.1f with it: the other half of this harmonic is not adjacent or is mixed with another component. %s", c, FormatPeriod(it.Period, a.step), pairWCorr, advice)
		case KindAlternating:
			a.note(LevelInfo, "Component %s alternates in sign from point to point: typical of a rate derived from a counter sampled with jitter, or of rounding.", c)
		}
	}
	if main != nil && clean {
		a.note(LevelOK, "Every harmonic pair has its two sigma within %.0f%% and |w-corr| >= %.1f between its halves: clean pairs.", 100*pairGap, pairWCorr)
	}
	if a.NoiseFrom >= a.d.Len() && a.d.errs() != nil {
		a.note(LevelWarn, "All %d computed components are above %.1f x the noise floor: the noise starts further down. Raise with.k to see it.", a.d.Len(), borderRatio)
	}
}

// clipHint writes the with.clip value for channel c.
func (a *Analysis) clipHint(c int, lo, hi float64) string {
	if a.multi() {
		return fmt.Sprintf("with.clip: {%s: [%g, %g]}", a.names[c], lo, hi)
	}
	return fmt.Sprintf("with.clip: [%g, %g]", lo, hi)
}

// on prefixes a finding about channel c with the channel name.
func (a *Analysis) on(c int) string {
	if a.multi() {
		return a.names[c] + ": "
	}
	return ""
}

func (a *Analysis) findBounds() {
	n := float64(len(a.d.data(0)))
	saturated := false
	for c, ch := range a.Noise.Channels {
		clipLo := ch.Min
		if ch.AtMin == 0 && ch.Min >= 0 {
			clipLo = 0
		}
		if ch.AtMax > 0 {
			saturated = true
			a.note(LevelWarn, "%s%.1f%% of the points sit exactly at the maximum %g, more than within one noise sigma below it: the series is clipped at a ceiling (saturation). Clipping flattens the peaks, and SSA represents the flat tops as extra harmonics of the main period (1/2, 1/3, ...): they belong with the cycle. Reconstructions and forecasts can overshoot the ceiling; clip them with %s. The noise is measured away from the ceiling.", a.on(c), 100*float64(ch.AtMax)/n, ch.Max, a.clipHint(c, clipLo, ch.Max))
		}
		if ch.AtMin > 0 {
			saturated = true
			a.note(LevelWarn, "%s%.1f%% of the points sit exactly at the minimum %g, more than within one noise sigma above it: the series is clipped at a floor (e.g. an idle machine at 0). Reconstructions and forecasts can undershoot it; clip them with %s. The noise is measured away from the floor.", a.on(c), 100*float64(ch.AtMin)/n, ch.Min, a.clipHint(c, ch.Min, ch.Max))
		}
	}
	if !saturated {
		a.note(LevelOK, "No saturation%s: neither the maximum nor the minimum is repeated more often than the values within one noise sigma of it.", a.subject("", " on any channel"))
	}
}

func (a *Analysis) findTopK() {
	errs := a.d.errs()
	if errs == nil {
		return
	}
	worst, wi := 0.0, -1
	for _, c := range a.Structure {
		if errs[c] > worst {
			worst, wi = errs[c], c
		}
	}
	iters := a.d.powerIters()
	switch {
	case worst > errMax:
		a.note(LevelWarn, "Top-k: component %d has err = %.1e > %.0e: not converged, its sigma and shape are approximate. Raise with.iters (now %d), e.g. to %d.", wi, worst, errMax, iters, 2*max(iters, 1))
	case wi >= 0:
		a.note(LevelOK, "Top-k: err <= %.1e for all structure components (threshold %.0e): converged.", worst, errMax)
	}
	if len(a.Structure) >= a.d.Len()-1 {
		a.note(LevelWarn, "%d of the %d computed components are structure: more may follow. Raise with.k.", len(a.Structure), a.d.Len())
	}
}

// ---- formatting ----

// FormatPeriod writes a period in points and, given the time step, in time units.
func FormatPeriod(period float64, step time.Duration) string {
	s := fmt.Sprintf("%.1f points", period)
	d := time.Duration(period * float64(step))
	switch {
	case step <= 0:
		return s
	case d >= 48*time.Hour:
		return s + fmt.Sprintf(" = %.2f days", d.Hours()/24)
	case d >= time.Hour:
		return s + fmt.Sprintf(" = %.2f h", d.Hours())
	default:
		return s + fmt.Sprintf(" = %.1f min", d.Minutes())
	}
}

// FormatPoints writes a whole number of points and, given the time step, the
// same length as a duration with.window accepts: "288 points (1d)".
func FormatPoints(p int, step time.Duration) string {
	if step <= 0 {
		return fmt.Sprintf("%d points", p)
	}
	return fmt.Sprintf("%d points (%s)", p, formatDuration(time.Duration(p)*step))
}

// formatDuration writes d as a Prometheus duration: whole days, hours,
// minutes, seconds and milliseconds, largest first (1d, 20h50m, 90s).
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
		if q := d / u.unit; q > 0 {
			b.WriteString(strconv.FormatInt(int64(q), 10) + u.name)
			d -= q * u.unit
		}
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
