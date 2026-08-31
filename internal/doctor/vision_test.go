package doctor

import (
	"context"
	"errors"
	"testing"
)

func TestVisionCheckClassification(t *testing.T) {
	cases := []struct {
		name     string
		deps     VisionDeps
		wantStat string
		wantSub  string
	}{
		{"ok", VisionDeps{Args: []string{"--limit-mm-per-prompt", `{"image": 4}`}, SnapshotHasVision: true,
			Post: fakePost(200, `{"choices":[{}]}`, nil)}, "ok", "round-trip"},
		{"no vision config", VisionDeps{SnapshotHasVision: false}, "warn", "no vision_config"},
		{"engine down", VisionDeps{SnapshotHasVision: true, Args: nil}, "warn", "not running"},
		{"flag missing", VisionDeps{SnapshotHasVision: true, Args: []string{"--max-model-len", "262144"}}, "bad", "without"},
		{"limit zero", VisionDeps{SnapshotHasVision: true, Args: []string{"--limit-mm-per-prompt", `{"image": 0}`},
			Post: fakePost(400, `At most 0 image(s) as input`, nil)}, "bad", "flag"},
		{"build predates mm", VisionDeps{SnapshotHasVision: true, Args: []string{"--limit-mm-per-prompt", `{"image": 4}`},
			Post: fakePost(400, `ValueError: model does not support modality 'image'`, nil)}, "bad", "predates"},
		{"other error", VisionDeps{SnapshotHasVision: true, Args: []string{"--limit-mm-per-prompt", "x"},
			Post: fakePost(500, `internal blast`, nil)}, "warn", "500"},
		{"conn refused", VisionDeps{SnapshotHasVision: true, Args: []string{"--limit-mm-per-prompt", "x"},
			Post: fakePost(0, "", errors.New("connection refused"))}, "warn", "reach"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.deps.Base == nil {
				tc.deps.Base = func() string { return "http://127.0.0.1:18300" }
			}
			if tc.deps.Key == nil {
				tc.deps.Key = func() string { return "k" }
			}
			ch := VisionCheck(context.Background(), tc.deps)
			if ch.Status != tc.wantStat {
				t.Errorf("status = %s, want %s (%s)", ch.Status, tc.wantStat, ch.Msg)
			}
			if !bodyHas(ch.Msg, tc.wantSub) && !bodyHas(ch.Hint, tc.wantSub) {
				t.Errorf("msg/hint missing %q: %s / %s", tc.wantSub, ch.Msg, ch.Hint)
			}
		})
	}
}

func fakePost(code int, body string, err error) func(context.Context, string, string, []byte) (int, string, error) {
	return func(context.Context, string, string, []byte) (int, string, error) { return code, body, err }
}
