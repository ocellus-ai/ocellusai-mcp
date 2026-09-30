package tsanomaly

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/mat"
)

// PCA detects multivariate anomalies against the normal linear correlation
// structure between metrics. Columns are scaled by median/MAD, principal
// components are fitted, and each row gets two statistics:
//
//   - SPE (Q): norm of the residual outside the retained subspace. High when
//     one metric decouples from its usual partners.
//   - T² (Hotelling): Mahalanobis distance inside the subspace. High when all
//     metrics move together further than they normally do ("everything grew
//     proportionally") — a case SPE alone misses because it lies exactly in
//     the modelled subspace.
//
// The score is max(z(SPE), z(T²)) where z is the robust z-score of each
// statistic, so the two are on a comparable scale. Scores are Calibrated.
//
// Fitting is done twice: rows whose first-pass score exceeds TrimZ are
// dropped from the second fit, which limits how much a long anomaly can
// mask itself by entering the components. Set TrainFraction < 1 if the
// leading part of the batch is known to be clean.
type PCA struct {
	Components    int     // number of components; 0 = choose by VarianceRatio
	VarianceRatio float64 // explained variance to keep when Components==0 (default 0.9)
	TrainFraction float64 // fraction of leading rows used for fitting (default 1.0)
	Lag           int     // lag embedding: append x_{t-1..t-Lag} to each row (0 = off)
	TrimZ         float64 // first-pass trim threshold for the refit (default 4)

	// Filled by ScoreMulti.
	SPE, T2      []float64   // raw statistics per row
	Contribution [][]float64 // Contribution[t][j]: share of metric j in the residual at row t
}

func (d *PCA) Name() string     { return fmt.Sprintf("pca(k=%d)", d.Components) }
func (d *PCA) Calibrated() bool { return true }

func (d *PCA) ScoreMulti(cols [][]float64) ([]float64, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("pca: no columns")
	}
	n := len(cols[0])
	imp := make([][]float64, len(cols))
	anyImputed := make([]bool, n)
	for j, c := range cols {
		if len(c) != n {
			return nil, fmt.Errorf("pca: column %d has length %d, want %d", j, len(c), n)
		}
		y, m := ImputeMask(c)
		imp[j] = y
		for i, b := range m {
			anyImputed[i] = anyImputed[i] || b
		}
	}
	imp = lagEmbed(imp, d.Lag)
	d0 := len(imp)

	tf := d.TrainFraction
	if tf <= 0 || tf > 1 {
		tf = 1
	}
	trimZ := d.TrimZ
	if trimZ <= 0 {
		trimZ = 4
	}
	train := make([]bool, n)
	cnt := 0
	for i := 0; i < int(float64(n)*tf); i++ {
		train[i] = !anyImputed[i]
		if train[i] {
			cnt++
		}
	}
	if cnt < d0+2 {
		return nil, fmt.Errorf("pca: not enough clean rows (%d) for %d columns", cnt, d0)
	}

	var scores []float64
	for pass := 0; pass < 2; pass++ {
		var err error
		scores, err = d.fitScore(imp, train, len(cols))
		if err != nil {
			return nil, err
		}
		if pass == 0 {
			kept := 0
			next := make([]bool, n)
			for i := range train {
				next[i] = train[i] && scores[i] <= trimZ
				if next[i] {
					kept++
				}
			}
			if kept < d0+2 {
				break
			}
			train = next
		}
	}
	return maskScores(scores, anyImputed), nil
}

func (d *PCA) fitScore(imp [][]float64, train []bool, nMetrics int) ([]float64, error) {
	n := len(imp[0])
	d0 := len(imp)
	std := standardizeCols(imp, train)

	X := mat.NewDense(n, d0, nil)
	for j := 0; j < d0; j++ {
		for i := 0; i < n; i++ {
			X.Set(i, j, std[j][i])
		}
	}
	trainRows := 0
	for _, b := range train {
		if b {
			trainRows++
		}
	}
	Xtr := mat.NewDense(trainRows, d0, nil)
	r := 0
	for i := 0; i < n; i++ {
		if train[i] {
			Xtr.SetRow(r, X.RawRowView(i))
			r++
		}
	}

	var svd mat.SVD
	if !svd.Factorize(Xtr, mat.SVDThin) {
		return nil, fmt.Errorf("pca: svd failed")
	}
	sv := svd.Values(nil)
	var V mat.Dense
	svd.VTo(&V)

	k := d.Components
	if k <= 0 {
		ratio := d.VarianceRatio
		if ratio <= 0 || ratio >= 1 {
			ratio = 0.9
		}
		total := 0.0
		for _, s := range sv {
			total += s * s
		}
		acc := 0.0
		for i, s := range sv {
			acc += s * s
			if acc/total >= ratio {
				k = i + 1
				break
			}
		}
		if k == 0 {
			k = len(sv)
		}
	}
	if k > len(sv) {
		k = len(sv)
	}
	if k >= d0 { // keep at least one residual dimension
		k = maxInt(d0-1, 1)
	}
	Vk := V.Slice(0, d0, 0, k).(*mat.Dense)

	var proj, recon mat.Dense
	proj.Mul(X, Vk)          // n x k scores
	recon.Mul(&proj, Vk.T()) // n x d0

	d.SPE = make([]float64, n)
	d.T2 = make([]float64, n)
	d.Contribution = make([][]float64, n)
	for i := 0; i < n; i++ {
		s := 0.0
		contrib := make([]float64, nMetrics)
		for j := 0; j < d0; j++ {
			res := X.At(i, j) - recon.At(i, j)
			s += res * res
			contrib[j%nMetrics] += res * res
		}
		d.SPE[i] = math.Sqrt(s)
		if s > 0 {
			for j := range contrib {
				contrib[j] /= s
			}
		}
		d.Contribution[i] = contrib
		// T²: sum of squared component scores scaled by their variance
		t2 := 0.0
		for c := 0; c < k; c++ {
			v := sv[c] * sv[c] / float64(maxInt(trainRows-1, 1))
			if v < 1e-12 {
				v = 1e-12
			}
			p := proj.At(i, c)
			t2 += p * p / v
		}
		d.T2[i] = math.Sqrt(t2)
	}

	zSPE := robustZOn(d.SPE, train)
	zT2 := robustZOn(d.T2, train)
	scores := make([]float64, n)
	for i := range scores {
		scores[i] = math.Max(zSPE[i], zT2[i])
	}
	return scores, nil
}

// robustZOn computes (x - median)/MAD with median/MAD taken over rows
// flagged in ref, clipped at 0.
func robustZOn(x []float64, ref []bool) []float64 {
	sub := make([]float64, 0, len(x))
	for i, v := range x {
		if ref == nil || ref[i] {
			sub = append(sub, v)
		}
	}
	med, mad := MAD(sub)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = math.Max(0, (v-med)/mad)
	}
	return out
}

// lagEmbed appends lagged copies of every column (shifted, edge-padded).
func lagEmbed(cols [][]float64, lag int) [][]float64 {
	if lag <= 0 {
		return cols
	}
	n := len(cols[0])
	out := append([][]float64(nil), cols...)
	for l := 1; l <= lag; l++ {
		for _, c := range cols {
			s := make([]float64, n)
			for t := 0; t < n; t++ {
				src := t - l
				if src < 0 {
					src = 0
				}
				s[t] = c[src]
			}
			out = append(out, s)
		}
	}
	return out
}
