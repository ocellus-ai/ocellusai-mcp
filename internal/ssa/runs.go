package ssa

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Stretches of the series away from its usual profile. An outage followed by
// a catch-up bends the trend and the periods: on a production fleet (2026-09-28) a
// 13-hour ingest outage with a catch-up inside four days of history turned a
// daily cycle into a 27-28 h one, and the advice to fit the window to that
// period made it worse. The SSA structure cannot serve as the reference: it
// reproduces a long stretch with extra components, and a burst far above a
// quiet level lifts the trend around it, so the residual rings. The usual
// profile is robust instead: the rolling median over three cycles plus the
// median of every phase of the cycle across the cycles. A stretch shorter
// than one and a half cycles does not move the first, and one that does not
// fall on the same phase in most cycles does not move the second.
const (
	runGap       = 2     // off points at most this many points apart join one stretch
	longShare    = 12    // a stretch of L/12 points or more (2 h at a daily window) is long
	minLong      = 6     // … and of at least this many points
	dayWindow    = 3600. // stretches recurring daily start within ±1 h (seconds) of one time of day
	minDays      = 3     // … on at least this many days
	nearCalendar = 0.07  // a main period within 7% of a day or a week is that calendar cycle
	nearBroken   = 0.2   // … within 20% when stretches in another regime distort it (27-28 h live)
	snapCycles   = 7.0   // … its estimate is rough below this many cycles in the history
	showRuns     = 3     // findings name at most this many stretches
)

// Run is a stretch of consecutive points of one channel farther than
// spikeSigmas robust σ (and the smallest meaningful change, if set) outside
// the range its usual profile covers within an hour of each point, all on
// one side of it (off points at most runGap apart join).
type Run struct {
	Channel  int
	From, To int // first and last point, inclusive
	// Deviation is the mean of the series minus its usual profile over the
	// stretch, in the units of the series: negative below it.
	Deviation float64
	// Long reports a stretch of at least the long length (LongLen): another
	// regime rather than a burst, unless it recurs daily.
	Long bool
	// Daily reports a stretch of a group that recurs at one time of day.
	Daily bool
}

// Len is the number of points of the stretch.
func (r Run) Len() int { return r.To - r.From + 1 }

// Recurrence is a group of stretches of one channel starting at about the
// same time of day on different days: a scheduled job.
type Recurrence struct {
	Channel int
	Runs    []int // indices into Analysis.Runs, chronological
	// Days is the number of days with a stretch of the group, Of the number
	// of times the time of day occurs in the history.
	Days, Of int
	// At is the mean start as time of day: UTC when the start of the series
	// is known, counted from the time of day of the first point otherwise.
	At time.Duration
	// Spread is the largest distance of a start from At.
	Spread time.Duration
}

// Broken reports stretches in another regime: long stretches away from the
// usual profile that do not recur daily. The trend, the periods and a forecast
// are distorted by them.
func (a *Analysis) Broken() bool {
	for _, r := range a.Runs {
		if r.Long && !r.Daily {
			return true
		}
	}
	return false
}

// LongLen is the length from which a stretch is long.
func (a *Analysis) LongLen() int {
	L, _ := a.d.window()
	return max(minLong, int(math.Round(float64(L)/longShare)))
}

// findRuns finds the stretches of every channel away from its usual
// profile, groups those that recur at one time of day and writes the
// findings.
func (a *Analysis) findRuns() {
	a.cycle, a.profile = a.usualCycle()
	long := a.LongLen()
	for c := range a.d.channels() {
		x := a.d.data(c)
		ch := a.Noise.Channels[c]
		// The profile, then again without the points far from it (the
		// robustness round of STL): an outage and its catch-up on the same
		// hours of two days out of four would otherwise make that phase of
		// the profile theirs, and the normal days look off.
		var r, ref []float64
		var med, sd float64
		var mask []bool
		for range 2 {
			ref = usual(x, a.cycle, a.medianWindow(), mask)
			r = make([]float64, len(x))
			for i, v := range x {
				r[i] = v - ref[i]
			}
			free := ch.free(x, r)
			sd, med = robustSD(free), median(free)
			if sd == 0 {
				break
			}
			mask = make([]bool, len(x))
			for i, v := range r {
				mask[i] = math.Abs(v-med) > spikeSigmas*sd
			}
		}
		if sd == 0 {
			continue
		}
		band := spikeSigmas * sd
		if c < len(a.minDelta) {
			band = max(band, a.minDelta[c])
		}
		for _, run := range offRuns(beyond(x, ref, med, max(0, a.cycle/24)), 0, band) {
			run.Channel = c
			var sum float64
			for i := run.From; i <= run.To; i++ {
				sum += r[i] - med
			}
			run.Deviation = sum / float64(run.Len())
			run.Long = run.Len() >= long
			a.Runs = append(a.Runs, run)
		}
	}
	for c := range a.d.channels() {
		a.recur(c)
	}
	a.noteRuns()
}

