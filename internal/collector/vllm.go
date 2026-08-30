package collector

import (
	"bytes"
	"math"
	"sort"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Metrics is one parsed /metrics scrape.
type Metrics struct {
	Families  map[string]*dto.MetricFamily
	ScrapedAt time.Time
}

// ParseMetricsText parses Prometheus exposition text.
func ParseMetricsText(text string) (Metrics, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader([]byte(text)))
	if err != nil {
		return Metrics{}, err
	}
	return Metrics{Families: fams, ScrapedAt: time.Now()}, nil
}

// Gauge returns the summed value across a family's series (single-engine host;
// labels vary only by model).
func (m Metrics) Gauge(name string) (float64, bool) {
	f, ok := m.Families[name]
	if !ok {
		return 0, false
	}
	var sum float64
	found := false
	for _, mm := range f.GetMetric() {
		switch {
		case mm.Gauge != nil:
			sum += mm.Gauge.GetValue()
			found = true
		case mm.Counter != nil:
			sum += mm.Counter.GetValue()
			found = true
		}
	}
	return sum, found
}

// histSnap stores a histogram family as sorted le-bounds with their
// (cumulative, as-exposed) counts summed across series.
type histSnap struct {
	le    []float64
	count []float64
}

func (m Metrics) histSnap(name string) (histSnap, bool) {
	f, ok := m.Families[name]
	if !ok {
		return histSnap{}, false
	}
	perLe := map[float64]float64{}
	found := false
	for _, mm := range f.GetMetric() {
		h := mm.GetHistogram()
		if h == nil {
			continue
		}
		found = true
		for _, b := range h.GetBucket() {
			if math.IsInf(b.GetUpperBound(), 1) {
				continue // +Inf carries no resolvable bound
			}
			perLe[b.GetUpperBound()] += float64(b.GetCumulativeCount())
		}
	}
	if !found || len(perLe) == 0 {
		return histSnap{}, false
	}
	var les []float64
	for le := range perLe {
		les = append(les, le)
	}
	sort.Float64s(les)
	hs := histSnap{le: les, count: make([]float64, len(les))}
	for i, le := range les {
		hs.count[i] = perLe[le]
	}
	return hs, true
}

// QuantileWindow computes the φ-quantile over the window between two
// cumulative bucket snapshots: per-bucket window counts are (cur-le-count −
// prev-at-same-or-lower-le), since both sides are cumulative. A rebuild/reset
// (cur < prev at a bound) rebaselines that bucket to the full current count.
// The walk assumes prev/cur sorted by le and matches each cur bound to the
// greatest prev bound ≤ it (new buckets count fully from 0).
func QuantileWindow(prev, cur histSnap, phi float64) (float64, bool) {
	if len(cur.le) == 0 || !(phi > 0 && phi < 1) {
		return 0, false
	}
	counts := make([]float64, len(cur.le))
	// Window cumulative per bound: cur.cum(le) − prev.cum(greatest prev le ≤ le).
	wcum := make([]float64, len(cur.le))
	j := -1 // prev pointer
	for i, le := range cur.le {
		for j+1 < len(prev.le) && prev.le[j+1] <= le {
			j++
		}
		prevCum := 0.0
		if j >= 0 {
			prevCum = prev.count[j]
		}
		d := cur.count[i] - prevCum
		if d < 0 {
			d = cur.count[i] // reset within this bucket
		}
		wcum[i] = d
	}
	// Enforce monotonicity of the window cumulative (bucket churn or partial
	// resets), then difference it into per-bucket counts.
	for i := 1; i < len(wcum); i++ {
		if wcum[i] < wcum[i-1] {
			wcum[i] = wcum[i-1]
		}
	}
	counts[0] = wcum[0]
	for i := 1; i < len(wcum); i++ {
		counts[i] = wcum[i] - wcum[i-1]
	}
	var total float64
	for _, c := range counts {
		total += c
	}
	if total <= 0 {
		return 0, false
	}
	return interpolate(cur.le, counts, phi), true
}

// interpolate is Prometheus's histogram_quantile interpolation over
// (0, le] buckets with non-cumulative counts.
func interpolate(upperBounds, counts []float64, phi float64) float64 {
	var sum float64
	for _, c := range counts {
		sum += c
	}
	rank := phi * sum
	acc := 0.0
	prevLe := 0.0
	for i, le := range upperBounds {
		next := acc + counts[i]
		if next >= rank {
			if counts[i] <= 0 {
				return le
			}
			return prevLe + (le-prevLe)*(rank-acc)/counts[i]
		}
		acc = next
		prevLe = le
	}
	return upperBounds[len(upperBounds)-1]
}

// HistState keeps previous buckets so each window is computed once per scrape.
type HistState struct {
	prev map[string]histSnap
}

// NewHistState returns an empty window state.
func NewHistState() *HistState { return &HistState{prev: map[string]histSnap{}} }

// Quantile returns the φ-quantile over the window since the previous call for
// this family and advances the stored snapshot. Returns (0,false) on the
// first scrape (no window yet) or when the family is absent.
func (hs *HistState) Quantile(m Metrics, name string, phi float64) (float64, bool) {
	cur, ok := m.histSnap(name)
	if !ok {
		return 0, false
	}
	prev, hadPrev := hs.prev[name]
	hs.prev[name] = cur
	if !hadPrev {
		return 0, false
	}
	return QuantileWindow(prev, cur, phi)
}
