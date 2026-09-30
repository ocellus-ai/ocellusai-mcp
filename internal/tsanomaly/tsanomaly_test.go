package tsanomaly

import (
	"math"
	"math/rand"
	"testing"
)

// synthetic: sine with period 100 + noise; spike at 700, shape anomaly at 1200..1260
func synthUni(seed int64) (x []float64, spike, shapeStart, shapeEnd int) {
	const n = 2000
	rng := rand.New(rand.NewSource(seed))
	x = make([]float64, n)
	for t := range x {
		x[t] = 10 + 3*math.Sin(2*math.Pi*float64(t)/100) + rng.NormFloat64()*0.2
	}
	spike, shapeStart, shapeEnd = 700, 1200, 1260
	x[spike] += 12
	for t := shapeStart; t <= shapeEnd; t++ { // flatten a segment: shape changes, values stay in range
		x[t] = 10 + rng.NormFloat64()*0.2
	}
	x[300] = math.NaN() // gap
	return
}

func rankOf(scores []float64, idx int) float64 { return Ranks(scores)[idx] }

func maxIn(scores []float64, a, b int) float64 {
	m := math.Inf(-1)
	for t := a; t <= b; t++ {
		if scores[t] > m {
			m = scores[t]
		}
	}
	return m
}

func TestMatrixProfileFindsShapeAnomaly(t *testing.T) {
	x, _, s, e := synthUni(1)
	d := &MatrixProfile{Window: 100}
	sc, err := d.Score(x)
	if err != nil {
		t.Fatal(err)
	}
	thr := QuantileThreshold(sc, 0.95)
	if maxIn(sc, s, e) <= thr {
		t.Fatalf("shape anomaly not in top 5%%: max=%v thr=%v", maxIn(sc, s, e), thr)
	}
}

func TestARFindsSpike(t *testing.T) {
	x, spike, _, _ := synthUni(2)
	d := &ARResidual{Order: 3}
	sc, err := d.Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(sc, spike) < 0.999 {
		t.Fatalf("spike rank too low: %v", rankOf(sc, spike))
	}
}

func TestIForestUni(t *testing.T) {
	x, spike, _, _ := synthUni(3)
	d := &IsolationForest{Window: 4, Seed: 1}
	sc, err := d.Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(sc, spike) < 0.99 {
		t.Fatalf("spike rank too low: %v", rankOf(sc, spike))
	}
}

func TestUniEnsemble(t *testing.T) {
	x, spike, s, e := synthUni(4)
	ens, err := EnsembleUni(x, CombineMaxZ, &MatrixProfile{Window: 100}, &ARResidual{Order: 3}, &IsolationForest{Window: 4})
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := ens.Flags(ThresholdPolicy{})
	rs := Ranges(fl, ens.Combined, 1, 25)
	hitSpike, hitShape := false, false
	for _, r := range rs {
		if r.Start <= spike && spike <= r.End {
			hitSpike = true
		}
		if r.End >= s && r.Start <= e {
			hitShape = true
		}
	}
	if !hitSpike || !hitShape {
		t.Fatalf("ensemble missed: spike=%v shape=%v ranges=%v", hitSpike, hitShape, rs)
	}
}

// multivariate: 4 correlated metrics; at 900..920 metric 2 decouples from
// the others while staying within its normal range.
func synthMulti(n int, seed int64) (cols [][]float64, a, b int) {
	rng := rand.New(rand.NewSource(seed))
	base := make([]float64, n)
	for t := range base {
		base[t] = math.Sin(2*math.Pi*float64(t)/150) + 0.3*math.Sin(2*math.Pi*float64(t)/37)
	}
	cols = make([][]float64, 4)
	for j := range cols {
		cols[j] = make([]float64, n)
		for t := range base {
			cols[j][t] = float64(j+1)*base[t] + rng.NormFloat64()*0.05
		}
	}
	a, b = 900, 920
	for t := a; t <= b; t++ {
		cols[2][t] = -3*base[t] + rng.NormFloat64()*0.05 // sign flip: same range, broken correlation
	}
	return
}

func TestPCAFindsCorrelationBreak(t *testing.T) {
	cols, a, b := synthMulti(2000, 5)
	d := &PCA{VarianceRatio: 0.9}
	sc, err := d.ScoreMulti(cols)
	if err != nil {
		t.Fatal(err)
	}
	thr := QuantileThreshold(sc, 0.98)
	if maxIn(sc, a, b) <= thr {
		t.Fatalf("pca missed correlation break")
	}
	// contribution should blame metric 2
	peak := a
	for t := a; t <= b; t++ {
		if sc[t] > sc[peak] {
			peak = t
		}
	}
	best := 0
	for j := range d.Contribution[peak] {
		if d.Contribution[peak][j] > d.Contribution[peak][best] {
			best = j
		}
	}
	if best != 2 {
		t.Fatalf("expected metric 2 to dominate contribution, got %d: %v", best, d.Contribution[peak])
	}
}

func TestKMP(t *testing.T) {
	cols, a, b := synthMulti(1200, 6)
	d := &MultiMatrixProfile{Window: 40}
	sc, err := d.ScoreMulti(cols)
	if err != nil {
		t.Fatal(err)
	}
	thr := QuantileThreshold(sc, 0.95)
	if maxIn(sc, a, b) <= thr {
		t.Fatalf("kmp missed anomaly")
	}
}

func TestAnalyzePipeline(t *testing.T) {
	cols, a, b := synthMulti(1500, 7)
	f := &Frame{Timestamps: make([]int64, 1500), Names: []string{"m0", "m1", "m2", "m3"}, Cols: cols}
	for i := range f.Timestamps {
		f.Timestamps[i] = int64(i * 15)
	}
	rep, err := Analyze(f, Options{Window: 50, SkipIForest: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Multi == nil || rep.Multi.Err != nil {
		t.Fatalf("multi failed: %+v", rep.Multi)
	}
	hit := false
	for _, r := range rep.Multi.Ranges {
		if r.End >= a && r.Start <= b {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("pipeline missed multivariate anomaly: %v", rep.Multi.Ranges)
	}
	for _, m := range rep.Metrics {
		if m.Err != nil {
			t.Fatalf("metric %s: %v", m.Name, m.Err)
		}
	}
}

func TestAlignAndImpute(t *testing.T) {
	s := Series{Name: "x", Timestamps: []int64{0, 15, 45, 60}, Values: []float64{1, 2, 4, 5}}
	f, err := Align([]Series{s}, 0, 60, 15)
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 5 || !math.IsNaN(f.Cols[0][2]) {
		t.Fatalf("bad align: %v", f.Cols[0])
	}
	y := Impute(f.Cols[0])
	if y[2] != 3 {
		t.Fatalf("bad impute: %v", y)
	}
}
