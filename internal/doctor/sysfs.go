package doctor

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type memInfo struct {
	MemTotalKiB, MemAvailableKiB, SwapTotalKiB, SwapFreeKiB uint64
}

func readMemInfo() (memInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memInfo{}, err
	}
	defer f.Close()
	var m memInfo
	sc := bufio.NewScanner(f)
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
		}
	}
	return m, sc.Err()
}

func defaultStatFreeKB(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize) / 1024, true
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// ServiceState reports systemd unit state (needs no docker).
func ServiceState(ctx any, units ...string) []Check {
	if len(units) == 0 {
		units = []string{"qfn-serve", "qfn-engine"}
	}
	var out []Check
	for _, u := range units {
		cmd := exec.Command("systemctl", "is-enabled", u)
		b, err := cmd.Output()
		state := strings.TrimSpace(string(b))
		if err != nil {
			state = "disabled/absent"
		}
		status := "ok"
		if state == "disabled/absent" {
			status = "warn"
		}
		out = append(out, Check{ID: "service." + u, Status: status,
			Msg: u + ": " + state, Hint: map[bool]string{true: "run `qfn service install`", false: ""}[status == "warn"]})
	}
	return out
}
