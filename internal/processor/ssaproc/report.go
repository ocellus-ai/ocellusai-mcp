package ssaproc

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
	"github.com/ocellus-ai/ocellusai-mcp/internal/ssa"
)

// Sources of the checked groups.
const (
	sourceGiven     = "given"
	sourceSuggested = "suggested"
)

// explosive is the growth over the whole forecast from which it is flagged.
const explosive = ssa.Explosive

// maxRuns is the number of stretches of each kind the report lists.
const maxRuns = 10

// Kinds of stretches away from the usual profile.
const (
	runLong  = "long"
	runDaily = "daily"
	runBurst = "burst"
)

// report fills the entry of an analysed series.
func (j *job) report(d decomposition, step time.Duration) map[string]any {
	st, e, a := j.st, j.entry, d.analysis()
	L, K := d.window()
	e["window"] = L
	e["K"] = K
	sigma, errs := d.sigma(), d.errs()
	e["decomposition"] = "full"
	if errs != nil {
		e["decomposition"] = fmt.Sprintf("top-%d", len(sigma))
	}
	e["spectrum"] = spectrum(sigma, d.norm2(), errs)
	e["noise"] = j.noise(a.Noise)
	comps := make([]any, len(a.Components))
	for i, c := range a.Components {
		comps[i] = j.component(c, step)
	}
	e["components"] = comps
	e["noise_from"] = a.NoiseFrom
	e["structure"] = toAnyList(orEmpty(a.Structure))

	groups, labels, source := st.groups, []string(nil), sourceGiven
	if groups == nil {
		source = sourceSuggested
		for _, s := range a.Coarse {
			groups = append(groups, s.Comps)
			labels = append(labels, s.Label)
		}
		if len(a.Fine) > len(a.Coarse) {
			fine := make([]any, len(a.Fine))
			for i, s := range a.Fine {
				fine[i] = map[string]any{"group": ssa.FormatGroup(s.Comps), "components": toAnyList(s.Comps), "label": s.Label}
			}
			e["fine_groups"] = fine
		}
	}
	findings := a.Findings
	e["groups"] = []any{}
	if len(groups) > 0 {
		ck, err := a.Check(groups)
		if err != nil {
			return j.skip("with.groups: " + err.Error())
		}
		list := make([]any, len(ck.Groups))
		for i, gs := range ck.Groups {
			label := ""
			if labels != nil {
				label = labels[i]
			}
			list[i] = j.group(gs, source, label)
		}
		e["groups"] = list
		e["groups_check"] = checkEntry(ck, groups)
		e["residual"] = j.residual(ck.Residual)
		findings = append(findings, ck.Findings...)
	}
	e["broken"] = a.Broken()
	e["runs_total"] = len(a.Runs)
	e["runs"] = j.runs(a)
	e["recurring"] = j.recurring(a)
	fs := make([]any, len(findings))
	for i, f := range findings {
		fs[i] = map[string]any{"level": f.Level, "text": f.Text}
	}
	e["findings"] = fs
	if st.forecast.IsSet() {
		var group, left, growing []int
		if st.groups != nil {
			group = ssa.Union(st.groups)
		} else {
			group, left, growing = a.Forecastable(st.forecast.Resolve(j.grid.Step))
		}
		e["forecast"] = j.forecast(d, group, left, growing, st.groups != nil)
	}
	return e
}

func runKind(r ssa.Run) string {
	switch {
	case r.Daily:
		return runDaily
	case r.Long:
		return runLong
	default:
		return runBurst
	}
}

// runs lists the stretches away from the usual profile: the maxRuns largest
// (points × |deviation|) of each kind, chronologically.
func (j *job) runs(a *ssa.Analysis) []any {
	byKind := map[string][]ssa.Run{}
	for _, r := range a.Runs {
		byKind[runKind(r)] = append(byKind[runKind(r)], r)
	}
	size := func(r ssa.Run) float64 { return float64(r.Len()) * math.Abs(r.Deviation) }
	var keep []ssa.Run
	for _, rs := range byKind {
		slices.SortStableFunc(rs, func(x, y ssa.Run) int { return cmp.Compare(size(y), size(x)) })
		keep = append(keep, rs[:min(len(rs), maxRuns)]...)
	}
	slices.SortFunc(keep, func(x, y ssa.Run) int {
		return cmp.Or(cmp.Compare(x.From, y.From), cmp.Compare(x.Channel, y.Channel))
	})
	out := make([]any, len(keep))
	for i, r := range keep {
		e := map[string]any{
			"kind":       runKind(r),
			"start":      j.time(r.From),
			"start_time": series.FormatTime(j.time(r.From)),
			"end":        j.time(r.To),
			"end_time":   series.FormatTime(j.time(r.To)),
			"points":     r.Len(),
			"duration":   float64(r.Len()) * j.grid.Step,
			"deviation":  r.Deviation,
		}
		if j.multi {
			e["dim"] = j.dims[r.Channel]
		}
		out[i] = e
	}
	return out
}

