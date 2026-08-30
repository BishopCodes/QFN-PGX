package engineassets

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWriteToDisk exercises the real extraction (this is the path that died
// with ENOENT on the Spark: top-level Dockerfile landed before any dir was
// created).
func TestWriteToDisk(t *testing.T) {
	root := t.TempDir()
	dir, err := WriteToDisk(root)
	if err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}
	if filepath.Base(dir) != "engine" {
		t.Fatalf("dir: %s", dir)
	}
	for _, want := range []string{
		"Dockerfile", "LICENSE",
		"src/vllm_ple_mmap.py", // any one shipped patch file proves src/ walked
		"tools/fp8_convert.py",
	} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(want)))
		if err != nil {
			t.Fatalf("%s missing: %v", want, err)
		}
		if len(b) == 0 {
			t.Fatalf("%s empty", want)
		}
	}
	// The Go embed file itself must NOT be part of the build context.
	if _, err := os.Stat(filepath.Join(dir, "embed.go")); err == nil {
		t.Fatal("embed.go leaked into the extracted context")
	}
}

func TestDigestAndBaseRef(t *testing.T) {
	df, err := FS.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	ref := BaseImageRef()
	if !strings.HasPrefix(ref, "vllm/vllm-openai:") {
		t.Fatalf("base ref: %q", ref)
	}
	d := Digest()
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(d) {
		t.Fatalf("digest: %q", d)
	}
	if !strings.Contains(string(df), d) {
		t.Fatal("digest not the one pinned in the Dockerfile")
	}
}
