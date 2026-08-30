// Package collector samples the machine and the engine once per second:
// host memory/swap/PSI/CPU from /proc (the primary story on a unified-memory
// GB10 where NVML's memory reporting is broken), the engine container's cgroup
// stats, GPU util/power via nvidia-smi, and vLLM's Prometheus metrics folded
// into windowed rates and histogram quantiles.
package collector

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// MemInfo is the subset of /proc/meminfo we surface (all KiB→bytes).
type MemInfo struct {
	MemTotalKiB     uint64 `json:"mem_total_kib"`
	MemAvailableKiB uint64 `json:"mem_available_kib"`
	SwapTotalKiB    uint64 `json:"swap_total_kib"`
	SwapFreeKiB     uint64 `json:"swap_free_kib"`
	CachedKiB       uint64 `json:"cached_kib"` // page cache — what the PLE mmap leans on
}

func parseMemInfo(content string) (MemInfo, error) {
	var m MemInfo
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			m.MemTotalKiB = v
		case "MemAvailable":
			m.MemAvailableKiB = v
		case "SwapTotal":
			m.SwapTotalKiB = v
		case "SwapFree":
			m.SwapFreeKiB = v
		case "Cached":
			m.CachedKiB = v
		}
	}
	return m, sc.Err()
}

// PSI is pressure-stall-info for one resource (percentages of a 1 s window
// averaged over 10/60/300 s). "some" = at least one task stalled; "full" = all
// runnable tasks stalled — the backpressure signal on this box.
type PSI struct {
	SomeAvg10  float64 `json:"some_avg10"`
	SomeAvg60  float64 `json:"some_avg60"`
	SomeAvg300 float64 `json:"some_avg300"`
	FullAvg10  float64 `json:"full_avg10"`
	FullAvg60  float64 `json:"full_avg60"`
	FullAvg300 float64 `json:"full_avg300"`
}

func parsePSI(content string) (PSI, error) {
	var p PSI
	for _, line := range strings.Split(content, "\n") {
		kind := strings.Fields(line)
		if len(kind) < 2 || (kind[0] != "some" && kind[0] != "full") {
			continue
		}
		var a10, a60, a300 float64
		for _, kv := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch k {
			case "avg10":
				a10 = f
			case "avg60":
				a60 = f
			case "avg300":
				a300 = f
			}
		}
		if kind[0] == "some" {
			p.SomeAvg10, p.SomeAvg60, p.SomeAvg300 = a10, a60, a300
		} else {
			p.FullAvg10, p.FullAvg60, p.FullAvg300 = a10, a60, a300
		}
	}
	return p, nil
}

// VMStatRates are per-second deltas computed by the Collector from vmstat
// counters (pswpin/pswpout/pgmajfault are the swap + PLE page-cache signals).
type VMStatRates struct {
	SwapInPerS    float64 `json:"swap_in_per_s"`
	SwapOutPerS   float64 `json:"swap_out_per_s"`
	MajFaultPerS  float64 `json:"maj_fault_per_s"` // PLE mmap thrash if elevated
	PswpinTotal   uint64  `json:"pswpin_total"`
	PswpoutTotal  uint64  `json:"pswpout_total"`
	MajFaultTotal uint64  `json:"pgmajfault_total"`
}

func parseVMStat(content string) (pswpin, pswpout, majfault uint64) {
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "pswpin":
			pswpin = v
		case "pswpout":
			pswpout = v
		case "pgmajfault":
			majfault = v
		}
	}
	return
}

// CPUUsage is a per-core delta computation.
type CPUUsage struct {
	PerCore []float64 `json:"per_core"` // 0..1 busy fraction since last sample
	Total   float64   `json:"total"`
}

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}
func (c cpuTimes) busy() uint64 { return c.total() - c.idle - c.iowait }

func parseProcStat(content string) (cpuTimes, []cpuTimes) {
	var total cpuTimes
	var cores []cpuTimes
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		var t cpuTimes
		dst := []*uint64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal}
		for i := 1; i < 9 && i < len(f); i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			*dst[i-1] = v
		}
		if f[0] == "cpu" {
			total = t
			continue
		}
		// cores arrive as cpu0..cpuN; index positionally
		if idx, err := strconv.Atoi(f[0][3:]); err == nil {
			for len(cores) <= idx {
				cores = append(cores, cpuTimes{})
			}
			cores[idx] = t
		}
	}
	return total, cores
}

// usageFrom computes busy-fraction between two snapshots, tolerant of core
// count changes and counter wraps (diff of 0 when negative → idle).
func usageFrom(prev, cur []cpuTimes) []float64 {
	out := make([]float64, len(cur))
	for i := range cur {
		if i >= len(prev) {
			continue
		}
		dt := int64(cur[i].total()) - int64(prev[i].total())
		db := int64(cur[i].busy()) - int64(prev[i].busy())
		if dt > 0 && db >= 0 {
			out[i] = float64(db) / float64(dt)
		}
	}
	return out
}

// loadAvg reads /proc/loadavg (1-minute).
func loadAvg() (l1 float64, ok bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	return l1, true
}
