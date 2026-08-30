// engineassets embeds the vendored engine build context (engine/ATTRIBUTION.md)
// so the single static binary can rebuild the image and prepare the hybrid
// checkpoint on the Spark without this repo checked out there.
package engineassets

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed all:src all:tools Dockerfile LICENSE
var FS embed.FS

// Digest is the pinned base-image digest parsed from the Dockerfile.
func Digest() string {
	df, err := FS.ReadFile("Dockerfile")
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^FROM\s+\S+@(sha256:[0-9a-f]{64})`).FindStringSubmatch(string(df))
	if m == nil {
		return ""
	}
	return m[1]
}

// BaseImageRef returns the full pinned base reference (image:tag@sha256:…).
func BaseImageRef() string {
	df, err := FS.ReadFile("Dockerfile")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(df), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "FROM "); ok {
			return strings.Fields(v)[0]
		}
	}
	return ""
}

// WriteToDisk extracts the build context under root (docker build wants a
// directory); returns the context dir.
func WriteToDisk(root string) (string, error) {
	dir := filepath.Join(root, "engine")
	// The walk root "." is skipped below, so the base dir must exist before
	// the first top-level file (lexically: Dockerfile) tries to land in it —
	// without this, extraction dies with ENOENT on the Spark (qfn build /
	// prepare-hybrid).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		full := filepath.Join(dir, p)
		if d.IsDir() {
			return os.MkdirAll(full, 0o755)
		}
		b, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(full, b, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("extracting embedded engine assets: %w", err)
	}
	return dir, nil
}
