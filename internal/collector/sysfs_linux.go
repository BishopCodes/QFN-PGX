package collector

import "syscall"

// statfsFreeKB returns free KiB on the filesystem hosting path.
func statfsFreeKB(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	bfree := st.Bavail * uint64(st.Bsize) //nolint:unconvert // Bsize type varies per arch
	return bfree / 1024, true
}