// usualCycle returns the cycle of the usual profile in points and its name:
// a day when the time step divides it and the history holds three days, else
// none (0). The main period of the decomposition is no substitute: on a short
// or broken history it is biased by a few percent, and phases taken with it
// slip by a whole cycle within the series.
func (a *Analysis) usualCycle() (int, string) {
	if a.step > 0 && (24*time.Hour)%a.step == 0 {
		if day := int(24 * time.Hour / a.step); len(a.d.data(0)) >= 3*day {
			return day, "daily profile"
		}
	}
	return 0, "level"
}

// medianWindow is the window of the rolling median: three cycles, or the SSA
// window without a cycle.
func (a *Analysis) medianWindow() int {
	if a.cycle > 0 {
		return 3 * a.cycle
	}
	L, _ := a.d.window()
	return L
}

// usual returns the usual profile of x: a robust line (repeated median) through
// the medians of whole blocks of T = period points (w without a cycle), plus the
// rolling median of the rest over w points in windows kept inside the
// series, plus, with a cycle of T points, the median of every phase ±T/96
// pooled over the cycles. A drift does not bias the ends, a stretch shorter
// than half of w does not move the level, and a burst on the same phase in
// two cycles out of four is a minority of its pool.
//
// mask, if not nil, marks the points the medians skip (a window with no other
// point takes them all).
func usual(x []float64, period, w int, mask []bool) []float64 {
	T, n := period, len(x)
	w = max(1, min(w, n))
	block := T
	if block < 2 {
		block = w
	}
	var bx, by []float64
	for k := 0; (k+1)*block <= n; k++ {
		bx = append(bx, float64(k*block)+float64(block-1)/2)
		by = append(by, medianOf(x, mask, k*block, (k+1)*block))
	}
	// Siegel's repeated median: one block in another regime (an outage day
	// out of four) spoils every slope through it, the median over the other
	// blocks of the median slope through each is still clean.
	var slope float64
	if len(bx) >= 2 {
		per := make([]float64, len(bx))
		slopes := make([]float64, 0, len(bx)-1)
		for i := range bx {
			slopes = slopes[:0]
			for j := range bx {
				if j != i {
					slopes = append(slopes, (by[j]-by[i])/(bx[j]-bx[i]))
				}
			}
			per[i] = median(slopes)
		}
		slope = median(per)
	}
	icpt := medianOf(x, mask, 0, n)
	if len(bx) > 0 {
		c := make([]float64, len(bx))
		for i := range bx {
			c[i] = by[i] - slope*bx[i]
		}
		icpt = median(c)
	}
	line := make([]float64, n)
	for i := range line {
		line[i] = icpt + slope*float64(i)
	}
	// Two rounds as in STL: the level of the series without its seasonal
	// part, then the seasonal part of the series without its level. The
	// level of the raw series would move with any stretch in its window: a
	// cycle puts few values near its median, so a few hours of an outage in
	// three days shift the median by a quarter of the swing.
	season := make([]float64, n)
	var ref []float64
	for range 2 {
		d := make([]float64, n)
		for i, v := range x {
			d[i] = v - line[i] - season[i]
		}
		ref = rollingMedian(d, w, mask)
		for i := range ref {
			ref[i] += line[i]
		}
		if T < 2 {
			return ref
		}
		for i, v := range x {
			d[i] = v - ref[i]
		}
		season = phaseMedian(d, T, max(1, T/96), mask)
	}
	for i := range ref {
		ref[i] += season[i]
	}
	return ref
}

