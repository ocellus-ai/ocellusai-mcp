// Package ensemble is the `anomaly_ensemble` processor: batch anomaly
// detection over multivariate series with the detector ensemble of package
// tsanomaly. Where anomaly_mv scores every sample against one Gaussian
// model, this analysis looks for anomalous segments with complementary
// detectors: shape discords (Matrix Profile), residual outliers
// (autoregression and a seasonal baseline), levels reached slowly (a
// trailing-median residual, visible for up to window/2 points after the
// shift), rare states (Isolation Forest), broken correlations and
// proportional growth (PCA) and multidimensional shape anomalies
// (k-dimensional Matrix Profile). Every detector's scores
// are brought to a robust-z scale, thresholds come from extreme-value theory
// (peaks over threshold with a per-point risk) unless a fixed z is given,
// flagged points are merged into ranges, and every multivariate range is
// attributed to the metrics that caused it.
//
// Analysis: the input is the multivariate series form of package series,
//
//	{"dims": ["cpu", "mem"], "series": [{"labels": {...}, "points": [[ts, v1, v2], ...], "reason": "..."}]}
//
// as produced by the join transform or a preceding fn: jq step. A single
// dim is allowed: then only the per-metric ensemble runs. Every series is
// placed on a regular time grid (the step is inferred from the smallest
// interval between samples unless with.step is set); empty slots are gaps
// that the detectors impute and mask, so a hole never becomes an anomaly.
// A series carrying a reason (an object join could not build), a series
// shorter than min_points and a series whose samples do not fit a grid are
// reported as skipped.
//
// Every detector measures deviations in units of the series' own spread,
// which collapses on a nearly constant or nearly idle dimension (steal at
// 0.001%, memory of a steady database): a negligible blip then gets a robust
// z of hundreds, and Isolation Forest and the Matrix Profile, which have no
// scale to floor, isolate it just as eagerly. min_delta (a number for every
// dim or an object {dim: number}, in the metric's units) and min_rel_delta
// (a fraction of each dim's median) name the smallest change worth
// reporting, unit = max(min_delta, min_rel_delta*|median|); Gaussian noise
// of a quarter of it (processor.NoiseFloorShare) is added to the detector
// input, so every detector sees at least that spread and a change of about
// one unit is the smallest it can flag. The noise is seeded by the series
// labels and the dim name (the same data always gives the same report) and
// never reaches the report: values, baseline and quarters are the raw
// samples. On a production fleet (2026-09-26, 114 VMs x 10 signals x 8 days) it
// cut the multivariate ranges that moved less than one unit from 3678 to 33
// and kept the recall of short strong moves (82% vs 84%).
//
// Output:
//
//	{detectors, dims, threshold: {mode: pot, risk, z_floor} | {mode: fixed, z},
//	 series_total, series_analyzed, series_skipped, total_anomalies,
//	 series: [{labels, points, grid_points, gaps, step, baseline, quarters, [noise_floor],
//	           window, season, min_len, merge_gap, skipped, [reason],
//	           anomaly_count, anomalies: [range], detectors, thresholds, max_score, [error],
//	           metrics: {dim: {anomaly_count, anomalies: [range], detectors, thresholds, max_score, [error]}}}]}
//
// noise_floor (present when min_delta or min_rel_delta is set) is the
// standard deviation of the noise added to each dim, 0 for a dim without one.
// baseline is the per-dim median of the whole series and quarters the
// per-dim medians of its four consecutive quarters (null where a quarter has
// no sample), so a level shift is visible without a second call. window,
// season, min_len and merge_gap are the lengths actually used, in grid
// points: in `with` each of them is either a number of points or a
// Prometheus duration such as 5m resolved by the step of the series.
//
// A range is {start, start_time, end, end_time, peak, peak_time, points,
// duration, score, fired_by, values | value, baseline}: start/end are the
// first and last grid timestamps of the segment, peak the moment located by
// the point-wise detectors, score the largest combined robust-z inside the
// segment, fired_by the detectors whose z exceeded their threshold inside
// it, values (per dim; value for a single metric) the raw samples at the
// peak and baseline the per-dim median of the whole series. A multivariate
// range adds kind (single: exactly one metric is anomalous on its own, or the
// leading metric's evidence is twice the runner-up's; correlation: no metric
// is anomalous alone but their relation broke; systemic: several metrics are
// anomalous and none dominates) and top_metrics: [{metric, score, own_z, pca_share,
// mp_share}], the metrics ranked by their evidence. The headline anomalies of a series
// are the multivariate ranges when there are at least two dims and the sole
// metric's ranges otherwise; the per-metric results are always under
// metrics. detectors and thresholds name the detectors that actually scored
// a series (one that could not, for example a Matrix Profile whose window
// exceeds half the series, is left out).
package ensemble

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
	"github.com/ocellus-ai/ocellusai-mcp/internal/tsanomaly"
)

