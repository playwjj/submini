package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
