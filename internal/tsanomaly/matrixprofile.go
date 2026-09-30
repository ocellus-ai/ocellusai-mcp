package tsanomaly

import "fmt"

// MatrixProfile detects subsequence (shape) anomalies in one series: the score
// of a point is the largest z-normalised Euclidean distance, over all windows
// covering that point, from the window to its nearest non-trivial
// neighbour(s) anywhere in the series. Windows with no look-alike ("discords")
// get high scores.
//
// Being z-normalised, it is blind to amplitude by design: "the same shape,
// ten times higher" is not a discord. Pair it with ARResidual /
// SeasonalResidual, which catch amplitude and level changes.
//
// Window should cover one "shape" of interest, e.g. one seasonal cycle or the
// typical duration of an incident. A rule of thumb for 15s-granularity metrics:
// 1-2 hours -> 240-480 points. Events much shorter than Window/4 are diluted.
//
// Complexity O(n^2) time, O(n) memory; parallel over rows.
type MatrixProfile struct {
	Window int // subsequence length in points (required, 2*Window < n)
	// KNN averages the KNN nearest neighbours (default 2). With 1, two
	// identical incidents in the batch find each other and are both missed.
	KNN int
	// MaxImputedFrac: windows with a larger share of imputed points are
	// neither scored nor used as neighbours (default 0.05).
	MaxImputedFrac float64
	Smooth         int // moving-average width applied to the profile (0 = off)
	NJobs          int // parallelism (0 = all CPUs)
}

func (d *MatrixProfile) Name() string      { return fmt.Sprintf("mp(w=%d)", d.Window) }
func (d *MatrixProfile) Subsequence() bool { return true }

func (d *MatrixProfile) knn() int {
	if d.KNN <= 0 {
		return 2
	}
	return d.KNN
}

func (d *MatrixProfile) maxImputed() float64 {
	if d.MaxImputedFrac <= 0 {
		return 0.05
	}
	return d.MaxImputedFrac
}

func (d *MatrixProfile) Score(x []float64) ([]float64, error) {
	n := len(x)
	if d.Window < 4 || 2*d.Window >= n {
		return nil, fmt.Errorf("matrixprofile: window must be >=4 and < n/2 (window=%d, n=%d)", d.Window, n)
	}
	y, imputed := ImputeMask(x)
	y = robustScale(y)
	frac := windowImputedFrac(imputed, d.Window)
	exclude := make([]bool, len(frac))
	for i, f := range frac {
		exclude[i] = f > d.maxImputed()
	}
	prof, _ := stompProfile(mpInput{dims: [][]float64{y}, w: d.Window, knn: d.knn(), njobs: d.NJobs, exclude: exclude})
	prof = MovingAverage(nanToZero(prof), d.Smooth)
	return maskScores(windowToPoints(prof, n, d.Window), imputed), nil
}

// MultiMatrixProfile is the k-dimensional matrix profile (mSTOMP). For every
// pair of subsequence positions the per-metric z-normalised distances are
// combined across dimensions:
//
//	K = -1 : worst metric (default; an anomaly in one metric out of many stays visible)
//	K = 0  : average over all metrics (a single broken metric out of 20 is diluted)
//	K = k  : average of the k best-matching metrics (an anomaly must show up
//	         in at least d-k+1 metrics to be visible; use for "system-wide" events)
//
// Complexity is O(n^2 * d): fine for batches up to a few tens of thousands
// of points.
type MultiMatrixProfile struct {
	Window         int
	K              int
	KNN            int
	MaxImputedFrac float64
	Smooth         int
	NJobs          int
	kSet           bool

	// Filled by ScoreMulti: Share[t][j] is the share of metric j in the
	// distance between the window that scored point t and its nearest
	// neighbour (rows sum to 1; nil rows where nothing was scored).
	Share [][]float64
}

// WithK sets K explicitly (needed because 0 is a valid value distinct from
// the default -1).
func (d *MultiMatrixProfile) WithK(k int) *MultiMatrixProfile {
	d.K, d.kSet = k, true
	return d
}

func (d *MultiMatrixProfile) k() int {
	if !d.kSet && d.K == 0 {
		return -1
	}
	return d.K
}

func (d *MultiMatrixProfile) Subsequence() bool { return true }

func (d *MultiMatrixProfile) Name() string {
	return fmt.Sprintf("kmp(w=%d,k=%d)", d.Window, d.k())
}

func (d *MultiMatrixProfile) ScoreMulti(cols [][]float64) ([]float64, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("kmp: no columns")
	}
	n := len(cols[0])
	if d.Window < 4 || 2*d.Window >= n {
		return nil, fmt.Errorf("kmp: window must be >=4 and < n/2 (window=%d, n=%d)", d.Window, n)
	}
	maxImp := d.MaxImputedFrac
	if maxImp <= 0 {
		maxImp = 0.05
	}
	t := make([][]float64, len(cols))
	anyImputed := make([]bool, n)
	for j, c := range cols {
		if len(c) != n {
			return nil, fmt.Errorf("kmp: column %d has length %d, want %d", j, len(c), n)
		}
		y, imp := ImputeMask(c)
		t[j] = robustScale(y)
		for i, b := range imp {
			anyImputed[i] = anyImputed[i] || b
		}
	}
	frac := windowImputedFrac(anyImputed, d.Window)
	exclude := make([]bool, len(frac))
	for i, f := range frac {
		exclude[i] = f > maxImp
	}
	knn := d.KNN
	if knn <= 0 {
		knn = 2
	}
	prof, dimDist := stompProfile(mpInput{dims: t, w: d.Window, kDims: d.k(), knn: knn, njobs: d.NJobs, exclude: exclude})
	prof = MovingAverage(nanToZero(prof), d.Smooth)
	scores, src := windowToPointsIdx(prof, n, d.Window)
	d.Share = make([][]float64, n)
	for p := 0; p < n; p++ {
		i := src[p]
		if i < 0 || dimDist[i] == nil {
			continue
		}
		sum := 0.0
		for _, v := range dimDist[i] {
			sum += v
		}
		if sum <= 0 {
			continue
		}
		sh := make([]float64, len(cols))
		for j, v := range dimDist[i] {
			sh[j] = v / sum
		}
		d.Share[p] = sh
	}
	return maskScores(scores, anyImputed), nil
}
