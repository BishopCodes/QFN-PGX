package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Both φ of one family must come from the SAME window: the old per-φ call
// advanced the window on the first call, so p90 never saw samples. Regression
// test for "TTFT p50/p90 never shows a value".
func TestQuantilesShareOneWindowPerScrape(t *testing.T) {
	m1, _ := ParseMetricsText(vllmFixture1)
	m2, _ := ParseMetricsText(vllmFixture2)
	hs := NewHistState()
	if q := hs.Quantiles(m1, "vllm:time_to_first_token_seconds", 0.5, 0.9); q[0] != 0 || q[1] != 0 {
		t.Fatalf("first scrape must have no window: %v", q)
	}
	q := hs.Quantiles(m2, "vllm:time_to_first_token_seconds", 0.5, 0.9)
	// window: 5 obs in (1, 2.5] → p50 = 1.75, p90 = 1 + 1.5*4.5/5 = 2.35
	if q[0] <= 0 || q[1] <= q[0] {
		t.Fatalf("p50/p90 from one window: %+v", q)
	}
	if q[0] < 1.74 || q[0] > 1.76 || q[1] < 2.34 || q[1] > 2.36 {
		t.Fatalf("quantile math off: %v (want ~1.75/2.35)", q)
	}
}

// The window must slide toward now−Horizon (never grow unbounded) and keep
// yielding quantiles for bursts older than a single scrape interval.
func TestQuantileHorizonSlides(t *testing.T) {
	hs := NewHistState()
	base := time.Now().Add(-10 * time.Minute)
	scrape := func(n int) Metrics {
		m, err := ParseMetricsText(fmt.Sprintf(
			"# TYPE vllm:time_to_first_token_seconds histogram\n"+
				"vllm:time_to_first_token_seconds_bucket{le=\"0.5\"} 0\n"+
				"vllm:time_to_first_token_seconds_bucket{le=\"1\"} %d\n"+
				"vllm:time_to_first_token_seconds_bucket{le=\"+Inf\"} %d\n"+
				"vllm:time_to_first_token_seconds_count %d\n"+
				"# TYPE process_start_time_seconds gauge\n"+
				"process_start_time_seconds %d\n", n, n, n, base.Unix()))
		if err != nil {
			t.Fatal(err)
		}
		m.ScrapedAt = base.Add(time.Duration(n) * 10 * time.Second)
		return m
	}
	for i := 1; i < 40; i++ {
		q := hs.Quantiles(scrape(i), "vllm:time_to_first_token_seconds", 0.5)
		if i == 1 {
			continue // first scrape only seeds the ring
		}
		if q[0] <= 0 {
			t.Fatalf("scrape %d: window must yield a quantile (%v)", i, q)
		}
	}
	if n := len(hs.rings["vllm:time_to_first_token_seconds"]); n > 9 {
		t.Fatalf("ring not pruned to the horizon: %d entries", n)
	}
}

// Uptime = now − the YOUNGEST process_start_time_seconds: an engine core
// respawned inside a live container must reset the clock, not inherit the API
// server's older start. A start that cannot be trusted (epoch-zero, or in the
// future) reports no uptime at all rather than a wrong number.
func TestEngineUptimeFromProcessStart(t *testing.T) {
	scrape := time.Now()
	for _, tc := range []struct {
		name string
		body string
		want float64
	}{
		{"youngest wins", fmt.Sprintf("process_start_time_seconds{process_name=\"engine\"} %d\n"+
			"process_start_time_seconds{process_name=\"api\"} %d\n",
			scrape.Add(-2*time.Hour).Unix(), scrape.Add(-2*time.Hour-30*time.Second).Unix()), 7200},
		{"epoch-zero start is not an uptime", "process_start_time_seconds 0\n", 0},
		// 1970-01-12: a relative counter wearing a gauge's clothes. Without a
		// plausible-year floor this reads as ~56 years of uptime.
		{"pre-2001 start is not an uptime", "process_start_time_seconds 1000000\n", 0},
		{"future start is not an uptime", fmt.Sprintf("process_start_time_seconds %d\n",
			scrape.Add(1*time.Hour).Unix()), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMetricsText("# TYPE process_start_time_seconds gauge\n" + tc.body)
			if err != nil {
				t.Fatal(err)
			}
			m.ScrapedAt = scrape
			c := &Collector{hist: NewHistState()}
			var snap Snapshot
			c.fillEngine(&snap, m, 2)
			if tc.want == 0 {
				if snap.Engine.UptimeS != 0 {
					t.Fatalf("uptime %f, want 0 (no trustworthy start)", snap.Engine.UptimeS)
				}
				return
			}
			if snap.Engine.UptimeS < tc.want-10 || snap.Engine.UptimeS > tc.want+10 {
				t.Fatalf("uptime %f, want ~%f", snap.Engine.UptimeS, tc.want)
			}
		})
	}
}

