package collector

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is the GB10 telemetry slice that NVML/nvidia-smi *can* report on
// unified memory: utilization, power, temperature, clocks. GPU memory is
// deliberately NOT taken from here — it is the host unified pool (meminfo) —
// because nvidia-smi's memory reporting is broken on GB10.
type GPU struct {
	Available   bool    `json:"available"`
	Source      string  `json:"source,omitempty"`     // "nvidia-smi"
	UtilPct     float64 `json:"util_pct"`
	PowerW      float64 `json:"power_w"`
	TempC       float64 `json:"temp_c"`
	ClockMHz    float64 `json:"clock_mhz"`
	MaxPowerW   float64 `json:"max_power_w,omitempty"`
}

// gpuQuery shells out to nvidia-smi (present on the Spark; NVML Go bindings
// would require cgo and break the static-binary goal).
type gpuQuery struct {
	bin     string
	timeout time.Duration
}

func newGPUQuery() *gpuQuery { return &gpuQuery{bin: "nvidia-smi", timeout: 1500 * time.Millisecond} }

// sample returns GPU state; Available=false when nvidia-smi is missing or
// errors (dev boxes without a GPU keep the dashboard honest).
func (g *gpuQuery) sample(ctx context.Context) GPU {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.bin,
		"--query-gpu=utilization.gpu,power.draw,temperature.gpu,clocks.sm,power.limit",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return GPU{}
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return GPU{}
	}
	// first GPU only (the Spark has one)
	line = strings.SplitN(line, "\n", 2)[0]
	f := strings.Split(line, ",")
	if len(f) < 4 {
		return GPU{}
	}
	pf := func(s string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return v
	}
	return GPU{
		Available: true,
		Source:    "nvidia-smi",
		UtilPct:   pf(f[0]),
		PowerW:    pf(f[1]),
		TempC:     pf(f[2]),
		ClockMHz:  pf(f[3]),
		MaxPowerW: func() float64 {
			if len(f) > 4 {
				return pf(f[4])
			}
			return 0
		}(),
	}
}
