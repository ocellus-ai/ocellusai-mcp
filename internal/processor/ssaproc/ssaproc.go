// Package ssaproc holds two analysis processors built on Singular Spectrum
// Analysis (package ssa): `ssa` decomposes every series of a Prometheus
// matrix on its own, `ssa_mv` decomposes the dimensions of every
// multivariate series together (MSSA). Both describe the structure of a
// series — trend, cycles with their periods, noise — suggest how to group
// the components, check the groups and, on request, forecast the structure.
//
// Analysis. ssa takes the scalar form (a Prometheus range result):
//
//	{"resultType": "matrix", "result": [{"metric": {...}, "values": [[ts, "v"], ...]}]}
//
// ssa_mv takes the multivariate form of package series, as produced by the
// join transform:
//
//	{"dims": ["cpu", "mem"], "series": [{"labels": {...}, "points": [[ts, v1, v2], ...], "reason": "..."}]}
//
// Every series is placed on a regular time grid (series.Regular: the step is
// the smallest interval between samples unless with.step is set) and the
// empty slots are filled by linear interpolation, because SSA needs a series
// without gaps; a series with more than max_gaps of its grid empty, shorter
// than min_points, not on a grid, or carrying a reason (an object join could
// not build) is reported as skipped. Series are analysed in parallel.
//
// The window L (with.window, points or a duration such as 1d; default n/3)
// should be a multiple of the main period: a day for VM metrics, on two
// weeks of history or more. ssa accepts L up to n−1 and replaces a window
// longer than n/2 by n−L+1 (the same decomposition); ssa_mv accepts up to
// (n+1)/2. with.k = 0 decomposes into all L components while the shorter
// side of the trajectory matrix is at most 300 points and into the 30
// leading ones by the randomized method above that; k > 0 asks for that
// many (all of them when k is close to L), iters is the number of power
// iterations of the randomized method.
//
// ssa_mv brings the dimensions to one scale before decomposing (normalize,
// default true): each is divided by its standard deviation, floored by
// min_delta (a number or {dim: number}, in the dim's units) and by
// min_rel_delta·|mean|, so a nearly constant dimension (steal at 0.001%) is
// not blown up to the size of the others (min_delta also floors the runs,
// see below). Scaling is a channel weight
// 1/scale² of the MSSA, and every reconstruction and forecast comes back in
// the original units. normalize: false keeps the raw scale, for dims in the
// same units whose absolute size should matter.
//
// Groups: with.groups ("0;1-2;3,5-7") names groups of components; without
// it the suggested ones are checked (the trend, one group per harmonic
// family, the rest one by one). with.forecast (points or a duration)
// continues the union of the given groups, or, when no groups are given, the
// structure (σ at least 1.5 times the noise floor) that has a stable
// continuation, by the L (recurrent, default) or K (vector) method: left_out
// lists the oscillations without a partner, the alternating components and,
// in growing, the harmonic pairs whose own roots grow more than twice over
// the horizon (growth_per_step of every component is its largest ESPRIT root
// modulus). with.clip ([lo, hi], "lo,hi",
// or per dim in ssa_mv) clips the groups and the forecast that contain
// component 0, the level: bounded metrics in their own scale.
//
// Output:
//
//	{[dims], series_total, series_analyzed, series_skipped,
//	 series: [{labels, points, grid_points, gaps, step, skipped, [reason],
//	           window, K, decomposition, [scale],
//	           spectrum: [{component, sigma, share, [err]}],
//	           noise: {floor, removed, negligible, sd, phi, [saturation]},
//	           components: [{group, components, kind, meaning, sigma, ratio, borderline,
//	                         [period, period_seconds, order, base_period, pair_gap], variance_share, growth_per_step}],
//	           noise_from, structure,
//	           groups: [{group, components, source, [label], kind, [periods, periods_seconds],
//	                     mean, drift, peak_to_peak, min, max, variance_share, [clipped], [points]}],
//	           [fine_groups: [{group, components, label}]],
//	           groups_check: {separation, separation_between, residual_wcorr, residual_group, left_out},
//	           residual: {explained, sd, robust_sd, lag1, spikes},
//	           broken, runs_total, runs: [{[dim], kind, start, start_time, end, end_time, points, duration, deviation}],
//	           recurring: [{[dim], time_of_day, seconds_of_day, spread, days, of_days, runs, duration, deviation}],
//	           findings: [{level, text}],
//	           [forecast: {method, [left_out], [growing: [{group, components, period, period_seconds, variance_share,
//	                                                       growth_per_step, growth}]],
//	                       components, steps, start, start_time, end, end_time, clipped, points} | {reason}]}]}
//
// Per-channel values (noise sd and phi, variance shares, group statistics,
// the residual, scale) are scalars in ssa and objects {dim: value} in
// ssa_mv. sigma, ratio, the spectrum and the noise floor are in the units of
// the (weighted) trajectory matrix; everything else is in the units of the
// series. points of a group reconstruction (with.reconstruction: true) and
// of the forecast are [[ts, v]] in ssa and [[ts, v1, v2, …]] in ssa_mv, like
// the input forms. findings are the statements of the analysis and of the
// group check, each with the value, the threshold and what it means; level
// is ok, info or warn.
//
// runs are the stretches where a dim leaves its usual profile (a robust
// level plus the median of the same time of day over the days, robust to the
// stretches themselves; the level alone when the history is shorter than
// three days): farther than 4 robust sigma and than the smallest meaningful
// change (min_delta, min_rel_delta·|median|; a number in ssa, also {dim:
// number} in ssa_mv, where it also floors the normalization scale) outside
// the range the profile covers within an hour of the point, so a day that
// runs the profile an hour late is not off. kind is long (L/12 or more, 2 h
// at a daily window: another regime such as an outage and its catch-up),
// daily (a group on one side of the profile that recurs at one time of day
// on at least three days, listed in recurring: a scheduled job) or burst;
// deviation is the mean distance from the profile in the units of the
// series, negative below it. At most maxRuns of each kind are listed (the
// largest), chronologically; runs_total counts all. broken reports long
// stretches that do not recur daily: the trend, the periods and the forecast
// are distorted by them, the forecast carries a warning, and the findings
// do not advise fitting the window to the distorted period.
package ssaproc

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gonum.org/v1/gonum/stat"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
	"github.com/ocellus-ai/ocellusai-mcp/internal/ssa"
	"github.com/ocellus-ai/ocellusai-mcp/internal/tsanomaly"
)

