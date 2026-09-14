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

// MaxGauge returns the LARGEST value across a family's series — for epoch-like
// gauges (process_start_time_seconds), where summing the several worker series
// would be meaningless: the engine has been serving since the LAST of them
// started, and an earlier sibling's start would overstate it.
func (m Metrics) MaxGauge(name string) (float64, bool) {
	f, ok := m.Families[name]
	if !ok {
		return 0, false
	}
	best, found := 0.0, false
	for _, mm := range f.GetMetric() {
		v := 0.0
		switch {
		case mm.Gauge != nil:
			v = mm.Gauge.GetValue()
		case mm.Counter != nil:
			v = mm.Counter.GetValue()
		default:
			continue
		}
		if !found || v > best {
			best, found = v, true
		}
	}
	return best, found
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

// HistState keeps the recent history of a histogram family so quantiles can be
// taken over a rolling window instead of "since the last scrape" — one scrape
// is ~2 s of traffic on one lane, far too thin to average.
type HistState struct {
	Horizon time.Duration       // window length; 0 = the default below
	rings   map[string][]histAt // name → ascending-by-time snapshots
	last    map[string]bool     // name → a previous scrape was recorded
}

type histAt struct {
	at   time.Time
	full histSnap
}

// NewHistState returns a one-minute rolling window state.
func NewHistState() *HistState {
	return &HistState{Horizon: 60 * time.Second, rings: map[string][]histAt{}}
}

// Reset drops all windows (engine went down; a restart must not rebaseline
// against stale pre-restart cumulatives).
func (hs *HistState) Reset() {
	hs.rings = map[string][]histAt{}
	hs.last = map[string]bool{}
}

// Quantiles returns each requested φ-quantile over the rolling window — all φ
// from the SAME window, which the old per-φ API could not promise: every call
// advanced the stored snapshot, so asking for p50 then p90 measured p90 over an
// empty window. Zeros when the family is absent or this is its first scrape.
func (hs *HistState) Quantiles(m Metrics, name string, phis ...float64) []float64 {
	out := make([]float64, len(phis))
	cur, ok := m.histSnap(name)
	if !ok {
		return out
	}
	horizon := hs.Horizon
	if horizon <= 0 {
		horizon = 60 * time.Second
	}
	now := m.ScrapedAt
	if now.IsZero() {
		now = time.Now()
	}
	if hs.last == nil {
		hs.last = map[string]bool{}
	}
	ring := hs.rings[name]
	switch {
	case !hs.last[name] || len(ring) == 0:
		// First scrape for this family (or the first after a reset): start the
		// ring here rather than hand back an all-zero window, which would blank
		// the panel for a whole horizon after an engine restart.
		ring = []histAt{{at: now, full: cur}}
	case !ring[len(ring)-1].at.Before(now):
		// Stale or repeated scrape: leave the ring alone, so a replay cannot
		// wipe the newest entry or slide the window backwards.
		hs.rings[name] = ring
		if len(ring) < 2 {
			return out
		}
		return quantilesFromRing(ring, now, horizon, phis)
	default:
		ring = append(ring, histAt{at: now, full: cur})
	}
	hs.rings[name] = pruneRing(ring, now, horizon)
	hs.last[name] = true
	if len(hs.rings[name]) < 2 {
		return out
	}
	return quantilesFromRing(hs.rings[name], now, horizon, phis)
}

// quantilesFromRing diffs the window's two endpoints once and draws every φ
// from that single window.
func quantilesFromRing(ring []histAt, now time.Time, horizon time.Duration, phis []float64) []float64 {
	out := make([]float64, len(phis))
	cut := now.Add(-horizon)
	start := 0
	for i := range ring {
		if !ring[i].at.Before(cut) {
			start = i
			break
		}
	}
	if start > len(ring)-2 {
		start = len(ring) - 2
	}
	for i, phi := range phis {
		if v, ok := QuantileWindow(ring[start].full, ring[len(ring)-1].full, phi); ok {
			out[i] = v
		}
	}
	return out
}

// pruneRing keeps the horizon plus one entry outside it: measuring a full
// horizon needs a sample at or before now−horizon, and keeping one outside
// holds that boundary steady instead of snapping the window to now−(horizon+ε)
// every time an entry falls off.
func pruneRing(ring []histAt, now time.Time, horizon time.Duration) []histAt {
	cut := now.Add(-horizon)
	keep := 0
	for i := range ring {
		if !ring[i].at.Before(cut) {
			keep = i
			if i > 0 {
				keep = i - 1
			}
			break
		}
	}
	if keep > 0 {
		ring = append([]histAt(nil), ring[keep:]...)
	}
	return ring
}