// rollingMedian returns the median of d over w points around every point, in
// windows kept inside the series, evaluated every w/72 points and
// interpolated in between.
func rollingMedian(d []float64, w int, mask []bool) []float64 {
	n := len(d)
	stride := max(1, w/72)
	var at []int
	for i := 0; i < n; i += stride {
		at = append(at, i)
	}
	if at[len(at)-1] != n-1 {
		at = append(at, n-1)
	}
	lvl := make([]float64, len(at))
	for k, i := range at {
		lo := min(max(0, i-w/2), n-w)
		lvl[k] = medianOf(d, mask, lo, lo+w)
	}
	out := make([]float64, n)
	for k := range at {
		if k+1 == len(at) {
			out[at[k]] = lvl[k]
			break
		}
		i0, i1 := at[k], at[k+1]
		for i := i0; i < i1; i++ {
			out[i] = lvl[k] + (lvl[k+1]-lvl[k])*float64(i-i0)/float64(i1-i0)
		}
	}
	return out
}

// phaseMedian returns, for every point, the median of d over the points of
// its phase ±h in every cycle of T = period points, skipping the masked
// ones.
func phaseMedian(d []float64, period, h int, mask []bool) []float64 {
	T, n := period, len(d)
	byPhase := make([]float64, T)
	var pool, all []float64
	for p := range T {
		pool, all = pool[:0], all[:0]
		for s := 0; s < n+T; s += T {
			for i := max(0, s+p-h); i <= min(n-1, s+p+h); i++ {
				all = append(all, d[i])
				if mask == nil || !mask[i] {
					pool = append(pool, d[i])
				}
			}
		}
		if len(pool) == 0 {
			pool = all
		}
		byPhase[p] = median(pool)
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = byPhase[i%T]
	}
	return out
}

// beyond returns how far every point of x lies outside the range the
// profile ref (shifted by med) covers within h points of it, 0 inside. A day
// that runs the usual profile an hour late (a weekend morning on the live
// fleet: the rise of received traffic started an hour later, 60 Mbit/s below
// the profile on the slope for two hours) is not away from it; an outage or
// a job on a flat part of the profile is.
func beyond(x, ref []float64, med float64, h int) []float64 {
	n := len(x)
	out := make([]float64, n)
	for i, v := range x {
		lo, hi := math.Inf(1), math.Inf(-1)
		for k := max(0, i-h); k <= min(n-1, i+h); k++ {
			lo, hi = min(lo, ref[k]+med), max(hi, ref[k]+med)
		}
		switch {
		case v > hi:
			out[i] = v - hi
		case v < lo:
			out[i] = v - lo
		}
	}
	return out
}

// medianOf returns the median of x[lo:hi] without the masked points, of all
// of them when every one is masked.
func medianOf(x []float64, mask []bool, lo, hi int) float64 {
	if mask == nil {
		return median(x[lo:hi])
	}
	vals := make([]float64, 0, hi-lo)
	for i := lo; i < hi; i++ {
		if !mask[i] {
			vals = append(vals, x[i])
		}
	}
	if len(vals) == 0 {
		return median(x[lo:hi])
	}
	return median(vals)
}

// offRuns returns the stretches of r farther than band from med, split where
// the side changes and where the next off point is more than runGap points
// away.
func offRuns(r []float64, med, band float64) []Run {
	var out []Run
	var below []bool
	for i, v := range r {
		d := v - med
		if math.Abs(d) <= band {
			continue
		}
		if k := len(out) - 1; k >= 0 && below[k] == (d < 0) && i-out[k].To-1 <= runGap {
			out[k].To = i
			continue
		}
		out = append(out, Run{From: i, To: i})
		below = append(below, d < 0)
	}
	return out
}

// seconds returns the time of point i in seconds: unix time when the start
// of the series is known, from the first point otherwise.
func (a *Analysis) seconds(i int) float64 {
	s := float64(i) * a.step.Seconds()
	if !a.start.IsZero() {
		s += float64(a.start.UnixNano()) / 1e9
	}
	return s
}