// Processor names referenced from YAML.
const (
	NameUni   = "ssa"
	NameMulti = "ssa_mv"
)

// Processor implements processor.Processor for ssa and ssa_mv.
type Processor struct{ multi bool }

var _ processor.Processor = (*Processor)(nil)

// NewUni creates the ssa processor (one series at a time).
func NewUni() *Processor { return &Processor{} }

// NewMulti creates the ssa_mv processor (the dims of a series together).
func NewMulti() *Processor { return &Processor{multi: true} }

// Name returns "ssa" or "ssa_mv".
func (p *Processor) Name() string {
	if p.multi {
		return NameMulti
	}
	return NameUni
}

// Validate checks the raw `with` section at catalog load time. Values that
// are still templates are checked after rendering, in Run.
func (p *Processor) Validate(with map[string]any) error {
	_, err := parseSettings(with, true, p.multi)
	return err
}

// input is one series to analyse.
type input struct {
	labels map[string]string
	points []series.MultiPoint
	reason string
}

// Run analyses every series and returns the report described in the
// package documentation.
func (p *Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false, p.multi)
	if err != nil {
		return nil, err
	}
	var inputs []input
	var dims []string
	if p.multi {
		m, err := series.ParseMulti(data)
		if err != nil {
			return nil, err
		}
		if err := st.checkDims(m.Dims); err != nil {
			return nil, err
		}
		dims = m.Dims
		for _, s := range m.Series {
			inputs = append(inputs, input{labels: s.Labels, points: s.Points, reason: s.Reason})
		}
	} else {
		all, err := series.ParseMatrix(data)
		if err != nil {
			return nil, err
		}
		dims = []string{"value"}
		for _, s := range all {
			inputs = append(inputs, input{labels: s.Labels, points: series.ScalarPoints(s.Points)})
		}
	}

	entries := make([]map[string]any, len(inputs))
	if err := parallel(ctx, len(inputs), func(i int) {
		j := &job{st: st, multi: p.multi, dims: dims, in: inputs[i]}
		entries[i] = j.run()
	}); err != nil {
		return nil, err
	}

	list := make([]any, len(entries))
	analyzed, skipped := 0, 0
	for i, e := range entries {
		list[i] = e
		if e["skipped"] == true {
			skipped++
		} else {
			analyzed++
		}
	}
	out := map[string]any{
		"series_total":    len(list),
		"series_analyzed": analyzed,
		"series_skipped":  skipped,
		"series":          list,
	}
	if p.multi {
		out["dims"] = toAnyList(dims)
	}
	return out, nil
}

