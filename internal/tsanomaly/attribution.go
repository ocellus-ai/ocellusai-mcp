package tsanomaly

import "sort"

// MetricAttribution is one metric's evidence for a multivariate anomaly.
type MetricAttribution struct {
	Name string
	// Score is the combined evidence on the robust-z scale:
	// max(UniZ, PCAShare*pcaZ, MPShare*mpZ).
	Score float64
	// UniZ: this metric's own univariate ensemble score (max inside the
	// range). High UniZ = the metric is anomalous on its own (root cause);
	// low UniZ with high shares = the metric is normal in isolation but
	// broke its relationship with the others.
	UniZ float64
	// PCAShare: share of the PCA reconstruction residual at the peak
	// (correlation-structure break). Rows over metrics sum to 1.
	PCAShare float64
	// MPShare: share of the multidimensional matrix-profile distance at the
	// peak (shape mismatch). Rows over metrics sum to 1.
	MPShare float64
}

// RangeAttribution ranks metrics for one multivariate range.
type RangeAttribution struct {
	Range   Range
	Metrics []MetricAttribution // sorted by Score, descending
	// Kind is a coarse label: "single" (one metric dominates and is
	// anomalous by itself), "correlation" (no metric is anomalous alone,
	// the relationship broke), "systemic" (several metrics anomalous).
	Kind string
}

// uniThr is the univariate robust-z above which a metric counts as anomalous
// on its own, for ranking and for Kind.
const uniThr = 4.0

// attribute builds the attribution for one multivariate range.
func attribute(rep *Report, r Range, pcaShare, mpShare [][]float64, pcaZ, mpZ []float64) RangeAttribution {
	names := rep.Frame.Names
	out := RangeAttribution{Range: r, Metrics: make([]MetricAttribution, len(names))}
	peak := r.Peak
	for j, name := range names {
		a := MetricAttribution{Name: name}
		if j < len(rep.Metrics) && rep.Metrics[j].Ensemble != nil {
			c := rep.Metrics[j].Ensemble.Combined
			for t := r.Start; t <= r.End && t < len(c); t++ {
				if c[t] > a.UniZ {
					a.UniZ = c[t]
				}
			}
		}
		if pcaShare != nil && peak < len(pcaShare) && pcaShare[peak] != nil {
			a.PCAShare = pcaShare[peak][j]
		}
		if mpShare != nil && peak < len(mpShare) && mpShare[peak] != nil {
			a.MPShare = mpShare[peak][j]
		}
		a.Score = a.UniZ
		if pcaZ != nil && peak < len(pcaZ) {
			a.Score = maxF(a.Score, a.PCAShare*pcaZ[peak])
		}
		if mpZ != nil && peak < len(mpZ) {
			a.Score = maxF(a.Score, a.MPShare*mpZ[peak])
		}
		out.Metrics[j] = a
	}
	// Metrics anomalous on their own (UniZ above the threshold) rank first, by
	// their own z: they are the likely cause. The others follow by Score, which
	// for them rests on the PCA / matrix-profile shares. Ranking everyone by
	// Score alone lets a large multivariate z, split by nearly uniform shares
	// (with two metrics PCA always blames both about equally), outrank a metric
	// that visibly jumped by itself.
	sort.SliceStable(out.Metrics, func(x, y int) bool {
		mx, my := out.Metrics[x], out.Metrics[y]
		ax, ay := mx.UniZ > uniThr, my.UniZ > uniThr
		if ax != ay {
			return ax
		}
		if ax {
			return mx.UniZ > my.UniZ
		}
		return mx.Score > my.Score
	})

	// coarse classification
	anomalous := 0
	for _, m := range out.Metrics {
		if m.UniZ > uniThr {
			anomalous++
		}
	}
	switch {
	case anomalous == 0:
		out.Kind = "correlation"
	case anomalous == 1 || (len(out.Metrics) > 1 && out.Metrics[0].Score > 2*out.Metrics[1].Score):
		out.Kind = "single"
	default:
		out.Kind = "systemic"
	}
	return out
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