// Name is the processor name referenced from YAML (fn: anomaly_ensemble).
const Name = "anomaly_ensemble"

// Defaults of the `with` settings.
const (
	DefaultMinPoints  = 20
	DefaultMaxRanges  = 20
	DefaultTopMetrics = 5
	DefaultRisk       = 1e-4
	DefaultZFloor     = 4.0
	DefaultKNN        = 2
	DefaultKMPDims    = -1
	DefaultPCATrain   = 1.0
	// minWindow is the smallest Matrix Profile window tsanomaly accepts.
	minWindow = 4
)

// allowedKeys are the `with` keys, all optional.
var allowedKeys = []string{
	"window", "season", "step", "detectors",
	"risk", "z_threshold", "z_floor", "min_len", "merge_gap",
	"ar_lags", "kmp_dims", "knn", "pca_train_fraction",
	"min_points", "max_ranges", "top_metrics", "min_delta", "min_rel_delta",
}

// detectorNames are the values accepted in with.detectors, in report order.
var detectorNames = []string{"mp", "ar", "level", "seasonal", "iforest", "pca", "kmp"}

// Processor implements processor.Processor.
type Processor struct{}

// New creates the ensemble anomaly processor.
func New() *Processor { return &Processor{} }

// Name returns "anomaly_ensemble".
func (*Processor) Name() string { return Name }

type settings struct {
	window, season processor.Span
	step           float64         // seconds; 0 = infer per series
	detectors      map[string]bool // nil = all
	risk           float64
	zThreshold     float64
	zFloor         float64
	minLen         processor.Span // default 1 point
	mergeGap       processor.Span // unset = window/16
	arLags         []int
	kmpDims        int
	knn            int
	pcaTrain       float64
	minPoints      int
	maxRanges      int
	topMetrics     int
	minDelta       processor.DimFloats
	minRelDelta    float64
}

// Validate checks the raw `with` section at catalog load time. Values that
// are still templates are checked after rendering, in Run.
func (*Processor) Validate(with map[string]any) error {
	_, err := parseSettings(with, true)
	return err
}

