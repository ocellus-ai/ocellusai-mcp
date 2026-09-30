package tsanomaly

import (
	"math"
	"math/rand"
	"testing"
)

// A long gap must not become a discord or an AR outlier.
func TestGapIsNotAnomaly(t *testing.T) {
	x, _, _, _ := synthUni(11)
	for i := 500; i < 560; i++ {
		x[i] = math.NaN()
	}
	ens, err := EnsembleUni(x, CombineMaxZ, &MatrixProfile{Window: 100}, &ARResidual{Order: 3}, &IsolationForest{Window: 4})
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := ens.Flags(ThresholdPolicy{})
	for _, r := range Ranges(fl, ens.Combined, 1, 10) {
		if r.End >= 480 && r.Start <= 580 {
			t.Fatalf("gap flagged as anomaly: %+v", r)
		}
	}
}

// Counter-like scale (1e9) and a flat plateau must not produce junk.
func TestBigScaleAndPlateau(t *testing.T) {
	x, spike, s, e := synthUni(12)
	for i := range x {
		if !math.IsNaN(x[i]) {
			x[i] = 3e9 + 1e6*x[i]
		}
	}
	for i := 1500; i < 1700; i++ { // plateau
		x[i] = 3e9 + 1e7
	}
	ens, err := EnsembleUni(x, CombineMaxZ, &MatrixProfile{Window: 100}, &ARResidual{Order: 3})
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := ens.Flags(ThresholdPolicy{})
	rs := Ranges(fl, ens.Combined, 1, 10)
	hitSpike, hitShape, insidePlateau := false, false, false
	for _, r := range rs {
		if r.Start <= spike && spike <= r.End {
			hitSpike = true
		}
		if r.End >= s && r.Start <= e {
			hitShape = true
		}
		if r.Start > 1520 && r.End < 1680 {
			insidePlateau = true
		}
	}
	if !hitSpike || !hitShape || insidePlateau {
		t.Fatalf("spike=%v shape=%v insidePlateau=%v ranges=%v", hitSpike, hitShape, insidePlateau, rs)
	}
}

// Two identical incidents: KNN=1 would let them match each other.
func TestTwinAnomalies(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	n := 3000
	x := make([]float64, n)
	for i := range x {
		x[i] = math.Sin(2*math.Pi*float64(i)/100) + rng.NormFloat64()*0.1
	}
	for _, at := range []int{800, 2100} {
		for i := at; i < at+50; i++ {
			x[i] = rng.NormFloat64() * 0.1 // same flattened shape twice
		}
	}
	one, _ := (&MatrixProfile{Window: 100, KNN: 1}).Score(x)
	two, _ := (&MatrixProfile{Window: 100, KNN: 2}).Score(x)
	z1 := RobustZ(one)
	z2 := RobustZ(two)
	if z2[825] <= z1[825] || z2[2125] < 6 {
		t.Fatalf("KNN=2 should score twins higher: knn1=%.1f knn2=%.1f", z1[825], z2[825])
	}
}

// Everything grows proportionally: SPE is blind, T² must catch it.
func TestPCAProportionalGrowth(t *testing.T) {
	cols, _, _ := synthMulti(2000, 14)
	for j := range cols {
		for t := 1300; t < 1320; t++ {
			cols[j][t] *= 4
		}
	}
	d := &PCA{}
	sc, err := d.ScoreMulti(cols)
	if err != nil {
		t.Fatal(err)
	}
	peak := 0.0
	for t := 1300; t < 1320; t++ {
		peak = math.Max(peak, sc[t])
	}
	if peak < 6 {
		t.Fatalf("proportional growth missed: max z=%.1f", peak)
	}
}

// Peak of the MP range must sit inside the true event (no centre shift).
func TestMPLocalisation(t *testing.T) {
	x, _, s, e := synthUni(15)
	sc, _ := (&MatrixProfile{Window: 100}).Score(x)
	best := 0
	for i := range sc {
		if sc[i] > sc[best] {
			best = i
		}
	}
	if best < s || best > e {
		t.Fatalf("peak at %d, event is [%d,%d]", best, s, e)
	}
}

// Seasonal detector: value normal in range but wrong for the phase.
func TestSeasonalResidual(t *testing.T) {
	rng := rand.New(rand.NewSource(16))
	S := 200
	n := 5 * S
	x := make([]float64, n)
	for i := range x {
		x[i] = 5 + 4*math.Sin(2*math.Pi*float64(i)/float64(S)) + rng.NormFloat64()*0.2
	}
	// at the trough of cycle 3, push the value to the normal *peak* level
	at := 3*S + 3*S/4
	for i := at; i < at+5; i++ {
		x[i] = 9
	}
	sc, err := (&SeasonalResidual{Season: S}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if sc[at+2] < 6 {
		t.Fatalf("seasonal miss: z=%.1f", sc[at+2])
	}
}

// AR with a lag longer than half the batch must still score the prefix.
func TestARPrefixFallback(t *testing.T) {
	x, spike, _, _ := synthUni(17)
	sc, err := (&ARResidual{Lags: []int{1, 2, 3, 1500}}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if sc[spike] < 6 {
		t.Fatalf("spike before max lag not scored: %.1f", sc[spike])
	}
}

// POT threshold approximates the true risk quantile and tightens with n.
func TestPOTDependsOnN(t *testing.T) {
	rng := rand.New(rand.NewSource(18))
	small := make([]float64, 2000)
	large := make([]float64, 40000)
	for i := range small {
		small[i] = math.Abs(rng.NormFloat64())
	}
	for i := range large {
		large[i] = math.Abs(rng.NormFloat64())
	}
	ts := POTThreshold(small, 0.98, 1e-4, 0)
	tl := POTThreshold(large, 0.98, 1e-4, 0)
	// true 1e-4 quantile of |N(0,1)| is 3.89; the fit on 40 exceedances is noisy
	if !(ts > 3 && ts < 7 && tl > 3.4 && tl < 4.4) {
		t.Fatalf("unexpected thresholds small=%.2f large=%.2f", ts, tl)
	}
}
