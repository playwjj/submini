package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Navidrome's "no cover" WebP (and any failed cover lookup) must reach the
// page as the embedded JPEG placeholder; real covers and other endpoints
// pass through untouched.
func TestPlaceholderCover(t *testing.T) {
	ph, err := iconsFS.ReadFile("web/icons/cover-placeholder.jpg")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "webp":
			w.Header().Set("Content-Type", "image/webp")
			_, _ = w.Write([]byte("RIFFxxxxWEBP"))
		case "missing":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("real-jpeg"))
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = placeholderCover
	front := httptest.NewServer(proxy)
	defer front.Close()

	cases := []struct {
		path, wantType string
		wantBody       []byte
	}{
		{"/rest/getCoverArt.view?id=webp", "image/jpeg", ph},
		{"/rest/getCoverArt?id=missing", "image/jpeg", ph},
		{"/rest/getCoverArt.view?id=real", "image/jpeg", []byte("real-jpeg")},
		{"/rest/stream.view?id=webp", "image/webp", []byte("RIFFxxxxWEBP")},
	}
	for _, c := range cases {
		resp, err := http.Get(front.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK && c.path != "/rest/stream.view?id=webp" {
			t.Errorf("%s: status %d", c.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != c.wantType {
			t.Errorf("%s: content-type %q, want %q", c.path, got, c.wantType)
		}
		if !bytes.Equal(body, c.wantBody) {
			t.Errorf("%s: body mismatch (%d bytes)", c.path, len(body))
		}
	}
}

// Genuine covers are cached in memory AND persisted under the data
// dir, so a restart serves them from disk without asking the upstream
// again; swapped placeholders are never persisted.
func TestCoverDiskCache(t *testing.T) {
	ph, err := iconsFS.ReadFile("web/icons/cover-placeholder.jpg")
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Query().Get("id") == "webp" {
			w.Header().Set("Content-Type", "image/webp")
			_, _ = w.Write([]byte("RIFFxxxxWEBP"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(ph)
	}))
	defer upstream.Close()

	dir := filepath.Join(t.TempDir(), "covers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := covers
	defer func() { covers = old }()
	covers = &coverStore{items: make(map[string]*coverEntry), dir: dir}
	tr := coverTransport{base: http.DefaultTransport}
	get := func(id string) []byte {
		req := httptest.NewRequest("GET", upstream.URL+"/rest/getCoverArt.view?id="+id+"&size=100", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return body
	}

	if body := get("real"); !bytes.Equal(body, ph) {
		t.Fatalf("first fetch: %d bytes, want the upstream cover", len(body))
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
	// Simulated restart: fresh memory, same disk dir.
	covers = &coverStore{items: make(map[string]*coverEntry), dir: dir}
	if body := get("real"); !bytes.Equal(body, ph) {
		t.Fatalf("after restart: %d bytes, want the disk-cached cover", len(body))
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits after restart = %d, want 1 (disk hit)", n)
	}
	// A swapped placeholder must not be persisted.
	_ = get("webp")
	entries, _ := os.ReadDir(dir)
	imgFiles := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".img" {
			imgFiles++
		}
	}
	if imgFiles != 1 {
		t.Fatalf("disk cache holds %d files, want exactly 1 (the real cover)", imgFiles)
	}
}