// A single dropped /metrics scrape must NOT wipe the histogram window (the
// ring now spans up to a minute, so resetting on one blip would blank
// TTFT/ITL for that whole horizon). A sustained outage still resets.
func TestHistSurvivesOneBlip(t *testing.T) {
	proc := map[string]string{
		"/proc/meminfo": meminfoFixture,
		"/proc/vmstat":  "pswpin 100\npswpout 50\npgmajfault 7\n",
		"/proc/stat":    "cpu  1000 0 500 8000 500 0 0 0\ncpu0 1000 0 500 8000 500 0 0 0\n",
		"/proc/loadavg": "2.75 2.50 2.00 1/234 5678\n",
	}
	cur := vllmFixture1
	fail := false
	c := New(Config{
		EngineBase:    func() string { return "http://127.0.0.1:18300" },
		EngineKey:     func() string { return "k" },
		ContainerName: "qwen38-flash",
		HFCacheHost:   t.TempDir(),
		Interval:      time.Second,
	}, IO{
		ReadFile: func(path string) ([]byte, error) {
			s, ok := proc[path]
			if !ok {
				return nil, &notFoundErr{}
			}
			return []byte(s), nil
		},
		StatFreeKB:  func(string) (uint64, bool) { return 12345, true },
		GPU:         func(context.Context) GPU { return GPU{} },
		ContainerID: func(context.Context) (string, error) { return "", nil },
		Scrape: func(ctx context.Context, url, bearer string) (string, error) {
			if fail {
				return "", errors.New("connection refused")
			}
			return cur, nil
		},
	})
	sample := func() Snapshot { return c.SampleOnce(context.Background()) }
	// 1) seed, 2) first window.
	sample()
	cur = vllmFixture2
	if s := sample(); s.Engine.TTFTP50 <= 0 {
		t.Fatalf("window must open on the second scrape: %+v", s.Engine)
	}
	// 3) ONE dropped scrape, 4) the window must still be there.
	fail = true
	if s := sample(); s.Engine.Reachable {
		t.Fatal("blip must report unreachable")
	}
	fail = false
	if s := sample(); s.Engine.TTFTP50 <= 0 {
		t.Fatalf("one dropped scrape must not blank the quantiles: %+v", s.Engine)
	}
	// 5-7) a sustained outage resets: the next successful scrape has no window.
	fail = true
	for i := 0; i < 8 && c.metFail < 3; i++ {
		sample()
	}
	fail = false
	cur = vllmFixture1
	for i := 0; i < 12 && !sample().Engine.Reachable; i++ { // dead-engine backoff skips ticks
	}
	if s := c.SampleOnce(context.Background()); s.Engine.TTFTP50 != 0 || s.Engine.TTFTP90 != 0 {
		t.Fatalf("a 3-sample outage must reset the windows: %+v", s.Engine)
	}
	cur = vllmFixture2
	if s := sample(); s.Engine.TTFTP50 <= 0 {
		t.Fatalf("windows must rebuild after a reset: %+v", s.Engine)
	}
}

func TestHistResetClearsWindows(t *testing.T) {
	m1, _ := ParseMetricsText(vllmFixture1)
	m2, _ := ParseMetricsText(vllmFixture2)
	hs := NewHistState()
	hs.Quantiles(m1, "vllm:time_to_first_token_seconds", 0.5)
	hs.Reset()
	if q := hs.Quantiles(m2, "vllm:time_to_first_token_seconds", 0.5); q[0] != 0 {
		t.Fatal("after Reset the first scrape must have no window again")
	}
}
