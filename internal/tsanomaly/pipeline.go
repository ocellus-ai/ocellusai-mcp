package tsanomaly

import (
	"fmt"
	"sync"
)

// Options configures Analyze.
type Options struct {
	Window int // subsequence length in points for Matrix Profile / lag features

	// Season in points (e.g. 86400/step for a daily cycle). Enables
	// SeasonalResidual per metric and a phase feature in IsolationForest.
	// 0 = no seasonality.
	Season int

	// How per-detector scores are merged (default CombineMaxZ).
	Combine Combine
	// Thresholding. Default: per-detector thresholds (see
	// EnsembleResult.Thresholds) — POT/EVT with Risk per point for point-wise
	// detectors, ZFloor for window-spread ones. Set ZThreshold > 0 for a
	// single fixed robust-z threshold on the combined score (6 is
	// reasonable). For CombineRank, Quantile is used.
	Risk       float64 // expected false-positive rate per point for POT (default 1e-4)
	ZFloor     float64 // minimum threshold on the robust-z scale (default 4; <0 disables)
	ZThreshold float64
	Quantile   float64 // for CombineRank (default 0.99)
	MinLen     int     // minimum anomaly length in points (default 1)
	MergeGap   int     // merge anomalies separated by <= this many points (default max(1, Window/16))

	// Detector selection (all enabled by default).
	SkipMatrixProfile bool
	SkipAR            bool
	SkipPCA           bool
	SkipIForest       bool
	SkipKMP           bool // k-dim matrix profile is O(n^2*d); disable for huge frames
	SkipSeasonal      bool // SeasonalResidual is added whenever Season > 1 unless skipped
	SkipLevel         bool // LevelShift (trailing-median residual over Window) per metric

	ARLags     []int   // AR lags; default {1,2,3}
	KMPDims    int     // MultiMatrixProfile K: -1 worst dim (default), 0 all, k best
	KMPDimsSet bool    // set true to pass KMPDims=0 explicitly
	KNN        int     // Matrix Profile neighbours to average (default 2)
	Parallel   int     // per-metric parallelism (default 4)
	PCATrainFr float64 // PCA training fraction (default 1.0)
}

// MetricResult is the outcome for one series.
type MetricResult struct {
	Name     string
	Ensemble *EnsembleResult
	Ranges   []Range
	Err      error
}

// MultiResult is the multivariate outcome.
type MultiResult struct {
	Ensemble     *EnsembleResult
	Ranges       []Range
	Attribution  []RangeAttribution // one per range: which metrics caused it
	Contribution [][]float64        // PCA per-metric share of residual (nil if PCA skipped)
	MPShare      [][]float64        // multi-MP per-metric share (nil if skipped)
	Err          error
}

// Report bundles everything Analyze produced.
type Report struct {
	Frame   *Frame
	Metrics []MetricResult
	Multi   *MultiResult
}

