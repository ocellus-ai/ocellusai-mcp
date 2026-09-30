package tsanomaly

import "testing"

// One metric decouples (sign flip, same amplitude): attribution must name it.
func TestAttributionSingleMetric(t *testing.T) {
	cols, a, b := synthMulti(1500, 21)
	f := &Frame{Timestamps: make([]int64, 1500), Names: []string{"m0", "m1", "m2", "m3"}, Cols: cols}
	rep, err := Analyze(f, Options{Window: 50, SkipIForest: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, at := range rep.Multi.Attribution {
		if at.Range.End < a || at.Range.Start > b {
			continue
		}
		found = true
		if at.Metrics[0].Name != "m2" {
			t.Fatalf("expected m2 first, got %+v", at.Metrics)
		}
		if at.Metrics[0].PCAShare < 0.5 || at.Metrics[0].MPShare < 0.4 {
			t.Fatalf("weak shares: %+v", at.Metrics[0])
		}
	}
	if !found {
		t.Fatalf("anomaly not found: %v", rep.Multi.Ranges)
	}
}

// Everything grows together: no single metric should dominate.
func TestAttributionSystemic(t *testing.T) {
	cols, _, _ := synthMulti(1500, 22)
	for j := range cols {
		for t := 1000; t < 1030; t++ {
			cols[j][t] *= 5
		}
	}
	f := &Frame{Timestamps: make([]int64, 1500), Names: []string{"m0", "m1", "m2", "m3"}, Cols: cols}
	rep, err := Analyze(f, Options{Window: 50, SkipIForest: true, SkipKMP: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range rep.Multi.Attribution {
		if at.Range.End >= 1000 && at.Range.Start <= 1030 {
			if at.Kind != "systemic" {
				t.Fatalf("expected systemic, got %s: %+v", at.Kind, at.Metrics)
			}
			return
		}
	}
	t.Fatalf("event not found: %v", rep.Multi.Ranges)
}