// recur finds groups of stretches of channel c that start within ±1 h of one
// time of day on at least minDays days and on at least half of the days the
// history covers; the more stretches a channel has per day, the more days a
// group needs, so that chance does not make a group of frequent bursts.
func (a *Analysis) recur(c int) {
	const day = 86400.
	n := len(a.d.data(0))
	first, last := a.seconds(0), a.seconds(n-1)
	if a.step <= 0 || last-first < minDays*day {
		return
	}
	var idx []int
	for i, r := range a.Runs {
		if r.Channel == c {
			idx = append(idx, i)
		}
	}
	perDay := float64(len(idx)) / ((last - first) / day)
	chance := 1 - math.Exp(-perDay*2*dayWindow/day) // a random ±1 h window holds a stretch on this share of days
	tod := func(i int) float64 { return math.Mod(a.seconds(a.Runs[i].From), day) }
	dist := func(x, y float64) float64 { d := math.Abs(x - y); return min(d, day-d) }
	for {
		var free []int
		for _, i := range idx {
			if !a.Runs[i].Daily {
				free = append(free, i)
			}
		}
		// The group around each start, among the stretches on its side of
		// the profile; the one covering the most days wins.
		var best, side []int
		bestDays := 0
		for _, i := range free {
			var same []int
			for _, j := range free {
				if (a.Runs[j].Deviation < 0) == (a.Runs[i].Deviation < 0) {
					same = append(same, j)
				}
			}
			if members, days := a.around(same, tod(i), tod, dist); days > bestDays {
				best, bestDays, side = members, days, same
			}
		}
		if bestDays < minDays {
			return
		}
		// Recentre on the circular mean of the group and take it again.
		var sx, sy float64
		for _, i := range best {
			w := 2 * math.Pi * tod(i) / day
			sx, sy = sx+math.Cos(w), sy+math.Sin(w)
		}
		at := math.Mod(math.Atan2(sy, sx)/(2*math.Pi)*day+day, day)
		best, bestDays = a.around(side, at, tod, dist)
		// The number of days whose window around the time of day at meets the
		// history (a window may reach past either end).
		of := 0
		for k := math.Floor((first-at)/day) - 1; k*day+at-dayWindow <= last; k++ {
			if k*day+at+dayWindow >= first {
				of++
			}
		}
		if bestDays < minDays || float64(bestDays) < max(0.5, 2*chance)*float64(of) {
			return
		}
		rec := Recurrence{Channel: c, Days: bestDays, Of: of, At: time.Duration(at * float64(time.Second))}
		for _, i := range best {
			a.Runs[i].Daily = true
			rec.Runs = append(rec.Runs, i)
			rec.Spread = max(rec.Spread, time.Duration(dist(tod(i), at)*float64(time.Second)))
		}
		slices.Sort(rec.Runs)
		a.Recurring = append(a.Recurring, rec)
	}
}

// around returns the stretches of free starting within dayWindow of the time
// of day t, one per day (the one starting nearest to t), and the number of
// days.
func (a *Analysis) around(free []int, t float64, tod func(int) float64, dist func(x, y float64) float64) ([]int, int) {
	byDay := map[int]int{}
	for _, i := range free {
		if dist(tod(i), t) > dayWindow {
			continue
		}
		d := int(math.Floor(a.seconds(a.Runs[i].From) / 86400))
		if j, ok := byDay[d]; !ok || dist(tod(i), t) < dist(tod(j), t) {
			byDay[d] = i
		}
	}
	out := make([]int, 0, len(byDay))
	for _, i := range byDay {
		out = append(out, i)
	}
	slices.Sort(out)
	return out, len(byDay)
}

// noteRuns writes the findings about the stretches: the long ones (another
// regime), the groups recurring daily, and the short bursts.
func (a *Analysis) noteRuns() {
	n := len(a.d.data(0))
	expect := 6.3e-5 * float64(n) // P(|z| > 4) for Gaussian noise, per point
	alarm := max(3, int(math.Ceil(5*expect)))
	for c := range a.d.channels() {
		var long, short []Run
		covered := 0
		for _, r := range a.Runs {
			switch {
			case r.Channel != c || r.Daily:
			case r.Long:
				long = append(long, r)
				covered += r.Len()
			default:
				short = append(short, r)
			}
		}
		if long != nil {
			a.note(LevelWarn, "%s%d stretch(es) of %s or longer leave the usual %s by more than %.0f sigma, %.1f%% of the points: %s. The trend, the periods and the forecast are distorted by them. If they are incidents (an outage, a catch-up), analyse a history without them; if the load is bursty by nature, SSA describes only its repeating part (find the stretches with anomaly_ensemble).",
				a.on(c), len(long), a.length(a.LongLen()), a.profile, spikeSigmas, 100*float64(covered)/float64(n), a.runList(long))
		}
		for _, rec := range a.Recurring {
			if rec.Channel == c {
				a.noteRecurrence(rec)
			}
		}
		if len(short) >= alarm {
			a.note(LevelInfo, "%s%d short burst(s) (under %s) leave the usual %s by more than %.0f sigma, Gaussian noise would give ~%.1f; the largest: %s. SSA leaves them in the residual; detect them with anomaly_ensemble if they matter.",
				a.on(c), len(short), a.length(a.LongLen()), a.profile, spikeSigmas, expect, a.runList(short))
		}
	}
}