// Analyze runs the univariate ensemble (MatrixProfile + ARResidual +
// IsolationForest) on every column and the multivariate ensemble
// (PCA + IsolationForest + MultiMatrixProfile) on the whole frame.
// With the default CombineMaxZ the combined score is on a sigma-like scale;
// the threshold comes from POT (see POTThreshold) unless ZThreshold is set.
func Analyze(f *Frame, o Options) (*Report, error) {
	if f == nil || len(f.Cols) == 0 {
		return nil, fmt.Errorf("analyze: empty frame")
	}
	n := f.Len()
	if o.Window <= 0 {
		o.Window = maxInt(8, n/50)
	}
	if o.Quantile <= 0 || o.Quantile >= 1 {
		o.Quantile = 0.99
	}
	// rangesOf builds ranges from flags; the peak inside each range is taken
	// from point-wise detectors when any of them fired there, because the
	// window-spread Matrix Profile score is a plateau and its argmax carries
	// no timing information.
	rangesOf := func(ens *EnsembleResult, flags []bool) []Range {
		rs := Ranges(flags, ens.Combined, o.MinLen, o.MergeGap)
		point := ens.PointScores()
		if point == nil {
			return rs
		}
		for k, r := range rs {
			best, bv := -1, 0.0
			for t := r.Start; t <= r.End; t++ {
				if point[t] > bv {
					best, bv = t, point[t]
				}
			}
			if best >= 0 && bv > 0 {
				rs[k].Peak = best
			}
		}
		return rs
	}
	flagsOf := func(ens *EnsembleResult) []bool {
		if o.Combine == CombineRank {
			return Flag(ens.Combined, QuantileThreshold(ens.Combined, o.Quantile))
		}
		if o.ZThreshold > 0 {
			return Flag(ens.Combined, o.ZThreshold)
		}
		f, _ := ens.Flags(ThresholdPolicy{Risk: o.Risk, Floor: o.ZFloor})
		return f
	}
	if o.MinLen <= 0 {
		o.MinLen = 1
	}
	if o.MergeGap <= 0 {
		o.MergeGap = maxInt(1, o.Window/16)
	}
	if o.Risk <= 0 {
		o.Risk = 1e-4
	}

	if o.Parallel <= 0 {
		o.Parallel = 4
	}
	lags := o.ARLags
	if lags == nil {
		lags = []int{1, 2, 3}
	}

	rep := &Report{Frame: f, Metrics: make([]MetricResult, len(f.Cols))}

	// ---- univariate, per metric ----
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.Parallel)
	for j := range f.Cols {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var dets []UnivariateDetector
			if !o.SkipMatrixProfile {
				dets = append(dets, &MatrixProfile{Window: o.Window, KNN: o.KNN, NJobs: 2})
			}
			if !o.SkipAR {
				dets = append(dets, &ARResidual{Lags: lags})
			}
			if !o.SkipLevel {
				dets = append(dets, &LevelShift{Window: o.Window})
			}
			if o.Season > 1 && !o.SkipSeasonal {
				dets = append(dets, &SeasonalResidual{Season: o.Season})
			}
			if !o.SkipIForest {
				dets = append(dets, &IsolationForest{Window: 4, SeasonPhase: o.Season, Seed: int64(j)})
			}
			res := MetricResult{Name: f.Names[j]}
			ens, err := EnsembleUni(f.Cols[j], o.Combine, dets...)
			if err != nil {
				res.Err = err
			} else {
				res.Ensemble = ens
				res.Ranges = rangesOf(ens, flagsOf(ens))
			}
			rep.Metrics[j] = res
		}(j)
	}
	wg.Wait()

	// ---- multivariate ----
	if len(f.Cols) >= 2 {
		mr := &MultiResult{}
		var dets []MultivariateDetector
		var pca *PCA
		if !o.SkipPCA {
			pca = &PCA{TrainFraction: o.PCATrainFr}
			dets = append(dets, pca)
		}
		if !o.SkipIForest {
			dets = append(dets, &IsolationForest{Window: 1, SeasonPhase: o.Season, Seed: 7})
		}
		var kmp *MultiMatrixProfile
		if !o.SkipKMP {
			kmp = &MultiMatrixProfile{Window: o.Window, KNN: o.KNN}
			if o.KMPDimsSet || o.KMPDims != 0 {
				kmp.WithK(o.KMPDims)
			}
			dets = append(dets, kmp)
		}
		ens, err := EnsembleMulti(f.Cols, o.Combine, dets...)
		if err != nil {
			mr.Err = err
		} else {
			mr.Ensemble = ens
			mr.Ranges = rangesOf(ens, flagsOf(ens))
			var pcaZ, mpZ []float64
			if pca != nil {
				mr.Contribution = pca.Contribution
				pcaZ = ens.zOf(pca.Name())
			}
			if kmp != nil {
				mr.MPShare = kmp.Share
				mpZ = ens.zOf(kmp.Name())
			}
			for _, r := range mr.Ranges {
				mr.Attribution = append(mr.Attribution, attribute(rep, r, mr.Contribution, mr.MPShare, pcaZ, mpZ))
			}
		}
		rep.Multi = mr
	}
	return rep, nil
}

// TopContributors returns, for a time index, the metrics ordered by their
// share in the PCA residual (largest first). Returns nil if unavailable.
// Prefer MultiResult.Attribution, which combines PCA with the univariate
// scores and the multidimensional matrix profile.
func (r *Report) TopContributors(t, k int) []struct {
	Name  string
	Share float64
} {
	if r.Multi == nil || r.Multi.Contribution == nil || t < 0 || t >= len(r.Multi.Contribution) {
		return nil
	}
	c := r.Multi.Contribution[t]
	type ns struct {
		Name  string
		Share float64
	}
	items := make([]ns, len(c))
	for j := range c {
		items[j] = ns{r.Frame.Names[j], c[j]}
	}
	// simple selection of top-k
	out := make([]struct {
		Name  string
		Share float64
	}, 0, k)
	used := make([]bool, len(items))
	for len(out) < k && len(out) < len(items) {
		best := -1
		for j := range items {
			if !used[j] && (best == -1 || items[j].Share > items[best].Share) {
				best = j
			}
		}
		used[best] = true
		out = append(out, struct {
			Name  string
			Share float64
		}{items[best].Name, items[best].Share})
	}
	return out
}