// parseSettings reads every key. At load time a value that is still a
// template is skipped; at run time everything must be a final value.
func parseSettings(with map[string]any, load bool) (*settings, error) {
	if err := processor.CheckKeys(with, allowedKeys...); err != nil {
		return nil, err
	}
	st := &settings{
		risk: DefaultRisk, zFloor: DefaultZFloor, minLen: processor.Span{Points: 1},
		kmpDims: DefaultKMPDims, knn: DefaultKNN, pcaTrain: DefaultPCATrain,
		minPoints: DefaultMinPoints, maxRanges: DefaultMaxRanges, topMetrics: DefaultTopMetrics,
	}
	// deferred reports a key whose value is only known after rendering.
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }
	var err error
	if !deferred("window") {
		if st.window, err = processor.SpanOf(with, "window"); err != nil {
			return nil, err
		}
	}
	if !deferred("season") {
		if st.season, err = processor.SpanOf(with, "season"); err != nil {
			return nil, err
		}
	}
	if !deferred("step") {
		if st.step, err = secondsOf(with, "step"); err != nil {
			return nil, err
		}
	}
	if st.detectors, err = detectorSet(with, load); err != nil {
		return nil, err
	}
	if !deferred("risk") {
		if st.risk, err = processor.Float(with, "risk", DefaultRisk); err != nil {
			return nil, err
		}
		if st.risk <= 0 || st.risk >= 1 {
			return nil, fmt.Errorf("with.risk: must be in (0, 1), got %v", st.risk)
		}
	}
	if !deferred("z_threshold") {
		if st.zThreshold, err = processor.Float(with, "z_threshold", 0); err != nil {
			return nil, err
		}
		if st.zThreshold < 0 {
			return nil, fmt.Errorf("with.z_threshold: must be >= 0 (0 = thresholds from POT), got %v", st.zThreshold)
		}
	}
	if !deferred("z_floor") {
		if st.zFloor, err = processor.Float(with, "z_floor", DefaultZFloor); err != nil {
			return nil, err
		}
		if st.zFloor < 0 {
			return nil, fmt.Errorf("with.z_floor: must be >= 0 (0 = no floor), got %v", st.zFloor)
		}
	}
	if !deferred("min_len") {
		// spanOf treats 0 as unset, so a given zero is rejected here.
		if v, ok := with["min_len"]; ok && v != nil {
			if st.minLen, err = processor.SpanOf(with, "min_len"); err != nil {
				return nil, err
			}
			if !st.minLen.IsSet() {
				return nil, fmt.Errorf("with.min_len: must be >= 1 point or a duration such as 5m, got %v", v)
			}
		}
	}
	if !deferred("merge_gap") {
		if st.mergeGap, err = processor.SpanOf(with, "merge_gap"); err != nil {
			return nil, err
		}
	}
	if st.arLags, err = lagList(with, load); err != nil {
		return nil, err
	}
	if !deferred("kmp_dims") {
		if st.kmpDims, err = processor.Int(with, "kmp_dims", DefaultKMPDims); err != nil {
			return nil, err
		}
		if st.kmpDims < -1 {
			return nil, fmt.Errorf("with.kmp_dims: must be -1 (worst dim), 0 (all dims) or the number of best dims to average, got %d", st.kmpDims)
		}
	}
	if !deferred("knn") {
		if st.knn, err = processor.Int(with, "knn", DefaultKNN); err != nil {
			return nil, err
		}
		if st.knn < 1 {
			return nil, fmt.Errorf("with.knn: must be >= 1, got %d", st.knn)
		}
	}
	if !deferred("pca_train_fraction") {
		if st.pcaTrain, err = processor.Float(with, "pca_train_fraction", DefaultPCATrain); err != nil {
			return nil, err
		}
		if st.pcaTrain <= 0 || st.pcaTrain > 1 {
			return nil, fmt.Errorf("with.pca_train_fraction: must be in (0, 1], got %v", st.pcaTrain)
		}
	}
	if !deferred("min_points") {
		if st.minPoints, err = processor.Int(with, "min_points", DefaultMinPoints); err != nil {
			return nil, err
		}
		if st.minPoints < 1 {
			return nil, fmt.Errorf("with.min_points: must be >= 1, got %d", st.minPoints)
		}
	}
	if !deferred("max_ranges") {
		if st.maxRanges, err = processor.Int(with, "max_ranges", DefaultMaxRanges); err != nil {
			return nil, err
		}
		if st.maxRanges < 0 {
			return nil, fmt.Errorf("with.max_ranges: must be >= 0 (0 = unlimited), got %d", st.maxRanges)
		}
	}
	if st.minDelta, err = processor.ParseDimFloats(with, "min_delta", load); err != nil {
		return nil, err
	}
	if !deferred("min_rel_delta") {
		if st.minRelDelta, err = processor.Float(with, "min_rel_delta", 0); err != nil {
			return nil, err
		}
		if st.minRelDelta < 0 {
			return nil, fmt.Errorf("with.min_rel_delta: must be >= 0 (0 = off), got %v", st.minRelDelta)
		}
	}
	if !deferred("top_metrics") {
		if st.topMetrics, err = processor.Int(with, "top_metrics", DefaultTopMetrics); err != nil {
			return nil, err
		}
		if st.topMetrics < 1 {
			return nil, fmt.Errorf("with.top_metrics: must be >= 1, got %d", st.topMetrics)
		}
	}
	return st, nil
}

