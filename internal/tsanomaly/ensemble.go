package tsanomaly

import (
	"fmt"
	"math"
	"sort"
)

// Scored is one detector's output.
type Scored struct {
	Detector string
	Scores   []float64
	// Calibrated marks scores that are already robust z-scores; the ensemble
	// uses them as-is instead of normalising a second time.
	Calibrated bool
	// Spread marks window-spread scores (see subsequence).
	Spread bool
}

// Calibrated is implemented by detectors whose scores are already robust
// z-scores (ARResidual, SeasonalResidual, PCA).
type calibrated interface{ Calibrated() bool }

// subsequence is implemented by detectors whose per-point score is spread
// over a window (Matrix Profile family). Their score distribution has a
// plateau around every event, so extreme-value fitting does not apply and
// the floor is used instead. (LevelShift also scores plateaus but stays
// point-wise: the floor alone let heavy-tailed residuals through.)
type subsequence interface{ Subsequence() bool }

func isSubsequence(d interface{}) bool {
	c, ok := d.(subsequence)
	return ok && c.Subsequence()
}

func isCalibrated(d interface{}) bool {
	c, ok := d.(calibrated)
	return ok && c.Calibrated()
}

// Combine selects how per-detector scores are merged.
type Combine int

const (
	// CombineMaxZ: each detector's scores are converted to robust z-scores
	// (|s - median| / MAD, clipped at 0) and the maximum over detectors is
	// taken. Detectors here are complementary (shape / point / density), so an
	// anomaly is usually seen by only one of them; max keeps it visible.
	// The result is on a "sigma" scale: 3 is mild, 6+ is a clear anomaly.
	CombineMaxZ Combine = iota
	// CombineMeanZ averages the robust z-scores (more conservative).
	CombineMeanZ
	// CombineRank averages normalised ranks (scale [0,1]); scale-free but an
	// anomaly seen by one of k detectors is diluted to ~ (1 + (k-1)/2) / k.
	CombineRank
)

// EnsembleResult is the combined output of several detectors.
type EnsembleResult struct {
	Combined []float64 // combined score (scale depends on Combine)
	Parts    []Scored  // raw per-detector scores (for debugging / attribution)
	Z        []Scored  // per-detector robust z-scores (nil for CombineRank)
}

// CombineScores merges heterogeneous score vectors. Weights may be nil.
func CombineScores(parts []Scored, weights []float64, how Combine) (*EnsembleResult, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("ensemble: no scores")
	}
	n := len(parts[0].Scores)
	for _, p := range parts {
		if len(p.Scores) != n {
			return nil, fmt.Errorf("ensemble: %s returned %d scores, want %d", p.Detector, len(p.Scores), n)
		}
	}
	res := &EnsembleResult{Parts: parts, Combined: make([]float64, n)}
	w := func(i int) float64 {
		if weights == nil {
			return 1
		}
		return weights[i]
	}
	switch how {
	case CombineRank:
		wsum := 0.0
		for i, p := range parts {
			r := Ranks(sanitize(append([]float64(nil), p.Scores...)))
			for t := range res.Combined {
				res.Combined[t] += w(i) * r[t]
			}
			wsum += w(i)
		}
		for t := range res.Combined {
			res.Combined[t] /= wsum
		}
	default:
		res.Z = make([]Scored, len(parts))
		for i, p := range parts {
			sc := sanitize(append([]float64(nil), p.Scores...))
			z := make([]float64, n)
			if p.Calibrated {
				for t, v := range sc {
					z[t] = math.Max(0, v) * w(i)
				}
			} else {
				// median/MAD over the non-zero part: masked (imputed) points are
				// exactly 0 and must not drag the scale down
				ref := make([]float64, 0, n)
				for _, v := range sc {
					if v != 0 {
						ref = append(ref, v)
					}
				}
				if len(ref) < 3 {
					ref = sc
				}
				med, mad := MAD(ref)
				for t, v := range sc {
					z[t] = math.Max(0, (v-med)/mad) * w(i)
				}
			}
			res.Z[i] = Scored{Detector: p.Detector, Scores: z, Calibrated: true, Spread: p.Spread}
		}
		for t := 0; t < n; t++ {
			acc := 0.0
			for i := range parts {
				z := res.Z[i].Scores[t]
				if how == CombineMeanZ {
					acc += z / float64(len(parts))
				} else if z > acc {
					acc = z
				}
			}
			res.Combined[t] = acc
		}
	}
	return res, nil
}

// RankAverage is kept for callers that want plain rank averaging.
func RankAverage(parts []Scored, weights []float64) ([]float64, error) {
	r, err := CombineScores(parts, weights, CombineRank)
	if err != nil {
		return nil, err
	}
	return r.Combined, nil
}

// EnsembleUni runs every detector on x and combines the results with how.
// Detectors that fail (e.g. series too short for the window) are skipped
// unless all of them fail.
func EnsembleUni(x []float64, how Combine, dets ...UnivariateDetector) (*EnsembleResult, error) {
	var parts []Scored
	var lastErr error
	for _, d := range dets {
		s, err := d.Score(x)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", d.Name(), err)
			continue
		}
		parts = append(parts, Scored{Detector: d.Name(), Scores: s, Calibrated: isCalibrated(d), Spread: isSubsequence(d)})
	}
	if len(parts) == 0 {
		return nil, lastErr
	}
	return CombineScores(parts, nil, how)
}