// parallel calls fn(i) for i < n on up to GOMAXPROCS goroutines and returns
// as soon as ctx is done; the analysis does not observe ctx, so a running
// call finishes in the background and its result is dropped, and no new one
// starts.
func parallel(ctx context.Context, n int, fn func(i int)) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var next atomic.Int64
		var wg sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), n) {
			wg.Go(func() {
				for i := int(next.Add(1)) - 1; i < n && ctx.Err() == nil; i = int(next.Add(1)) - 1 {
					fn(i)
				}
			})
		}
		wg.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}

// job analyses one series.
type job struct {
	st    *settings
	multi bool
	dims  []string
	in    input
	entry map[string]any
	grid  *series.Grid
	cols  [][]float64 // the gridded series, gaps interpolated
}

// skip marks the entry skipped with a reason and returns it.
func (j *job) skip(reason string) map[string]any {
	j.entry["skipped"] = true
	j.entry["reason"] = reason
	return j.entry
}

// perDim writes one value per dim: a scalar in ssa, {dim: value} in ssa_mv.
func (j *job) perDim(vals []float64) any {
	if !j.multi {
		return finiteOrNil(vals[0])
	}
	out := make(map[string]any, len(j.dims))
	for d, dim := range j.dims {
		out[dim] = finiteOrNil(vals[d])
	}
	return out
}

func (j *job) run() map[string]any {
	st := j.st
	j.entry = map[string]any{
		"labels":  series.LabelsToAny(j.in.labels),
		"points":  len(j.in.points),
		"skipped": false,
	}
	if j.in.reason != "" {
		return j.skip(j.in.reason)
	}
	if len(j.in.points) < st.minPoints {
		return j.skip(fmt.Sprintf("fewer than %d points (min_points)", st.minPoints))
	}
	g, reason := series.Regular(j.in.points, len(j.dims), st.step)
	if reason != "" {
		return j.skip(reason)
	}
	j.grid = g
	n := g.Len()
	j.entry["grid_points"] = n
	j.entry["gaps"] = g.Gaps
	j.entry["step"] = g.Step
	if share := float64(g.Gaps) / float64(n); share > st.maxGaps {
		return j.skip(fmt.Sprintf("%d of %d grid slots have no sample (%.0f%%), more than max_gaps %.0f%%: interpolating them would invent the series", g.Gaps, n, 100*share, 100*st.maxGaps))
	}
	cols := make([][]float64, len(g.Cols))
	for d, col := range g.Cols {
		cols[d] = tsanomaly.Impute(col)
	}
	j.cols = cols

	L := st.window.Resolve(g.Step)
	if L == 0 {
		L = n / 3
	}
	maxL := n - 1
	if j.multi {
		maxL = (n + 1) / 2
	}
	switch {
	case L < 2:
		return j.skip(fmt.Sprintf("the window is %d point(s) at a step of %gs; SSA needs at least 2", L, g.Step))
	case L > maxL:
		return j.skip(fmt.Sprintf("the window of %d points exceeds %d, the longest a series of %d points allows (shorten with.window or query more history)", L, maxL, n))
	}
	k := st.k
	if k == 0 && min(L, n-L+1) > autoFullMax {
		k = autoK
	}
	if k > 0 {
		for _, grp := range st.groups {
			k = max(k, slices.Max(grp)+1)
		}
	}
	step := time.Duration(g.Step * float64(time.Second))
	sec, frac := math.Modf(g.Times[0])
	start := time.Unix(int64(sec), int64(frac*1e9)).UTC()

	var d decomposition
	if j.multi {
		var weights []float64
		if st.normalize {
			scale := j.scales(cols)
			j.entry["scale"] = j.perDim(scale)
			weights = make([]float64, len(scale))
			for i, s := range scale {
				weights[i] = 1 / (s * s)
			}
		}
		m, err := ssa.DecomposeMulti(cols, L, k, st.iters, weights)
		if err != nil {
			return j.skip("decomposition failed: " + err.Error())
		}
		d = multiDecomp{m, m.AnalyzeWith(j.dims, ssa.Options{Step: step, Start: start, MinDelta: j.minDeltas(cols)})}
	} else {
		s, err := ssa.Decompose(cols[0], L, k, st.iters)
		if err != nil {
			return j.skip("decomposition failed: " + err.Error())
		}
		d = uniDecomp{s, s.AnalyzeWith(ssa.Options{Step: step, Start: start, MinDelta: j.minDeltas(cols)})}
	}
	return j.report(d, step)
}