// secondsOf reads an optional positive duration in seconds: a number, a
// numeric string or a Prometheus duration such as 30s.
func secondsOf(with map[string]any, key string) (float64, error) {
	sec, err := processor.Seconds(with, key, 0)
	if err != nil {
		return 0, err
	}
	if v, ok := with[key]; ok && v != nil && sec <= 0 {
		return 0, fmt.Errorf("with.%s: must be > 0 seconds, got %v", key, sec)
	}
	return sec, nil
}

// listItems reads an optional list value: a bare string is split on commas
// (so one rendered template can carry a list), a list is taken as is.
func listItems(with map[string]any, key string) ([]any, bool, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	switch x := v.(type) {
	case string:
		var items []any
		for _, part := range strings.Split(x, ",") {
			items = append(items, strings.TrimSpace(part))
		}
		return items, true, nil
	case []any:
		return x, true, nil
	case []string:
		items := make([]any, 0, len(x))
		for _, s := range x {
			items = append(items, s)
		}
		return items, true, nil
	default:
		return nil, false, fmt.Errorf("with.%s: must be a list, got %s", key, processor.JSONType(v))
	}
}

// detectorSet reads with.detectors: nil means every detector. At load time a
// list with a templated element is deferred to Run.
func detectorSet(with map[string]any, load bool) (map[string]bool, error) {
	items, present, err := listItems(with, "detectors")
	if err != nil || !present {
		return nil, err
	}
	set := make(map[string]bool, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("with.detectors[%d]: must be a string, got %s", i, processor.JSONType(it))
		}
		if processor.IsTemplate(s) {
			if load {
				return nil, nil
			}
			return nil, fmt.Errorf("with.detectors[%d]: unrendered template %q", i, s)
		}
		s = strings.TrimSpace(s)
		if !knownDetector(s) {
			return nil, fmt.Errorf("with.detectors[%d]: unknown detector %q (available: %s)", i, s, strings.Join(detectorNames, ", "))
		}
		if set[s] {
			return nil, fmt.Errorf("with.detectors: duplicate %q", s)
		}
		set[s] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("with.detectors: must not be empty (available: %s)", strings.Join(detectorNames, ", "))
	}
	return set, nil
}

func knownDetector(name string) bool {
	for _, d := range detectorNames {
		if d == name {
			return true
		}
	}
	return false
}

// lagList reads with.ar_lags: positive distinct integers; nil keeps the
// tsanomaly default {1, 2, 3}.
func lagList(with map[string]any, load bool) ([]int, error) {
	items, present, err := listItems(with, "ar_lags")
	if err != nil || !present {
		return nil, err
	}
	lags := make([]int, 0, len(items))
	seen := make(map[int]bool, len(items))
	for i, it := range items {
		if s, ok := it.(string); ok && processor.IsTemplate(s) {
			if load {
				return nil, nil
			}
			return nil, fmt.Errorf("with.ar_lags[%d]: unrendered template %q", i, s)
		}
		f, err := processor.Number(it)
		if err != nil {
			return nil, fmt.Errorf("with.ar_lags[%d]: %w", i, err)
		}
		if f != math.Trunc(f) || f < 1 {
			return nil, fmt.Errorf("with.ar_lags[%d]: must be a positive integer, got %v", i, f)
		}
		l := int(f)
		if seen[l] {
			return nil, fmt.Errorf("with.ar_lags: duplicate %d", l)
		}
		seen[l] = true
		lags = append(lags, l)
	}
	if len(lags) == 0 {
		return nil, fmt.Errorf("with.ar_lags: must not be empty (for example [1, 2, 3])")
	}
	return lags, nil
}

func (st *settings) enabled(name string) bool { return st.detectors == nil || st.detectors[name] }

