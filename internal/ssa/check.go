package ssa

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/stat"
)

// Kinds of groups beyond the component kinds.
const (
	GroupTrend      = "trend"
	GroupCycle      = "cycle"
	GroupTrendCycle = "trend + cycle"
	GroupNoise      = "noise"
)

// Check describes a set of groups and how well they split the series.
type Check struct {
	Groups []GroupStat
	// Separation is the largest |w-correlation| between two groups (NaN
	// with fewer than two groups), SeparationPair the two groups.
	Separation     float64
	SeparationPair [2]int
	// ResidualCorr is the largest |w-correlation| between a group and the
	// residual, ResidualGroup that group.
	ResidualCorr  float64
	ResidualGroup int
	// LeftOut lists the structure components that are in no group.
	LeftOut []int
	// Residual describes, per channel, the series minus the union of the groups.
	Residual []ResidualStat
	Findings []Finding
}

// GroupStat describes one group.
type GroupStat struct {
	Comps []int
	// Kind is trend, cycle, trend + cycle, noise (only components below the
	// borderline), or the component kinds joined with " + ".
	Kind    string
	Periods []float64 // periods of its harmonics, in points
	// Values is the reconstruction of the group, per channel.
	Values   [][]float64
	Channels []GroupChannel
}

// GroupChannel describes the reconstruction of a group on one channel, in
// the units of the series.
type GroupChannel struct {
	Mean  float64
	Drift float64 // mean of the last stretch minus mean of the first (trend groups)
	// PeakToPeak is the swing away from the ends, where reconstructions are
	// less reliable (the whole series when it is shorter than 3 windows).
	PeakToPeak float64
	Min, Max   float64
	SD         float64
	Share      float64 // variance of the group / variance of the channel (NaN for a constant channel)
}

// ResidualStat describes the residual of one channel.
type ResidualStat struct {
	Explained float64 // 1 − var(residual)/var(series): the variance the groups explain
	SD        float64
	RobustSD  float64 // away from saturation
	Lag1      float64
	Spikes    int // points farther than 4 robust σ from the median
}

