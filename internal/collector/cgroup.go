package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupStats reads cgroup v2 files directly for the engine container —
// cheaper than `docker stats` per second and it keeps working while the box
// is thrashing. Falls back to absent when the scope path differs (non-systemd
// cgroup drivers): callers just omit the panel.
type CgroupStats struct {
	MemCurrent uint64  `json:"mem_current"` // bytes (RSS+cache charged to container)
	CPUUsec    uint64  `json:"cpu_usec"`    // cumulative, for delta → pct
	ReadBytes  uint64  `json:"read_bytes"`  // cumulative block reads (PLE traffic!)
	WriteBytes uint64  `json:"write_bytes"`
	Found      bool    `json:"found"`
}

// cgroupRoot is a var so tests can point at a fake tree.
var cgroupRoot = "/sys/fs/cgroup"

// cgroupPath builds the systemd-slice scope path for a docker container id.
func cgroupPath(id string) string {
	return filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope")
}

// readCgroup samples the container scope; Found=false when anything is
// missing (caller hides the panel rather than showing zeros).
func readCgroup(id string) CgroupStats {
	dir := cgroupPath(id)
	st := CgroupStats{}
	if v, ok := readFileUint(filepath.Join(dir, "memory.current")); ok {
		st.MemCurrent = v
		st.Found = true
	}
	if v, ok := readKV(filepath.Join(dir, "cpu.stat"), "service_usec"); ok {
		st.CPUUsec = v
	}
	// io.stat: "8:16 rbytes=123456 wbytes=0 rios=0 wios=0" per device line;
	// sum rbytes/wbytes across devices.
	if b, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			for _, kv := range strings.Fields(line) {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "rbytes":
					st.ReadBytes += n
				case "wbytes":
					st.WriteBytes += n
				}
			}
		}
		st.Found = true
	}
	return st
}

func readFileUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

func readKV(path, key string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			v, err := strconv.ParseUint(f[1], 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}
