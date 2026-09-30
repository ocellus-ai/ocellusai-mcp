package tsanomaly

import (
	"fmt"
	"math"
	"math/rand"
)

// IsolationForest (Liu et al. 2008). Rows that are easy to isolate by random
// axis-parallel splits get short path lengths and high scores. It is
// distribution-free and handles mixed scales and many columns well, which
// makes it a good complement to PCA (linear) and Matrix Profile (shape).
//
// Implements both interfaces: for a single series it embeds each point as the
// vector (x_t, x_{t-1}, ..., x_{t-Window+1}); for multivariate input each row
// is the vector of all metrics at t (plus Window-1 lags if Window>1).
//
// Keep Window small (<= 8): axis-parallel splits in a high-dimensional,
// almost collinear lag embedding produce random short paths. With
// SeasonPhase > 0 the phase within the season (t mod SeasonPhase) is added
// as a feature, so "normal value, wrong time of day" becomes isolable.
// Imputed points get score 0.
type IsolationForest struct {
	Trees       int // default 100
	SampleSize  int // default 256
	Window      int // lag embedding width (default 1 = no lags)
	SeasonPhase int // season length in points to add a phase feature (0 = off)
	Seed        int64
}

func (d *IsolationForest) Name() string { return fmt.Sprintf("iforest(w=%d)", d.win()) }

func (d *IsolationForest) win() int {
	if d.Window <= 0 {
		return 1
	}
	return d.Window
}

func (d *IsolationForest) Score(x []float64) ([]float64, error) {
	return d.ScoreMulti([][]float64{x})
}

func (d *IsolationForest) ScoreMulti(cols [][]float64) ([]float64, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("iforest: no columns")
	}
	n := len(cols[0])
	imp := make([][]float64, len(cols))
	anyImputed := make([]bool, n)
	for j, c := range cols {
		if len(c) != n {
			return nil, fmt.Errorf("iforest: column %d has length %d, want %d", j, len(c), n)
		}
		y, m := ImputeMask(c)
		imp[j] = y
		for i, b := range m {
			anyImputed[i] = anyImputed[i] || b
		}
	}
	imp = lagEmbed(imp, d.win()-1)
	std := standardizeCols(imp, nil)
	if d.SeasonPhase > 1 {
		// phase encoded as sin/cos so midnight is not a discontinuity
		sn := make([]float64, n)
		cs := make([]float64, n)
		for t := 0; t < n; t++ {
			a := 2 * math.Pi * float64(t%d.SeasonPhase) / float64(d.SeasonPhase)
			sn[t], cs[t] = math.Sin(a), math.Cos(a)
		}
		std = append(std, sn, cs)
	}

	dim := len(std)
	rows := make([][]float64, n)
	for i := 0; i < n; i++ {
		r := make([]float64, dim)
		for j := 0; j < dim; j++ {
			r[j] = std[j][i]
		}
		rows[i] = r
	}
	return maskScores(d.fitScore(rows), anyImputed), nil
}

type iNode struct {
	left, right *iNode
	splitAttr   int
	splitValue  float64
	size        int // leaf size
}

func (d *IsolationForest) fitScore(rows [][]float64) []float64 {
	n := len(rows)
	trees := d.Trees
	if trees <= 0 {
		trees = 100
	}
	psi := d.SampleSize
	if psi <= 0 {
		psi = 256
	}
	if psi > n {
		psi = n
	}
	hlim := int(math.Ceil(math.Log2(float64(maxInt(psi, 2)))))
	rng := rand.New(rand.NewSource(d.Seed + 12345))

	forest := make([]*iNode, trees)
	for t := 0; t < trees; t++ {
		sample := make([][]float64, psi)
		perm := rng.Perm(n)
		for i := 0; i < psi; i++ {
			sample[i] = rows[perm[i]]
		}
		forest[t] = buildTree(sample, 0, hlim, rng)
	}

	c := avgPathLength(float64(psi))
	scores := make([]float64, n)
	for i, r := range rows {
		sum := 0.0
		for _, tr := range forest {
			sum += pathLength(tr, r, 0)
		}
		scores[i] = math.Pow(2, -(sum/float64(trees))/c)
	}
	return scores
}

func buildTree(rows [][]float64, depth, hlim int, rng *rand.Rand) *iNode {
	if depth >= hlim || len(rows) <= 1 {
		return &iNode{size: len(rows)}
	}
	dim := len(rows[0])
	// pick an attribute that actually varies
	attr := -1
	var lo, hi float64
	for try := 0; try < dim; try++ {
		a := rng.Intn(dim)
		lo, hi = rows[0][a], rows[0][a]
		for _, r := range rows {
			if r[a] < lo {
				lo = r[a]
			}
			if r[a] > hi {
				hi = r[a]
			}
		}
		if hi > lo {
			attr = a
			break
		}
	}
	if attr == -1 {
		return &iNode{size: len(rows)}
	}
	sv := lo + rng.Float64()*(hi-lo)
	var L, R [][]float64
	for _, r := range rows {
		if r[attr] < sv {
			L = append(L, r)
		} else {
			R = append(R, r)
		}
	}
	return &iNode{
		splitAttr:  attr,
		splitValue: sv,
		left:       buildTree(L, depth+1, hlim, rng),
		right:      buildTree(R, depth+1, hlim, rng),
	}
}

func pathLength(nd *iNode, r []float64, depth int) float64 {
	if nd.left == nil {
		return float64(depth) + avgPathLength(float64(nd.size))
	}
	if r[nd.splitAttr] < nd.splitValue {
		return pathLength(nd.left, r, depth+1)
	}
	return pathLength(nd.right, r, depth+1)
}

// avgPathLength is c(n): expected path length of an unsuccessful BST search.
func avgPathLength(n float64) float64 {
	if n <= 1 {
		return 0
	}
	if n == 2 {
		return 1
	}
	h := math.Log(n-1) + 0.5772156649
	return 2*h - 2*(n-1)/n
}