// detectorList names the configured detectors in report order; seasonal
// only counts when a season is set.
func (st *settings) detectorList() []string {
	out := make([]string, 0, len(detectorNames))
	for _, d := range detectorNames {
		if !st.enabled(d) || (d == "seasonal" && !st.season.IsSet()) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// thresholdInfo describes the thresholding mode for the report.
func (st *settings) thresholdInfo() map[string]any {
	if st.zThreshold > 0 {
		return map[string]any{"mode": "fixed", "z": st.zThreshold}
	}
	return map[string]any{"mode": "pot", "risk": st.risk, "z_floor": st.zFloor}
}

// floor maps with.z_floor to tsanomaly's convention, where 0 means "the
// default" and a negative value disables the floor.
func (st *settings) floor() float64 {
	if st.zFloor == 0 {
		return -1
	}
	return st.zFloor
}

// options builds the tsanomaly options for a series of n grid points.
func (st *settings) options(n int, step float64) (tsanomaly.Options, error) {
	o := tsanomaly.Options{
		Window:            st.window.Resolve(step),
		Season:            st.season.Resolve(step),
		Risk:              st.risk,
		ZFloor:            st.floor(),
		ZThreshold:        st.zThreshold,
		MinLen:            max(1, st.minLen.Resolve(step)),
		MergeGap:          st.mergeGap.Resolve(step),
		SkipMatrixProfile: !st.enabled("mp"),
		SkipAR:            !st.enabled("ar"),
		SkipLevel:         !st.enabled("level"),
		SkipSeasonal:      !st.enabled("seasonal"),
		SkipPCA:           !st.enabled("pca"),
		SkipIForest:       !st.enabled("iforest"),
		SkipKMP:           !st.enabled("kmp"),
		ARLags:            st.arLags,
		KMPDims:           st.kmpDims,
		KMPDimsSet:        true,
		KNN:               st.knn,
		PCATrainFr:        st.pcaTrain,
	}
	if o.Window == 0 {
		o.Window = max(8, n/50) // the same rule tsanomaly.Analyze applies
	}
	if o.Window < minWindow {
		return o, fmt.Errorf("with.window: must be at least %d points, got %d (at a step of %gs)", minWindow, o.Window, step)
	}
	if o.MergeGap == 0 {
		o.MergeGap = max(1, o.Window/16) // the same rule tsanomaly.Analyze applies
	}
	return o, nil
}

// grid is one series placed on a regular time grid.
type grid struct {
	frame    *tsanomaly.Frame
	times    []float64 // grid timestamps, unix seconds
	step     float64
	gaps     int         // grid slots without a sample
	medians  []float64   // per-dim median of the samples, the baseline of the series and of every range
	quarters [][]float64 // per-dim median of each quarter of the grid (NaN when a quarter has no sample)
}

// valuesAt returns every dimension's raw sample at grid index i (null for a gap).
func (g *grid) valuesAt(i int) map[string]any {
	out := make(map[string]any, len(g.frame.Names))
	for d, name := range g.frame.Names {
		out[name] = finiteOrNil(g.frame.Cols[d][i])
	}
	return out
}

// baseline returns every dimension's median.
func (g *grid) baseline() map[string]any {
	out := make(map[string]any, len(g.frame.Names))
	for d, name := range g.frame.Names {
		out[name] = finiteOrNil(g.medians[d])
	}
	return out
}

// quartersAny returns every dimension's median in each quarter of the grid
// (null where a quarter has no sample), so a level shift is visible without
// a second call.
func (g *grid) quartersAny() map[string]any {
	out := make(map[string]any, len(g.frame.Names))
	for d, name := range g.frame.Names {
		qs := make([]any, len(g.quarters[d]))
		for q, v := range g.quarters[d] {
			qs[q] = finiteOrNil(v)
		}
		out[name] = qs
	}
	return out
}

// noise returns the standard deviation of the noise to add to each dim of g
// (NoiseFloorShare of max(min_delta, min_rel_delta*|median|)), or nil when no
// dim gets any.
func (st *settings) noise(g *grid) []float64 {
	out := make([]float64, len(g.frame.Names))
	set := false
	for d, name := range g.frame.Names {
		unit := st.minDelta.Of(name)
		if m := g.medians[d]; !math.IsNaN(m) {
			unit = math.Max(unit, st.minRelDelta*math.Abs(m))
		}
		out[d] = processor.NoiseFloorShare * unit
		set = set || out[d] > 0
	}
	if !set {
		return nil
	}
	return out
}

// dithered returns a copy of the grid's frame with Gaussian noise of standard
// deviation sigma[d] added to every sample of dim d; gaps stay gaps. The
// generator is seeded by the labels and the dim name, and a value is drawn
// for every slot, so the same series always gets the same noise.
func (g *grid) dithered(sigma []float64, labels map[string]string) *tsanomaly.Frame {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var id strings.Builder
	for _, k := range keys {
		id.WriteString(k + "=" + labels[k] + "\x1f")
	}
	f := &tsanomaly.Frame{Timestamps: g.frame.Timestamps, Names: g.frame.Names, Cols: make([][]float64, len(g.frame.Cols))}
	for d, col := range g.frame.Cols {
		if sigma[d] <= 0 {
			f.Cols[d] = col
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(id.String() + "\x00" + g.frame.Names[d]))
		seed := h.Sum64()
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		out := make([]float64, len(col))
		for i, x := range col {
			out[i] = x + sigma[d]*rng.NormFloat64() // a gap (NaN) stays a gap
		}
		f.Cols[d] = out
	}
	return f
}

// alignSeries places the samples of s on a regular grid. With step 0 the
// step is the smallest interval between consecutive samples. Every sample
// goes to the nearest slot (the last one wins on a collision); slots without
// a sample are NaN. A non-empty reason means the series cannot be gridded.
func alignSeries(s series.MultiSeries, step float64) (*grid, string) {
	r, reason := series.Regular(s.Points, len(s.Dims), step)
	if reason != "" {
		return nil, reason
	}
	n, cols := r.Len(), r.Cols
	g := &grid{step: r.Step, times: r.Times, gaps: r.Gaps}
	ts := make([]int64, n)
	for i, t := range r.Times {
		ts[i] = int64(math.Round(t))
	}
	g.medians = make([]float64, len(cols))
	g.quarters = make([][]float64, len(cols))
	for d, col := range cols {
		finite := make([]float64, 0, len(col))
		var parts [4][]float64
		for i, x := range col {
			if math.IsNaN(x) {
				continue
			}
			finite = append(finite, x)
			q := i * 4 / n
			parts[q] = append(parts[q], x)
		}
		g.medians[d] = tsanomaly.Median(finite)
		g.quarters[d] = make([]float64, len(parts))
		for q, part := range parts {
			g.quarters[d][q] = tsanomaly.Median(part) // NaN for an empty quarter
		}
	}
	g.frame = &tsanomaly.Frame{Timestamps: ts, Names: append([]string(nil), s.Dims...), Cols: cols}
	return g, ""
}

// analyze runs tsanomaly.Analyze and returns as soon as ctx is done; the
// computation itself does not observe ctx, so on cancellation it finishes
// in the background and its result is dropped.
func analyze(ctx context.Context, f *tsanomaly.Frame, o tsanomaly.Options) (*tsanomaly.Report, error) {
	type result struct {
		rep *tsanomaly.Report
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rep, err := tsanomaly.Analyze(f, o)
		ch <- result{rep, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.rep, r.err
	}
}

// Run analyses every series of a multivariate set and returns the report
// described in the package documentation.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	m, err := series.ParseMulti(data)
	if err != nil {
		return nil, err
	}
	if err := st.minDelta.Check("min_delta", m.Dims); err != nil {
		return nil, err
	}

	report := make([]any, 0, len(m.Series))
	analyzed, skipped, total := 0, 0, 0
	skip := func(entry map[string]any, reason string) {
		skipped++
		entry["skipped"] = true
		entry["reason"] = reason
		report = append(report, entry)
	}
	for _, s := range m.Series {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := map[string]any{
			"labels":        series.LabelsToAny(s.Labels),
			"points":        len(s.Points),
			"skipped":       false,
			"anomaly_count": 0,
			"anomalies":     []any{},
		}
		if s.Reason != "" {
			skip(entry, s.Reason)
			continue
		}
		if len(s.Points) < st.minPoints {
			skip(entry, fmt.Sprintf("fewer than %d points (min_points)", st.minPoints))
			continue
		}
		g, reason := alignSeries(s, st.step)
		if reason != "" {
			skip(entry, reason)
			continue
		}
		entry["grid_points"] = g.frame.Len()
		entry["gaps"] = g.gaps
		entry["step"] = g.step
		entry["baseline"] = g.baseline()
		entry["quarters"] = g.quartersAny()
		frame := g.frame
		if sigma := st.noise(g); sigma != nil {
			frame = g.dithered(sigma, s.Labels)
			nf := make(map[string]any, len(sigma))
			for d, name := range g.frame.Names {
				nf[name] = sigma[d]
			}
			entry["noise_floor"] = nf
		}
		opts, err := st.options(g.frame.Len(), g.step)
		if err != nil {
			return nil, err
		}
		entry["window"] = opts.Window
		entry["season"] = opts.Season
		entry["min_len"] = opts.MinLen
		entry["merge_gap"] = opts.MergeGap
		rep, err := analyze(ctx, frame, opts)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			skip(entry, err.Error())
			continue
		}
		metrics := make(map[string]any, len(m.Dims))
		for j, mr := range rep.Metrics {
			metrics[m.Dims[j]] = metricEntry(mr, g, st, j)
		}
		entry["metrics"] = metrics
		var headline map[string]any
		if len(m.Dims) >= 2 {
			headline = multiEntry(rep.Multi, g, st)
		} else {
			headline, _ = metrics[m.Dims[0]].(map[string]any)
		}
		for _, k := range []string{"anomaly_count", "anomalies", "detectors", "thresholds", "max_score", "error"} {
			if v, ok := headline[k]; ok {
				entry[k] = v
			}
		}
		analyzed++
		if n, ok := entry["anomaly_count"].(int); ok {
			total += n
		}
		report = append(report, entry)
	}
	return map[string]any{
		"detectors":       toAnyList(st.detectorList()),
		"dims":            toAnyList(m.Dims),
		"threshold":       st.thresholdInfo(),
		"series_total":    len(report),
		"series_analyzed": analyzed,
		"series_skipped":  skipped,
		"total_anomalies": total,
		"series":          report,
	}, nil
}

// metricEntry reports the univariate ensemble of dimension dim.
func metricEntry(mr tsanomaly.MetricResult, g *grid, st *settings, dim int) map[string]any {
	e := map[string]any{"anomaly_count": 0, "anomalies": []any{}}
	if mr.Err != nil {
		e["error"] = mr.Err.Error()
		return e
	}
	v := st.view(mr.Ensemble)
	v.fill(e)
	e["anomaly_count"] = len(mr.Ranges)
	e["anomalies"] = rangesToAny(capRanges(mr.Ranges, nil, st.maxRanges), g, v, dim, st.topMetrics)
	return e
}

// multiEntry reports the multivariate ensemble with its attribution.
func multiEntry(mr *tsanomaly.MultiResult, g *grid, st *settings) map[string]any {
	e := map[string]any{"anomaly_count": 0, "anomalies": []any{}}
	if mr == nil {
		e["error"] = "multivariate ensemble did not run"
		return e
	}
	if mr.Err != nil {
		e["error"] = mr.Err.Error()
		return e
	}
	v := st.view(mr.Ensemble)
	v.fill(e)
	e["anomaly_count"] = len(mr.Ranges)
	e["anomalies"] = rangesToAny(capRanges(mr.Ranges, mr.Attribution, st.maxRanges), g, v, -1, st.topMetrics)
	return e
}

// ensembleView is one ensemble result together with the per-detector
// thresholds tsanomaly applied to it.
type ensembleView struct {
	ens        *tsanomaly.EnsembleResult
	thresholds map[string]float64
}

// view pairs an ensemble result with its thresholds. With a fixed z the
// combined score (the maximum over detectors) is compared, which is the same
// as every detector having that threshold; otherwise the POT thresholds are
// recomputed exactly as Analyze did (they are deterministic).
func (st *settings) view(ens *tsanomaly.EnsembleResult) ensembleView {
	v := ensembleView{ens: ens, thresholds: make(map[string]float64, len(ens.Z))}
	if st.zThreshold > 0 {
		for _, z := range ens.Z {
			v.thresholds[z.Detector] = st.zThreshold
		}
		return v
	}
	for name, thr := range ens.Thresholds(tsanomaly.ThresholdPolicy{Risk: st.risk, Floor: st.floor()}) {
		v.thresholds[name] = thr
	}
	return v
}

// fill adds the detectors that scored, their thresholds and the largest
// combined score to a report entry.
func (v ensembleView) fill(e map[string]any) {
	names := make([]any, 0, len(v.ens.Parts))
	for _, p := range v.ens.Parts {
		names = append(names, p.Detector)
	}
	e["detectors"] = names
	thr := make(map[string]any, len(v.thresholds))
	for name, t := range v.thresholds {
		thr[name] = finiteOrNil(t)
	}
	e["thresholds"] = thr
	mx := 0.0
	for _, s := range v.ens.Combined {
		mx = math.Max(mx, s)
	}
	e["max_score"] = finiteOrNil(mx)
}

// firedBy names the detectors whose robust z exceeded their threshold
// somewhere inside [start, end], in detector order.
func (v ensembleView) firedBy(start, end int) []any {
	out := make([]any, 0, len(v.ens.Z))
	for _, z := range v.ens.Z {
		t, ok := v.thresholds[z.Detector]
		if !ok {
			continue
		}
		for i := start; i <= end && i < len(z.Scores); i++ {
			if z.Scores[i] > t {
				out = append(out, z.Detector)
				break
			}
		}
	}
	return out
}

// rangeOut is one range with, for multivariate results, its attribution.
type rangeOut struct {
	r    tsanomaly.Range
	attr *tsanomaly.RangeAttribution
}

// capRanges pairs ranges with their attribution and keeps the max strongest
// (0 = unlimited) in time order.
func capRanges(rs []tsanomaly.Range, attrs []tsanomaly.RangeAttribution, max int) []rangeOut {
	out := make([]rangeOut, len(rs))
	for i, r := range rs {
		out[i] = rangeOut{r: r}
		if i < len(attrs) {
			a := attrs[i]
			out[i].attr = &a
		}
	}
	if max > 0 && len(out) > max {
		sort.SliceStable(out, func(i, j int) bool { return out[i].r.PeakScore > out[j].r.PeakScore })
		out = out[:max]
		sort.Slice(out, func(i, j int) bool { return out[i].r.Start < out[j].r.Start })
	}
	return out
}

// rangesToAny writes ranges for the report. dim >= 0 is a univariate range
// of that dimension (value and baseline are numbers); -1 is a multivariate
// range (values and baseline per dim, kind and top_metrics).
func rangesToAny(rs []rangeOut, g *grid, v ensembleView, dim, top int) []any {
	out := make([]any, 0, len(rs))
	for _, ro := range rs {
		r := ro.r
		points := r.End - r.Start + 1
		m := map[string]any{
			"start":      g.times[r.Start],
			"start_time": series.FormatTime(g.times[r.Start]),
			"end":        g.times[r.End],
			"end_time":   series.FormatTime(g.times[r.End]),
			"peak":       g.times[r.Peak],
			"peak_time":  series.FormatTime(g.times[r.Peak]),
			"points":     points,
			"duration":   float64(points) * g.step,
			"score":      finiteOrNil(r.PeakScore),
			"fired_by":   v.firedBy(r.Start, r.End),
		}
		if dim >= 0 {
			m["value"] = finiteOrNil(g.frame.Cols[dim][r.Peak])
			m["baseline"] = finiteOrNil(g.medians[dim])
		} else {
			m["values"] = g.valuesAt(r.Peak)
			m["baseline"] = g.baseline()
		}
		if ro.attr != nil {
			m["kind"] = ro.attr.Kind
			m["top_metrics"] = topMetrics(ro.attr.Metrics, top)
		}
		out = append(out, m)
	}
	return out
}

func topMetrics(ms []tsanomaly.MetricAttribution, n int) []any {
	out := make([]any, 0, min(n, len(ms)))
	for i, m := range ms {
		if i >= n {
			break
		}
		out = append(out, map[string]any{
			"metric":    m.Name,
			"score":     finiteOrNil(m.Score),
			"own_z":     finiteOrNil(m.UniZ),
			"pca_share": finiteOrNil(m.PCAShare),
			"mp_share":  finiteOrNil(m.MPShare),
		})
	}
	return out
}

// finiteOrNil keeps the report JSON-encodable: NaN and ±Inf become null.
func finiteOrNil(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func toAnyList(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