// EnsembleMulti is the multivariate counterpart of EnsembleUni.
func EnsembleMulti(cols [][]float64, how Combine, dets ...MultivariateDetector) (*EnsembleResult, error) {
	var parts []Scored
	var lastErr error
	for _, d := range dets {
		s, err := d.ScoreMulti(cols)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", d.Name(), err)
			continue
		}
		parts = append(parts, Scored{Detector: d.Name(), Scores: s, Calibrated: isCalibrated(d), Spread: isSubsequence(d)})
	}
	if len(parts) == 0 {
		return nil, lastErr
	}
	return CombineScores(parts, nil, how)
}

// ---- thresholding ----

// ThresholdPolicy decides per-detector thresholds on the robust-z scale.
type ThresholdPolicy struct {
	Risk  float64 // POT per-point risk for point-wise detectors (default 1e-4)
	Floor float64 // minimum threshold for every detector (default 4)
	// Cap bounds the POT threshold to Cap times the level a Gaussian-like
	// tail of the same body width would reach at 1-Risk. EVT assumes the
	// exceedances are the normal tail; when incidents make up more than
	// ~1-2% of the batch the fit describes the incidents instead and the
	// threshold explodes. The cap is the conservative fallback for that
	// case (default 3; on clean data POT sits near 1x and the cap is idle).
	Cap float64
}

func (p ThresholdPolicy) norm() ThresholdPolicy {
	if p.Risk <= 0 {
		p.Risk = 1e-4
	}
	if p.Floor == 0 {
		p.Floor = 4
	} else if p.Floor < 0 {
		p.Floor = 0
	}
	if p.Cap <= 0 {
		p.Cap = 3
	}
	return p
}

// Thresholds computes one threshold per detector: POT for point-wise
// detectors (bounded by Floor and Cap), Floor for window-spread ones.
// Requires CombineMaxZ / CombineMeanZ (Z must be populated).
func (r *EnsembleResult) Thresholds(pol ThresholdPolicy) map[string]float64 {
	pol = pol.norm()
	gauss := gaussQuantile(1 - pol.Risk)
	gauss90 := gaussQuantile(0.95) // q90 of a folded normal = one-sided 0.95
	out := make(map[string]float64, len(r.Z))
	for _, z := range r.Z {
		thr := pol.Floor
		if !z.Spread {
			t := POTThreshold(z.Scores, 0.98, pol.Risk, pol.Floor)
			// scale the cap by how wide this detector's body is compared to a
			// half-normal (iForest z-scores are heavy-tailed by nature). The
			// width is read at q90, which incidents of a few percent of the
			// batch cannot move.
			width := math.Max(1, Quantile(z.Scores, 0.90)/gauss90)
			cap := pol.Cap * gauss * width
			if t > cap {
				t = math.Max(pol.Floor, cap)
			}
			thr = t
		}
		out[z.Detector] = thr
	}
	return out
}

// zOf returns the robust-z scores of the named detector, or nil.
func (r *EnsembleResult) zOf(name string) []float64 {
	for _, z := range r.Z {
		if z.Detector == name {
			return z.Scores
		}
	}
	return nil
}

// PointScores returns the max robust z over point-wise (non-spread)
// detectors, or nil if there are none.
func (r *EnsembleResult) PointScores() []float64 {
	var out []float64
	for _, z := range r.Z {
		if z.Spread {
			continue
		}
		if out == nil {
			out = make([]float64, len(z.Scores))
		}
		for i, v := range z.Scores {
			if v > out[i] {
				out[i] = v
			}
		}
	}
	return out
}

// Flags marks a point when any detector exceeds its own threshold.
func (r *EnsembleResult) Flags(pol ThresholdPolicy) ([]bool, map[string]float64) {
	thr := r.Thresholds(pol)
	n := len(r.Combined)
	flags := make([]bool, n)
	for _, z := range r.Z {
		t := thr[z.Detector]
		for i, v := range z.Scores {
			if v > t {
				flags[i] = true
			}
		}
	}
	return flags, thr
}

