package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubEngine answers one streaming completion and records every request body
// it saw, in order.
func stubEngine(t *testing.T, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*seen = append(*seen, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// countImages counts image parts across a recorded request. History carries
// the picture along, so the interesting number is "exactly one", not "zero on
// later turns".
func countImages(body map[string]any) int {
	n := 0
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		if parts, ok := m.(map[string]any)["content"].([]any); ok {
			for _, p := range parts {
				if _, ok := p.(map[string]any)["image_url"]; ok {
					n++
				}
			}
		}
	}
	return n
}

// parts returns the content parts of the nth message, and whether it was text-only.
func parts(t *testing.T, body map[string]any, n int) ([]any, bool) {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) <= n {
		t.Fatalf("only %d messages", len(msgs))
	}
	content := msgs[n].(map[string]any)["content"]
	if _, ok := content.(string); ok {
		return nil, true
	}
	return content.([]any), false
}

// An attached image must travel as a data URL whose MIME type was SNIFFED from
// the bytes. The first cut hardcoded image/png, which mislabels every JPEG,
// WebP and GIF — and a model that validates the declared type rejects (or
// silently mis-decodes) the attachment.
func TestAttachedImageCarriesItsSniffedMIME(t *testing.T) {
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x00}, 600)...)
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x00}, 600)...)
	for _, tc := range []struct {
		name string
		b    []byte
		mime string
	}{
		{"shot.jpg", jpeg, "image/jpeg"},
		{"shot.png", png, "image/png"},
	} {
		path := filepath.Join(t.TempDir(), tc.name)
		if err := os.WriteFile(path, tc.b, 0o600); err != nil {
			t.Fatal(err)
		}
		var seen []map[string]any
		srv := stubEngine(t, &seen)
		s := New(Options{Base: srv.URL, Image: path})
		if _, err := s.Send(context.Background(), "what is in this?", new(bytes.Buffer)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		srv.Close()

		list, textOnly := parts(t, seen[0], 0)
		if textOnly {
			t.Fatalf("%s: the attachment vanished — content is plain text", tc.name)
		}
		if len(list) != 2 {
			t.Fatalf("%s: want text + image parts, got %d", tc.name, len(list))
		}
		url := list[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(url, "data:"+tc.mime+";base64,") {
			t.Fatalf("%s: data URL declares the wrong type: %.40s (want data:%s;...)", tc.name, url, tc.mime)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.SplitN(url, ",", 2)[1])
		if err != nil || !bytes.Equal(raw, tc.b) {
			t.Fatalf("%s: payload did not survive the round trip (%v)", tc.name, err)
		}
	}
}

// The picture belongs to the question, not to the session: later turns must
// not re-upload it.
func TestAttachedImageIsSentOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x00}, 600)...), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []map[string]any
	srv := stubEngine(t, &seen)
	defer srv.Close()
	s := New(Options{Base: srv.URL, Image: path})
	if _, err := s.Send(context.Background(), "first", new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "second", new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("want 2 requests, got %d", len(seen))
	}
	if n := countImages(seen[0]); n != 1 {
		t.Fatalf("first turn carried %d image parts, want 1", n)
	}
	if n := countImages(seen[1]); n != 1 {
		t.Fatalf("second turn re-uploaded the image (%d parts in the history)", n)
	}
	if _, textOnly := parts(t, seen[1], 2); !textOnly {
		t.Fatalf("the follow-up question is not plain text")
	}
}