// scales returns the normalization scale of every dim: its standard
// deviation, at least max(min_delta, min_rel_delta·|mean|); the absolute
// mean, or 1, for a constant dim without a floor.
func (j *job) scales(cols [][]float64) []float64 {
	out := make([]float64, len(cols))
	for d, col := range cols {
		mean, sd := stat.PopMeanStdDev(col, nil)
		s := max(sd, j.st.minDelta.Of(j.dims[d]), j.st.minRelDelta*math.Abs(mean))
		if s == 0 {
			s = math.Abs(mean)
		}
		if s == 0 {
			s = 1
		}
		out[d] = s
	}
	return out
}

// minDeltas returns the smallest meaningful change of every dim:
// max(min_delta, min_rel_delta·|median|), nil when neither is set.
func (j *job) minDeltas(cols [][]float64) []float64 {
	out := make([]float64, len(cols))
	set := false
	for d, col := range cols {
		out[d] = j.st.minDelta.Of(j.dims[d])
		if j.st.minRelDelta > 0 {
			out[d] = max(out[d], j.st.minRelDelta*math.Abs(median(col)))
		}
		set = set || out[d] > 0
	}
	if !set {
		return nil
	}
	return out
}

// decomposition is what the report needs from SSA and MSSA.
type decomposition interface {
	analysis() *ssa.Analysis
	window() (int, int) // L and K
	sigma() []float64
	norm2() float64
	errs() []float64
	forecast(method string, group []int, steps int) ([][]float64, error)
	esprit(group []int) (ssa.Esprit, error)
}

type uniDecomp struct {
	s *ssa.SSA
	a *ssa.Analysis
}

func (u uniDecomp) analysis() *ssa.Analysis { return u.a }
func (u uniDecomp) window() (int, int)      { return u.s.L, u.s.K }
func (u uniDecomp) sigma() []float64        { return u.s.Sigma }
func (u uniDecomp) norm2() float64          { return u.s.Norm2 }
func (u uniDecomp) errs() []float64         { return u.s.Err }

func (u uniDecomp) esprit(group []int) (ssa.Esprit, error) { return u.s.Esprit(group) }

func (u uniDecomp) forecast(method string, group []int, steps int) ([][]float64, error) {
	var y []float64
	var err error
	if method == MethodK {
		y, err = u.s.KForecast(group, steps)
	} else {
		y, err = u.s.LForecast(group, steps)
	}
	return [][]float64{y}, err
}

type multiDecomp struct {
	m *ssa.MSSA
	a *ssa.Analysis
}

func (m multiDecomp) analysis() *ssa.Analysis { return m.a }
func (m multiDecomp) window() (int, int)      { return m.m.L, m.m.K }
func (m multiDecomp) sigma() []float64        { return m.m.Sigma }
func (m multiDecomp) norm2() float64          { return m.m.Norm2 }
func (m multiDecomp) errs() []float64         { return m.m.Err }

func (m multiDecomp) esprit(group []int) (ssa.Esprit, error) { return m.m.Esprit(group) }

func (m multiDecomp) forecast(method string, group []int, steps int) ([][]float64, error) {
	if method == MethodK {
		return m.m.KForecast(group, steps)
	}
	return m.m.LForecast(group, steps)
}

func finiteOrNil(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

func toAnyList[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