// gaussQuantile is the standard normal quantile (Acklam's approximation).
func gaussQuantile(p float64) float64 {
	if p <= 0 || p >= 1 {
		return math.NaN()
	}
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	if p < 0.02425 {
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	if p > 1-0.02425 {
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}

// POTThreshold sets a threshold with extreme-value theory (Peaks Over
// Threshold, as in SPOT, Siffer et al. 2017): exceedances over the initQ
// quantile are fitted with a generalised Pareto distribution (probability
// weighted moments), and the threshold is the level exceeded with
// probability risk per point. Unlike a fixed "z = 6" it adapts to the tail
// shape of the actual score distribution and to n: the expected number of
// false positives is about risk*n regardless of series length.
//
// EVT assumes anomalies are rare. When the top of the distribution is a
// plateau (a long incident, or a Matrix Profile discord spread over its
// window) there is no tail to fit; the function then returns floor. The
// result is never below floor, so floor acts as a sanity minimum on the
// robust-z scale (4 is a reasonable value; 0 disables it).
//
// Typical values: initQ = 0.98, risk = 1e-4 (≈0.6 false points per 24h at
// 15s). initQ is lowered automatically until at least 30 exceedances exist.
func POTThreshold(scores []float64, initQ, risk, floor float64) float64 {
	if initQ <= 0 || initQ >= 1 {
		initQ = 0.98
	}
	if risk <= 0 || risk >= 1 {
		risk = 1e-4
	}
	clean := make([]float64, 0, len(scores))
	for _, v := range scores {
		if isFinite(v) {
			clean = append(clean, v)
		}
	}
	n := float64(len(clean))
	// Iterate: a fit contaminated by incidents yields a threshold that is too
	// high; drop what lies above it and refit. The estimate descends until
	// the exceedance set is the normal tail. n is kept at its original value
	// so the risk still refers to the full series.
	cur := clean
	thr := math.Inf(1)
	for iter := 0; iter < 6; iter++ {
		t := potOnce(cur, n, initQ, risk)
		if !(t < thr) { // no progress
			break
		}
		thr = t
		next := cur[:0:0]
		for _, v := range cur {
			if v <= t {
				next = append(next, v)
			}
		}
		if len(next) == len(cur) {
			break
		}
		cur = next
	}
	if !isFinite(thr) {
		thr = Quantile(clean, initQ)
	}
	return math.Max(thr, floor)
}

// potOnce fits one GPD to the exceedances of data over its initQ quantile
// and returns the risk-level threshold; n is the reference series length.
func potOnce(data []float64, n, initQ, risk float64) float64 {
	var u float64
	var y []float64
	for q := initQ; q >= 0.85; q -= 0.02 {
		u = Quantile(data, q)
		y = y[:0]
		for _, v := range data {
			if v > u {
				y = append(y, v-u)
			}
		}
		if len(y) >= 30 {
			break
		}
	}
	nt := float64(len(y))
	if nt < 10 {
		return u
	}
	sort.Float64s(y)
	// PWM estimators (Hosking & Wallis 1987): a0 = E[Y], a1 = E[Y(1-F(Y))]
	a0, a1 := 0.0, 0.0
	for j, v := range y {
		a0 += v
		a1 += v * (nt - 1 - float64(j)) / (nt - 1)
	}
	a0 /= nt
	a1 /= nt
	den := a0 - 2*a1
	if den <= 1e-12 || a0 <= 0 { // degenerate (ties / plateau): no tail to fit
		return u
	}
	sigma := 2 * a0 * a1 / den
	xi := 2 - a0/den
	if xi > 0.9 { // cap: heavier tails are not credible for scores
		xi = 0.9
	}
	if xi < -0.5 {
		xi = -0.5
	}
	ratio := risk * n / nt
	var thr float64
	if math.Abs(xi) < 1e-6 {
		thr = u - sigma*math.Log(ratio)
	} else {
		thr = u + sigma/xi*(math.Pow(ratio, -xi)-1)
	}
	if !isFinite(thr) || thr < u {
		thr = u
	}
	return thr
}

// MADThreshold returns median + k*MAD of the scores (k≈3-5 is typical).
func MADThreshold(scores []float64, k float64) float64 {
	med, mad := MAD(scores)
	return med + k*mad
}

// QuantileThreshold returns the q-quantile of scores (e.g. 0.99 to flag the
// top 1% of points).
func QuantileThreshold(scores []float64, q float64) float64 {
	return Quantile(scores, q)
}

// Flag marks scores strictly above thr.
func Flag(scores []float64, thr float64) []bool {
	f := make([]bool, len(scores))
	for i, s := range scores {
		f[i] = isFinite(s) && s > thr
	}
	return f
}

// Ranges turns per-point flags into segments. Segments separated by at most
// mergeGap unflagged points are merged; segments shorter than minLen are
// dropped. scores is used to locate the peak of every range.
func Ranges(flags []bool, scores []float64, minLen, mergeGap int) []Range {
	var out []Range
	n := len(flags)
	i := 0
	for i < n {
		if !flags[i] {
			i++
			continue
		}
		start := i
		end := i
		for i < n {
			if flags[i] {
				end = i
				i++
				continue
			}
			// look ahead for a gap
			j := i
			for j < n && !flags[j] && j-i < mergeGap {
				j++
			}
			if j < n && flags[j] && j-i < mergeGap+1 {
				i = j
				continue
			}
			break
		}
		if end-start+1 >= maxInt(minLen, 1) {
			r := Range{Start: start, End: end, Peak: start, PeakScore: math.Inf(-1)}
			for t := start; t <= end; t++ {
				if scores != nil && t < len(scores) && scores[t] > r.PeakScore {
					r.PeakScore, r.Peak = scores[t], t
				}
			}
			out = append(out, r)
		}
	}
	return out
}