// Check describes groups and checks them: what each one is, whether they
// separate, what stays in the residual. Components must be computed ones,
// without repeats inside a group.
func (a *Analysis) Check(groups [][]int) (*Check, error) {
	if len(groups) == 0 {
		return nil, fmt.Errorf("no groups to check")
	}
	for _, g := range groups {
		if err := checkGroup(a.d.Len(), g); err != nil {
			return nil, err
		}
	}
	S := a.d.channels()
	n := len(a.d.data(0))
	ck := &Check{Separation: math.NaN()}
	R := make([][][]float64, len(groups))
	for j, g := range groups {
		R[j] = a.d.recon(g)
	}
	all := Union(groups)
	resid := a.residual(all)

	// What each group is. The level for relative amplitudes is the first trend group.
	kinds := make([]map[string]bool, len(groups))
	level := make([]float64, S)
	for c := range S {
		level[c] = stat.Mean(a.d.data(c), nil)
	}
	haveTrend := false
	for j, g := range groups {
		kinds[j] = map[string]bool{}
		for _, c := range g {
			if it := a.byComp[c]; it != nil {
				k := it.Kind
				if k == KindSlow {
					k = KindTrend
				}
				kinds[j][k] = true
			}
		}
		if len(kinds[j]) == 1 && kinds[j][KindTrend] && !haveTrend {
			haveTrend = true
			for c := range S {
				level[c] = stat.Mean(R[j][c], nil)
			}
		}
	}
	for j, g := range groups {
		ck.Groups = append(ck.Groups, a.describeGroup(ck, g, R[j], kinds[j], level))
	}

	if len(groups) > 1 {
		worst, wa, wb := 0.0, 0, 1
		for x := range groups {
			for y := x + 1; y < len(groups); y++ {
				if r := math.Abs(a.wcorr(R[x], R[y])); r > worst {
					worst, wa, wb = r, x, y
				}
			}
		}
		ck.Separation, ck.SeparationPair = worst, [2]int{wa, wb}
		pair := FormatGroup(groups[wa]) + " vs " + FormatGroup(groups[wb])
		switch {
		case worst < sepGood:
			ck.note(LevelOK, "Separability: the largest |w-corr| between groups is %.2f (%s) < %.1f: the groups are well separated, each captures its own part of the series.", worst, pair, sepGood)
		case worst < sepBad:
			ck.note(LevelInfo, "Separability: the largest |w-corr| between groups is %.2f (%s), between %.1f and %.1f: weakly mixed, a little of one group shows up in the other. Usually acceptable.", worst, pair, sepGood, sepBad)
		default:
			advice := "set with.window to a multiple of the main period"
			if a.Broken() {
				advice = "analyse a history without the stretches in another regime (see above)"
			}
			ck.note(LevelWarn, "Separability: the largest |w-corr| between groups is %.2f (%s) >= %.1f: the groups are mixed. Merge them, or %s.", worst, pair, sepBad, advice)
		}
	}

	worst, wj := 0.0, 0
	for j := range groups {
		if r := math.Abs(a.wcorr(R[j], resid)); r > worst {
			worst, wj = r, j
		}
	}
	ck.ResidualCorr, ck.ResidualGroup = worst, wj
	if worst < sepGood {
		ck.note(LevelOK, "Residual vs groups: the largest |w-corr| is %.2f < %.1f: nothing of the groups' patterns is left in the residual.", worst, sepGood)
	} else {
		ck.note(LevelWarn, "Residual vs groups: |w-corr| with group %s is %.2f >= %.1f: part of its pattern stays in the residual, some of its components are missing from the group.", FormatGroup(groups[wj]), worst, sepGood)
	}

	for _, c := range a.Structure {
		if !slices.Contains(all, c) {
			ck.LeftOut = append(ck.LeftOut, c)
		}
	}
	if ck.LeftOut != nil {
		ck.note(LevelWarn, "Structure component(s) %s are in no group: they stay in the residual. Add them to with.groups unless that is intended.", FormatGroup(ck.LeftOut))
	}

	ck.Residual = make([]ResidualStat, S)
	for c := range S {
		x, r := a.d.data(c), resid[c]
		rs := &ck.Residual[c]
		rs.Explained = math.NaN()
		if vx := stat.Variance(x, nil); vx > 0 {
			rs.Explained = 1 - stat.Variance(r, nil)/vx
		}
		rs.SD = stat.StdDev(r, nil)
		free := a.Noise.Channels[c].free(x, r)
		rs.RobustSD = robustSD(free)
		rs.Lag1 = lag1(r)
		med := median(free)
		for _, v := range free {
			if math.Abs(v-med) > spikeSigmas*rs.RobustSD {
				rs.Spikes++
			}
		}
	}
	a.findExplained(ck)
	a.findWhiteness(ck, n)
	a.findSpikes(ck, n)
	return ck, nil
}

func (ck *Check) note(level, format string, args ...any) {
	ck.Findings = append(ck.Findings, Finding{Level: level, Text: fmt.Sprintf(format, args...)})
}

// perChannel joins per-channel phrases: the phrase alone for one series,
// "cpu: phrase; mem: phrase" for several channels.
func (a *Analysis) perChannel(phrase func(c int) string) string {
	if !a.multi() {
		return phrase(0)
	}
	parts := make([]string, len(a.names))
	for c, name := range a.names {
		parts[c] = name + ": " + phrase(c)
	}
	return strings.Join(parts, "; ")
}