func (a *Analysis) noteRecurrence(rec Recurrence) {
	var lens, devs []float64
	for _, i := range rec.Runs {
		lens = append(lens, float64(a.Runs[i].Len()))
		devs = append(devs, a.Runs[i].Deviation)
	}
	at := fmt.Sprintf("%02d:%02d UTC", int(rec.At.Hours()), int(rec.At.Minutes())%60)
	if a.start.IsZero() {
		at = formatDuration(rec.At.Round(time.Minute)) + " into each day counted from the first point"
	}
	a.note(LevelInfo, "%s%d stretch(es) off the usual profile recur daily at about %s (+-%s) on %d of %d days, each about %s, %s: a scheduled job. SSA leaves them in the residual; detect them with anomaly_ensemble if they matter.",
		a.on(rec.Channel), len(rec.Runs), at, formatDuration(rec.Spread.Round(time.Minute)), rec.Days, rec.Of,
		a.length(int(math.Round(median(lens)))), side(median(devs)))
}

// runList describes up to showRuns stretches, the largest first (points ×
// |deviation|).
func (a *Analysis) runList(runs []Run) string {
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(x, y Run) int {
		return cmp.Compare(float64(y.Len())*math.Abs(y.Deviation), float64(x.Len())*math.Abs(x.Deviation))
	})
	parts := make([]string, 0, showRuns)
	for _, r := range runs[:min(len(runs), showRuns)] {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", a.span(r), a.length(r.Len()), side(r.Deviation)))
	}
	return strings.Join(parts, "; ")
}

// span writes the first and last point of a stretch: UTC times when the start
// of the series is known, point numbers otherwise.
func (a *Analysis) span(r Run) string {
	if a.start.IsZero() || a.step <= 0 {
		return fmt.Sprintf("points %d-%d", r.From, r.To)
	}
	from := a.start.Add(time.Duration(r.From) * a.step).UTC()
	to := a.start.Add(time.Duration(r.To) * a.step).UTC()
	switch {
	case r.From == r.To:
		return from.Format("2006-01-02 15:04 UTC")
	case from.YearDay() == to.YearDay() && from.Year() == to.Year():
		return from.Format("2006-01-02 15:04") + "-" + to.Format("15:04 UTC")
	default:
		return from.Format("2006-01-02 15:04") + " - " + to.Format("01-02 15:04 UTC")
	}
}

// length writes a number of points as a duration, or as points without a
// time step.
func (a *Analysis) length(p int) string {
	if a.step <= 0 {
		return fmt.Sprintf("%d points", p)
	}
	return formatDuration(time.Duration(p) * a.step)
}

func side(dev float64) string {
	if dev < 0 {
		return fmt.Sprintf("below it by %.3g", -dev)
	}
	return fmt.Sprintf("above it by %.3g", dev)
}

// calendar returns the calendar cycle (a day or a week) within tol (a
// share) of period (in points), in points, and its name; ok is false
// without a time step or when period is far from both.
func (a *Analysis) calendar(period, tol float64) (points float64, name string, ok bool) {
	if a.step <= 0 {
		return 0, "", false
	}
	for _, cal := range []struct {
		d    time.Duration
		name string
	}{{24 * time.Hour, "daily"}, {7 * 24 * time.Hour, "weekly"}} {
		pts := float64(cal.d) / float64(a.step)
		if math.Abs(period/pts-1) <= tol {
			return pts, cal.name, true
		}
	}
	return 0, "", false
}