// recurring lists the groups of stretches that recur at one time of day.
func (j *job) recurring(a *ssa.Analysis) []any {
	out := make([]any, len(a.Recurring))
	for i, rec := range a.Recurring {
		lens := make([]float64, len(rec.Runs))
		devs := make([]float64, len(rec.Runs))
		for k, ri := range rec.Runs {
			lens[k] = float64(a.Runs[ri].Len())
			devs[k] = a.Runs[ri].Deviation
		}
		secs := rec.At.Seconds()
		e := map[string]any{
			"time_of_day":    fmt.Sprintf("%02d:%02d", int(secs)/3600, int(secs)%3600/60),
			"seconds_of_day": secs,
			"spread":         rec.Spread.Seconds(),
			"days":           rec.Days,
			"of_days":        rec.Of,
			"runs":           len(rec.Runs),
			"duration":       median(lens) * j.grid.Step,
			"deviation":      median(devs),
		}
		if j.multi {
			e["dim"] = j.dims[rec.Channel]
		}
		out[i] = e
	}
	return out
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

func spectrum(sigma []float64, norm2 float64, errs []float64) []any {
	out := make([]any, min(len(sigma), spectrumShow))
	for i := range out {
		row := map[string]any{"component": i, "sigma": sigma[i], "share": 0.0}
		if norm2 > 0 {
			row["share"] = sigma[i] * sigma[i] / norm2
		}
		if errs != nil {
			row["err"] = finiteOrNil(errs[i])
		}
		out[i] = row
	}
	return out
}

func (j *job) noise(nm ssa.Noise) map[string]any {
	sd, phi := make([]float64, len(nm.Channels)), make([]float64, len(nm.Channels))
	sat := map[string]any{}
	for c, ch := range nm.Channels {
		sd[c], phi[c] = ch.SD, ch.Phi
		s := map[string]any{}
		if ch.AtMax > 0 {
			s["ceiling"], s["at_ceiling"] = ch.Max, ch.AtMax
		}
		if ch.AtMin > 0 {
			s["floor"], s["at_floor"] = ch.Min, ch.AtMin
		}
		if len(s) > 0 {
			sat[j.dims[c]] = s
		}
	}
	out := map[string]any{
		"floor":      nm.Floor,
		"removed":    nm.Removed,
		"negligible": nm.Negligible,
		"sd":         j.perDim(sd),
		"phi":        j.perDim(phi),
	}
	switch {
	case len(sat) == 0:
	case j.multi:
		out["saturation"] = sat
	default:
		out["saturation"] = sat[j.dims[0]]
	}
	return out
}

func (j *job) component(c ssa.Component, step time.Duration) map[string]any {
	e := map[string]any{
		"group":           ssa.FormatGroup(c.Comps),
		"components":      toAnyList(c.Comps),
		"kind":            c.Kind,
		"meaning":         c.Describe(step),
		"sigma":           c.Sigma,
		"ratio":           c.Ratio,
		"borderline":      c.Borderline(),
		"variance_share":  j.perDim(c.Share),
		"growth_per_step": finiteOrNil(c.Growth),
	}
	if c.Period > 0 {
		e["period"] = c.Period
		e["period_seconds"] = c.Period * j.grid.Step
	}
	if c.Order > 0 {
		e["order"] = c.Order
		e["base_period"] = c.Base
	}
	if len(c.Comps) == 2 {
		e["pair_gap"] = c.Gap
	}
	return e
}

func (j *job) group(gs ssa.GroupStat, source, label string) map[string]any {
	e := map[string]any{
		"group":      ssa.FormatGroup(gs.Comps),
		"components": toAnyList(gs.Comps),
		"source":     source,
		"kind":       gs.Kind,
	}
	if label != "" {
		e["label"] = label
	}
	if gs.Periods != nil {
		secs := make([]float64, len(gs.Periods))
		for i, T := range gs.Periods {
			secs[i] = T * j.grid.Step
		}
		e["periods"] = toAnyList(gs.Periods)
		e["periods_seconds"] = toAnyList(secs)
	}
	stat := func(f func(ssa.GroupChannel) float64) any {
		vals := make([]float64, len(gs.Channels))
		for c, gc := range gs.Channels {
			vals[c] = f(gc)
		}
		return j.perDim(vals)
	}
	e["mean"] = stat(func(g ssa.GroupChannel) float64 { return g.Mean })
	e["drift"] = stat(func(g ssa.GroupChannel) float64 { return g.Drift })
	e["peak_to_peak"] = stat(func(g ssa.GroupChannel) float64 { return g.PeakToPeak })
	e["min"] = stat(func(g ssa.GroupChannel) float64 { return g.Min })
	e["max"] = stat(func(g ssa.GroupChannel) float64 { return g.Max })
	e["variance_share"] = stat(func(g ssa.GroupChannel) float64 { return g.Share })
	if j.st.reconstruction {
		vals, clipped := j.clip(gs.Values, gs.Comps)
		e["points"] = j.points(vals, 0, j.grid.Len())
		e["clipped"] = clipped
	}
	return e
}

func checkEntry(ck *ssa.Check, groups [][]int) map[string]any {
	e := map[string]any{
		"separation":     finiteOrNil(ck.Separation),
		"residual_wcorr": ck.ResidualCorr,
		"residual_group": ssa.FormatGroup(groups[ck.ResidualGroup]),
		"left_out":       toAnyList(orEmpty(ck.LeftOut)),
	}
	if len(groups) > 1 {
		e["separation_between"] = []any{ssa.FormatGroup(groups[ck.SeparationPair[0]]), ssa.FormatGroup(groups[ck.SeparationPair[1]])}
	}
	return e
}

func (j *job) residual(rs []ssa.ResidualStat) map[string]any {
	col := func(f func(ssa.ResidualStat) float64) any {
		vals := make([]float64, len(rs))
		for c, r := range rs {
			vals[c] = f(r)
		}
		return j.perDim(vals)
	}
	return map[string]any{
		"explained": col(func(r ssa.ResidualStat) float64 { return r.Explained }),
		"sd":        col(func(r ssa.ResidualStat) float64 { return r.SD }),
		"robust_sd": col(func(r ssa.ResidualStat) float64 { return r.RobustSD }),
		"lag1":      col(func(r ssa.ResidualStat) float64 { return r.Lag1 }),
		"spikes":    col(func(r ssa.ResidualStat) float64 { return float64(r.Spikes) }),
	}
}

// forecast continues group by with.forecast points; given reports whether
// the group comes from with.groups; left and growing list the structure
// components a default group leaves out: without a stable continuation, and
// harmonic pairs whose roots grow more than explosive times over the horizon.
func (j *job) forecast(d decomposition, group, left, growing []int, given bool) map[string]any {
	st := j.st
	e := map[string]any{"method": st.method}
	if out := append(slices.Clone(left), growing...); len(out) > 0 {
		slices.Sort(out)
		e["left_out"] = toAnyList(out)
	}
	if growing != nil {
		steps := st.forecast.Resolve(j.grid.Step)
		var list []any
		for _, c := range d.analysis().Components {
			if !slices.Contains(growing, c.Comps[0]) {
				continue
			}
			list = append(list, map[string]any{
				"group":           ssa.FormatGroup(c.Comps),
				"components":      toAnyList(c.Comps),
				"period":          c.Period,
				"period_seconds":  c.Period * j.grid.Step,
				"variance_share":  j.perDim(c.Share),
				"growth_per_step": c.Growth,
				"growth":          finiteOrNil(math.Pow(c.Growth, float64(steps))),
			})
		}
		e["growing"] = list
	}
	if len(group) == 0 {
		e["reason"] = fmt.Sprintf("no trend or harmonic rises above %.1f x the noise floor: there is no structure to continue (name the components in with.groups to forecast them anyway)", ssa.StructRatio)
		return e
	}
	e["group"] = ssa.FormatGroup(group)
	e["components"] = toAnyList(group)
	if given {
		e["source"] = sourceGiven
	} else {
		e["source"] = "structure"
	}
	steps := st.forecast.Resolve(j.grid.Step)
	Y, err := d.forecast(st.method, group, steps)
	if err != nil {
		e["reason"] = err.Error()
		return e
	}
	// The largest ESPRIT root of the group is the growth per step of its
	// fastest-growing part: above 1 the forecast grows geometrically.
	var warnings []string
	if es, err := d.esprit(group); err == nil {
		rho := 0.0
		for _, r := range es.Rho {
			rho = max(rho, r)
		}
		e["growth_per_step"] = rho
		if g := math.Pow(rho, float64(steps)); g > explosive {
			warnings = append(warnings, fmt.Sprintf("the recurrence has a root growing x%.4g per step, x%.3g over the %d forecast steps", rho, g, steps))
		}
	}
	if w := j.leaves(Y); w != "" {
		warnings = append(warnings, w)
	}
	var parts []string
	if warnings != nil {
		parts = append(parts, strings.Join(warnings, "; ")+": the forecast is unstable, do not trust its far end; leave the growing components out of with.groups, shorten with.forecast or set with.window to a multiple of the main period")
	}
	if w := j.broken(d.analysis()); w != "" {
		parts = append(parts, w)
	}
	if parts != nil {
		e["warning"] = strings.Join(parts, "; ")
	}
	Y, clipped := j.clip(Y, group)
	n := j.grid.Len()
	pts := j.points(Y, n, n+steps)
	e["steps"] = steps
	e["clipped"] = clipped
	e["start"] = j.time(n)
	e["start_time"] = series.FormatTime(j.time(n))
	e["end"] = j.time(n + steps - 1)
	e["end_time"] = series.FormatTime(j.time(n + steps - 1))
	e["points"] = pts
	return e
}

// broken describes the longest stretch in another regime, "" when there is
// none.
func (j *job) broken(a *ssa.Analysis) string {
	var worst *ssa.Run
	count := 0
	for i, r := range a.Runs {
		if r.Long && !r.Daily {
			count++
			if worst == nil || r.Len() > worst.Len() {
				worst = &a.Runs[i]
			}
		}
	}
	if worst == nil {
		return ""
	}
	name := ""
	if j.multi {
		name = j.dims[worst.Channel] + " "
	}
	side := "above"
	if worst.Deviation < 0 {
		side = "below"
	}
	return fmt.Sprintf("the history has %d stretch(es) in another regime, the longest %s%s to %s (%s the usual profile by %.4g): the structure and its forecast are distorted by them, forecast from a history without them",
		count, name, series.FormatTime(j.time(worst.From)), series.FormatTime(j.time(worst.To)), side, math.Abs(worst.Deviation))
}

// leaves describes the first channel whose forecast (before clipping) leaves
// the observed range by more than its width, "" when none does.
func (j *job) leaves(y [][]float64) string {
	n := j.grid.Len()
	for c, ch := range y {
		lo, hi := slices.Min(j.cols[c]), slices.Max(j.cols[c])
		span := hi - lo
		for i := n; i < len(ch); i++ {
			if ch[i] > hi+span || ch[i] < lo-span {
				name := ""
				if j.multi {
					name = j.dims[c] + " "
				}
				return fmt.Sprintf("the %sforecast reaches %.4g at %s, beyond the observed range %.4g..%.4g by more than its width", name, ch[i], series.FormatTime(j.time(i)), lo, hi)
			}
		}
	}
	return ""
}

// time returns the timestamp of grid index i, also past the end of the grid.
func (j *job) time(i int) float64 { return j.grid.Times[0] + float64(i)*j.grid.Step }

// clip clips every channel of y to its with.clip range when group holds
// component 0, the level; the input is not modified.
func (j *job) clip(y [][]float64, group []int) ([][]float64, bool) {
	if !slices.Contains(group, 0) {
		return y, false
	}
	out := make([][]float64, len(y))
	clipped := false
	for c := range y {
		b, ok := j.st.clipFor(j.dims[c])
		if !ok {
			out[c] = y[c]
			continue
		}
		clipped = true
		out[c] = make([]float64, len(y[c]))
		for i, v := range y[c] {
			out[c][i] = min(max(v, b.lo), b.hi)
		}
	}
	return out, clipped
}

// points writes indices [from, to) of y as [[ts, v]] (ssa) or
// [[ts, v1, v2, …]] (ssa_mv).
func (j *job) points(y [][]float64, from, to int) []any {
	out := make([]any, 0, to-from)
	for i := from; i < to; i++ {
		row := make([]any, 0, len(y)+1)
		row = append(row, j.time(i))
		for c := range y {
			row = append(row, finiteOrNil(y[c][i]))
		}
		out = append(out, row)
	}
	return out
}

func orEmpty(xs []int) []int {
	if xs == nil {
		return []int{}
	}
	return xs
}