func (a *Analysis) describeGroup(ck *Check, g []int, y [][]float64, kinds map[string]bool, level []float64) GroupStat {
	L, _ := a.d.window()
	n, name := len(y[0]), FormatGroup(g)
	st := GroupStat{Comps: slices.Clone(g), Values: y, Channels: make([]GroupChannel, len(y))}
	var noisy, weak []int
	for _, c := range g {
		it := a.byComp[c]
		switch {
		case it == nil:
			noisy = append(noisy, c)
		case it.Borderline():
			weak = append(weak, c)
		}
		if it != nil && it.Kind == KindHarmonic && it.Comps[0] == c {
			st.Periods = append(st.Periods, it.Period)
		}
	}
	periods := make([]string, len(st.Periods))
	for i, T := range st.Periods {
		periods[i] = FormatPeriod(T, a.step)
	}
	m := max(1, min(L, n/10))
	for c := range y {
		core := y[c] // away from the ends, where reconstructions are less reliable
		if n > 3*L {
			core = y[c][L : n-L]
		}
		gc := &st.Channels[c]
		gc.Mean = stat.Mean(y[c], nil)
		gc.Drift = stat.Mean(y[c][n-m:], nil) - stat.Mean(y[c][:m], nil)
		gc.PeakToPeak = floats.Max(core) - floats.Min(core)
		gc.Min, gc.Max = floats.Min(y[c]), floats.Max(y[c])
		gc.SD = stat.PopStdDev(y[c], nil)
		gc.Share = math.NaN()
		if vx := stat.PopVariance(a.d.data(c), nil); vx > 0 {
			gc.Share = stat.PopVariance(y[c], nil) / vx
		}
	}
	switch {
	case len(kinds) == 0:
		st.Kind = GroupNoise
		ck.note(LevelWarn, "Group %s has only components below %.1f x the noise floor: its reconstruction is noise.", name, borderRatio)
		return st
	case len(kinds) == 1 && kinds[KindTrend]:
		st.Kind = GroupTrend
		a.describeTrend(ck, name, st)
	case len(kinds) == 1 && kinds[KindHarmonic]:
		st.Kind = GroupCycle
		ck.note(LevelInfo, "Group %s (cycle, period %s): %s.", name, strings.Join(periods, ", "), a.perChannel(func(c int) string {
			gc := st.Channels[c]
			rel := ""
			if level[c] != 0 {
				rel = fmt.Sprintf(", %.0f%% of the level %.4g", 100*gc.PeakToPeak/math.Abs(level[c]), level[c])
			}
			return fmt.Sprintf("peak-to-peak %.3g%s, the cycle moves the series by about +-%.3g around the trend", gc.PeakToPeak, rel, gc.PeakToPeak/2)
		}))
	case len(kinds) == 2 && kinds[KindTrend] && kinds[KindHarmonic]:
		st.Kind = GroupTrendCycle
		ck.note(LevelInfo, "Group %s (trend + cycle, period %s), the series without noise: %s.", name, strings.Join(periods, ", "), a.perChannel(func(c int) string {
			return fmt.Sprintf("range %.4g..%.4g", st.Channels[c].Min, st.Channels[c].Max)
		}))
		var over []string
		for c, gc := range st.Channels {
			x := a.d.data(c)
			if xlo, xhi := floats.Min(x), floats.Max(x); gc.Max > xhi || gc.Min < xlo {
				over = append(over, a.on(c)+fmt.Sprintf("observed range %.4g..%.4g", xlo, xhi))
			}
		}
		if over != nil {
			ck.note(LevelInfo, "Group %s leaves the observed range of the series (%s): smoothing overshoots near sharp edges. For a bounded metric, clip it with with.clip.", name, strings.Join(over, "; "))
		}
	default:
		var ks []string
		for k := range kinds {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		st.Kind = strings.Join(ks, " + ")
		ck.note(LevelInfo, "Group %s (%s): sigma of its reconstruction %s.", name, st.Kind, a.perChannel(func(c int) string {
			return fmt.Sprintf("%.4g", st.Channels[c].SD)
		}))
	}
	if noisy != nil {
		ck.note(LevelWarn, "Group %s includes component(s) %s below %.1f x the noise floor, i.e. noise: they add noise to the reconstruction, not structure.", name, FormatGroup(noisy), borderRatio)
	}
	if weak != nil {
		ck.note(LevelInfo, "Group %s includes borderline component(s) %s (%.1f-%.1f x the noise floor): weak structure or noise.", name, FormatGroup(weak), borderRatio, structRatio)
	}
	return st
}

// describeTrend says per channel whether the trend drifts and whether it is
// a straight line: what swings around the least-squares line is a slow cycle
// (e.g. weekly at a daily window) if it has a period that repeats within the
// series, and curvature otherwise.
func (a *Analysis) describeTrend(ck *Check, name string, st GroupStat) {
	drifts, waves, slow := false, false, false
	driftText := a.perChannel(func(c int) string {
		gc, sd := st.Channels[c], a.Noise.Channels[c].SD
		if math.Abs(gc.Drift) < sd {
			return fmt.Sprintf("level %.4g, changes by %+.3g from start to end, less than one noise sigma (%.3g): no drift", gc.Mean, gc.Drift, sd)
		}
		drifts = true
		dir, rel := "rises", ""
		if gc.Drift < 0 {
			dir = "falls"
		}
		if gc.Mean != 0 {
			rel = fmt.Sprintf(" (%.1f%% of the level)", 100*math.Abs(gc.Drift/gc.Mean))
		}
		return fmt.Sprintf("level %.4g, %s by %.3g from start to end%s, more than one noise sigma (%.3g): a real drift, range %.4g..%.4g", gc.Mean, dir, math.Abs(gc.Drift), rel, sd, gc.Min, gc.Max)
	})
	level := LevelOK
	if drifts {
		level = LevelInfo
	}
	ck.note(level, "Group %s (trend): %s.", name, driftText)

	waveText := a.perChannel(func(c int) string {
		y := st.Values[c]
		n := len(y)
		t := make([]float64, n)
		for i := range t {
			t[i] = float64(i)
		}
		b0, b1 := stat.LinearRegression(t, y, nil, false)
		wave := make([]float64, n)
		for i, v := range y {
			wave[i] = v - b0 - b1*t[i]
		}
		sw, sd := stat.PopStdDev(wave, nil), a.Noise.Channels[c].SD
		if sw <= sd {
			return fmt.Sprintf("close to a straight line, it deviates from one by sigma %.3g, less than one noise sigma", sw)
		}
		waves = true
		if T := a.peakPeriod([][]float64{wave}); T <= float64(n)/2 {
			slow = true
			return fmt.Sprintf("not a straight line, it swings by about +-%.3g (more than one noise sigma) with a dominant period of %s", math.Sqrt2*sw, FormatPeriod(T, a.step))
		}
		return fmt.Sprintf("curved, it deviates from a straight line by up to %.3g (more than one noise sigma) without a period that repeats within the series", max(floats.Max(wave), -floats.Min(wave)))
	})
	level, advice := LevelOK, ""
	if waves {
		level = LevelInfo
	}
	if slow {
		advice = fmt.Sprintf(" A repeating slow cycle (weekly?) is part of the trend at this window; to separate it, set with.window to a multiple of the period and use a history of %.0f periods or more.", minCycles)
	}
	ck.note(level, "Group %s shape: %s.%s", name, waveText, advice)
}

func (a *Analysis) findExplained(ck *Check) {
	verdict := func(ev float64) string {
		switch {
		case math.IsNaN(ev):
			return "a constant series"
		case ev < 0.5:
			return "mostly noise, the structure found is a small effect next to it"
		case ev < 0.9:
			return "both structure and noise matter"
		default:
			return "mostly structure"
		}
	}
	ck.note(LevelInfo, "The groups explain %s.", a.perChannel(func(c int) string {
		rs := ck.Residual[c]
		return fmt.Sprintf("%.1f%% of the variance around the mean (%s); residual sigma %.4g, robust sigma %.4g (the noise estimate was %.4g)", 100*rs.Explained, verdict(rs.Explained), rs.SD, rs.RobustSD, a.Noise.Channels[c].SD)
	}))
}

// findWhiteness checks the residual lag-1 autocorrelation of every channel
// against 2/sqrt(n) and groups the channels by verdict.
func (a *Analysis) findWhiteness(ck *Check, n int) {
	thr := 2 / math.Sqrt(float64(n))
	const (
		white = iota
		leftOut
		red
		rest
	)
	var by [4][]int
	for c, rs := range ck.Residual {
		switch {
		case math.Abs(rs.Lag1) < thr:
			by[white] = append(by[white], c)
		case ck.LeftOut != nil:
			by[leftOut] = append(by[leftOut], c)
		case a.Noise.Channels[c].Phi >= 0.1:
			by[red] = append(by[red], c)
		default:
			by[rest] = append(by[rest], c)
		}
	}
	r1 := func(cs []int) string {
		if !a.multi() {
			return fmt.Sprintf("%.3f", ck.Residual[0].Lag1)
		}
		return a.channelList(cs, "%.3f", func(c int) any { return ck.Residual[c].Lag1 })
	}
	if cs := by[white]; cs != nil {
		ck.note(LevelOK, "Residual lag-1 autocorrelation %s, |r1| < 2/sqrt(n) = %.3f: consistent with white noise, no short-range structure left.", r1(cs), thr)
	}
	if cs := by[leftOut]; cs != nil {
		ck.note(LevelWarn, "Residual lag-1 autocorrelation %s, |r1| >= 2/sqrt(n) = %.3f: the residual is not white; the structure components left out are the likely cause.", r1(cs), thr)
	}
	if cs := by[red]; cs != nil {
		ck.note(LevelInfo, "Residual lag-1 autocorrelation %s, |r1| >= 2/sqrt(n) = %.3f: the residual is not white, as expected for the red noise found (phi >= 0.1); not a sign of missing structure.", r1(cs), thr)
	}
	if cs := by[rest]; cs != nil {
		ck.note(LevelWarn, "Residual lag-1 autocorrelation %s, |r1| >= 2/sqrt(n) = %.3f: the residual is not white, some structure is left. Check the borderline components.", r1(cs), thr)
	}
}

// findSpikes compares the points farther than 4 robust sigma from the
// median of the residual with what Gaussian noise gives.
func (a *Analysis) findSpikes(ck *Check, n int) {
	expect := 6.3e-5 * float64(n) // P(|z| > 4) for Gaussian noise
	alarm := max(5, int(5*expect))
	var spiky []int
	for c, rs := range ck.Residual {
		if rs.Spikes >= alarm {
			spiky = append(spiky, c)
		}
	}
	if spiky == nil {
		ck.note(LevelOK, "No spikes: at most %d point(s) per %s deviate by more than %.0f sigma, below the alarm level of %d (Gaussian noise alone gives ~%.1f).", maxSpikes(ck), a.subject("series", "channel"), spikeSigmas, alarm, expect)
		return
	}
	list := fmt.Sprintf("%d points (%.2f%%)", ck.Residual[0].Spikes, 100*float64(ck.Residual[0].Spikes)/float64(n))
	if a.multi() {
		list = a.channelList(spiky, "%s", func(c int) any {
			return fmt.Sprintf("%d points (%.2f%%)", ck.Residual[c].Spikes, 100*float64(ck.Residual[c].Spikes)/float64(n))
		})
	}
	ck.note(LevelInfo, "%s deviate from the groups by more than %.0f sigma, Gaussian noise would give ~%.1f: bursts, or stretches in another regime (the analysis names the stretches away from the structure). SSA leaves them in the residual.", list, spikeSigmas, expect)
}

func maxSpikes(ck *Check) int {
	m := 0
	for _, rs := range ck.Residual {
		m = max(m, rs.Spikes)
	}
	return m
}
