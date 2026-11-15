package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
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

func TestMergeSaverIndex(t *testing.T) {
	old := []saverFile{{File: "ss-a.jpg", Caption: "A"}, {File: "ss-b.jpg", Caption: "B"}, {File: "ss-c.jpg", Caption: "C"}}
	fresh := []saverFile{{File: "ss-d.jpg", Caption: "D"}, {File: "ss-b.jpg", Caption: "B2"}}
	got := mergeSaverIndex(old, fresh, 24)
	if len(got) != 4 || got[0].File != "ss-d.jpg" || got[1].File != "ss-b.jpg" || got[1].Caption != "B2" || got[2].File != "ss-a.jpg" || got[3].File != "ss-c.jpg" {
		t.Errorf("merge = %+v", got)
	}
	if got := mergeSaverIndex(old, fresh, 2); len(got) != 2 || got[1].File != "ss-b.jpg" {
		t.Errorf("capped merge = %+v", got)
	}
	if got := mergeSaverIndex(nil, fresh, 24); len(got) != 2 {
		t.Errorf("merge into empty = %+v", got)
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

// testArt builds a w*h picture from a per-pixel fill.
func testArt(w, h int, fill func(x, y int) color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill(x, y))
		}
	}
	return img
}

func testJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// black ink shapes on yellow paper, like a zincograph print
func duoArt() *image.RGBA {
	return testArt(800, 500, func(x, y int) color.Color {
		if x%97 < 9 || y%61 < 7 {
			return color.RGBA{20, 20, 20, 255}
		}
		return color.RGBA{240, 200, 60, 255}
	})
}

// six saturated hue bands
func bandArt() *image.RGBA {
	bands := []color.RGBA{
		{200, 60, 60, 255}, {200, 200, 60, 255}, {60, 200, 60, 255},
		{60, 200, 200, 255}, {60, 60, 200, 255}, {200, 60, 200, 255},
	}
	return testArt(800, 500, func(x, y int) color.Color {
		return bands[x*len(bands)/800]
	})
}

func TestSaverColorOK(t *testing.T) {
	if saverColorOK(duoArt()) {
		t.Error("duotone yellow-paper print must be rejected")
	}
	gray := testArt(800, 500, func(x, y int) color.Color {
		v := uint8(40 + x*180/800)
		return color.RGBA{v, v, v, 255}
	})
	if saverColorOK(gray) {
		t.Error("grayscale gradient must be rejected")
	}
	if !saverColorOK(bandArt()) {
		t.Error("multi-hue picture must pass")
	}
	muted := testArt(800, 500, func(x, y int) color.Color {
		switch {
		case y < 200:
			return color.RGBA{150, 170, 190, 255} // pale sky
		case y < 350:
			return color.RGBA{160, 180, 150, 255} // pale green
		default:
			return color.RGBA{190, 180, 150, 255} // pale sand
		}
	})
	if !saverColorOK(muted) {
		t.Error("muted multi-hue landscape must pass")
	}
	// every bundled starter must pass the gate it will never face
	// (starters are curated) — this pins the thresholds against the
	// most single-hued real paintings in the set, e.g. the Temeraire.
	entries, err := saverFS.ReadDir("web/screensaver")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jpg") {
			continue
		}
		data, err := saverFS.ReadFile("web/screensaver/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if !saverColorOK(img) {
			t.Errorf("starter %s must pass the colour gate", e.Name())
		}
		checked++
	}
	if checked < 8 {
		t.Errorf("only %d starters checked", checked)
	}
}

func TestStoreSaverImageColorGate(t *testing.T) {
	dir := t.TempDir()
	if _, ok := storeSaverImage(dir, "https://example.com/duo.jpg", testJPEG(t, duoArt())); ok {
		t.Error("duotone print must not be stored")
	}
	name, ok := storeSaverImage(dir, "https://example.com/bands.jpg", testJPEG(t, bandArt()))
	if !ok || name == "" {
		t.Fatal("multi-hue picture must be stored")
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
}

func TestPruneSaverIndex(t *testing.T) {
	dir := t.TempDir()
	duo := testJPEG(t, duoArt())
	bands := testJPEG(t, bandArt())
	if err := os.WriteFile(filepath.Join(dir, "ss-duo.jpg"), duo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ss-bands.jpg"), bands, 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _ := json.Marshal(map[string]interface{}{"images": []saverFile{
		{File: "ss-duo.jpg", Caption: "print", Kind: "art"},
		{File: "ss-bands.jpg", Caption: "bands", Kind: "art"},
	}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	pruneSaverIndex(dir)
	items := readSaverIndex(dir)
	if len(items) != 1 || items[0].File != "ss-bands.jpg" {
		t.Fatalf("after prune: %+v", items)
	}
	if _, err := os.Stat(filepath.Join(dir, "ss-duo.jpg")); !os.IsNotExist(err) {
		t.Error("duotone file must be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "ss-bands.jpg")); err != nil {
		t.Error("colourful file must stay")
	}
	// a second prune is a no-op
	pruneSaverIndex(dir)
	if items := readSaverIndex(dir); len(items) != 1 {
		t.Fatalf("second prune changed the set: %+v", items)
	}
}
