package main

import (
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBingCaption(t *testing.T) {
	if got := bingCaption("亚伯拉罕湖，艾伯塔省，加拿大 (© Guang Cao/Getty Images)"); got != "亚伯拉罕湖，艾伯塔省，加拿大" {
		t.Errorf("caption = %q", got)
	}
	if got := bingCaption("Some place"); got != "Some place" {
		t.Errorf("caption without credit = %q", got)
	}
	var br bingResponse
	fixture := `{"images":[{"url":"/th?id=OHR.X_1920x1080.jpg","copyright":"湖光山色 (© A/B)"}]}`
	if err := json.Unmarshal([]byte(fixture), &br); err != nil || len(br.Images) != 1 {
		t.Fatalf("bing fixture parse: %v", err)
	}
}

func TestDownscale(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2048, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 2048; x++ {
			src.Set(x, y, color.RGBA{200, 100, 50, 255})
		}
	}
	out := downscale(src, 1024)
	if b := out.Bounds(); b.Dx() != 1024 || b.Dy() != 512 {
		t.Fatalf("downscaled bounds = %v, want 1024x512", b)
	}
	r, g, b, _ := out.At(500, 250).RGBA()
	if r>>8 != 200 || g>>8 != 100 || b>>8 != 50 {
		t.Errorf("pixel = (%d,%d,%d), want the flat source colour", r>>8, g>>8, b>>8)
	}
	if downscale(src, 4096) != image.Image(src) {
		t.Errorf("small image must be returned unchanged")
	}
}

func TestStoreSaverImage(t *testing.T) {
	dir := t.TempDir()
	// a real embedded starter (1024x810 JPEG, landscape) is accepted
	data, err := saverFS.ReadFile("web/screensaver/starry-night.jpg")
	if err != nil {
		t.Fatal(err)
	}
	name, ok := storeSaverImage(dir, "https://example.com/a.jpg", data)
	if !ok || !strings.HasPrefix(name, "ss-") {
		t.Fatalf("store landscape jpeg: ok=%v name=%q", ok, name)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
	// undecodable bytes are rejected
	if _, ok := storeSaverImage(dir, "https://example.com/b.jpg", []byte("not an image")); ok {
		t.Errorf("garbage bytes must be rejected")
	}
}

func TestSaverStarters(t *testing.T) {
	items := saverStarters()
	if len(items) < 8 {
		t.Fatalf("starters = %d, want the bundled set", len(items))
	}
	for _, it := range items {
		if !strings.HasPrefix(it.Src, "/screensaver/starter/") || it.Caption == "" {
			t.Errorf("bad starter item: %+v", it)
		}
	}
}
