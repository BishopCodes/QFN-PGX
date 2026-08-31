package cli

import (
	"bufio"
	"os"
	"testing"
)

// withPipedStdin swaps the package input for an os.Pipe (never a tty, so
// AskChoice must take the legacy line path) and mutes stdout; both are
// restored via t.Cleanup.
func withPipedStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldStdin, oldStdout := stdin, os.Stdin, os.Stdout
	stdin = bufio.NewReader(r)
	os.Stdin = r
	os.Stdout = devnull
	t.Cleanup(func() {
		stdin, os.Stdin, os.Stdout = oldIn, oldStdin, oldStdout
		r.Close()
		devnull.Close()
	})
}

func TestParseChoiceIndex(t *testing.T) {
	tests := []struct {
		txt    string
		n      int
		defIdx int
		want   int
		ok     bool
	}{
		{"2\n", 3, 0, 1, true},   // number → index
		{"3", 3, 0, 2, true},     // upper bound inclusive
		{"\n", 3, 2, 2, true},    // empty → default
		{"   \n", 3, 1, 1, true}, // whitespace-only → default
		{"9\n", 3, 0, 0, false},  // out of range high
		{"0\n", 3, 0, 0, false},  // out of range low
		{"foo\n", 3, 0, 0, false},
		{"1x\n", 3, 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := parseChoiceIndex(tt.txt, tt.n, tt.defIdx)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseChoiceIndex(%q, %d, %d) = %d, %v; want %d, %v",
				tt.txt, tt.n, tt.defIdx, got, ok, tt.want, tt.ok)
		}
	}
}

func TestAskChoicePipePicksNumber(t *testing.T) {
	withPipedStdin(t, "2\n")
	got, err := AskChoice("mode", []string{"a", "b", "c"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
}

func TestAskChoicePipeEmptyTakesDefault(t *testing.T) {
	withPipedStdin(t, "\n")
	got, err := AskChoice("mode", []string{"a", "b", "c"}, 2, "some hint")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("got %d, want 2 (default)", got)
	}
}

func TestAskChoicePipeRepromptsOnGarbage(t *testing.T) {
	withPipedStdin(t, "9\nfoo\n3\n")
	got, err := AskChoice("mode", []string{"a", "b", "c"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("got %d, want 2 after two bad lines", got)
	}
}

func TestAskChoicePipeEOFErrors(t *testing.T) {
	withPipedStdin(t, "")
	if _, err := AskChoice("mode", []string{"a", "b"}, 0, ""); err == nil {
		t.Fatal("EOF with no input must return an error")
	}
}
