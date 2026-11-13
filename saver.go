package main

// Screensaver pictures: famous paintings and landscapes for the idle
// home-screen app. The page only ever loads pictures from this server
// (same origin, LAN speed, works offline): a starter set of public-
// domain paintings ships inside the binary, and a daily updater adds
// fresh downloads under DATA_DIR/screensaver — Bing's daily wallpapers
// (landscapes) and public-domain works from the Metropolitan Museum's
// Open Access API. (The Art Institute of Chicago's API was the first
// choice for paintings but sits behind a bot challenge; the Met's API
// is open and keyless.) Updates merge into the existing set (newest
// first, capped), so a partially successful day still lands; a day
// with zero new pictures leaves the set untouched.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/screensaver
var saverFS embed.FS

type saverItem struct {
	Src     string `json:"src"`
	Caption string `json:"caption"`
	Kind    string `json:"kind,omitempty"` // "photo" (Bing) or "art" (Met, starters)
}

type saverFile struct {
	File    string `json:"file"`
	Caption string `json:"caption"`
	Kind    string `json:"kind,omitempty"`
}

func saverStarters() []saverItem {
	data, err := saverFS.ReadFile("web/screensaver/starters.json")
	if err != nil {
		return nil
	}
	var man struct {
		Images []saverFile `json:"images"`
	}
	if json.Unmarshal(data, &man) != nil {
		return nil
	}
	out := make([]saverItem, 0, len(man.Images))
	for _, im := range man.Images {
		out = append(out, saverItem{Src: "/screensaver/starter/" + im.File, Caption: im.Caption, Kind: "art"})
	}
	return out
}

func saverDir(dataDir string) string { return filepath.Join(dataDir, "screensaver") }

func saverDownloaded(dataDir string) []saverItem {
	data, err := os.ReadFile(filepath.Join(saverDir(dataDir), "index.json"))
	if err != nil {
		return nil
	}
	var idx struct {
		Images []saverFile `json:"images"`
	}
	if json.Unmarshal(data, &idx) != nil {
		return nil
	}
	out := make([]saverItem, 0, len(idx.Images))
	for _, im := range idx.Images {
		out = append(out, saverItem{Src: "/screensaver/dl/" + im.File, Caption: im.Caption, Kind: im.Kind})
	}
	return out
}

func handleAPIScreensaver(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		images := append(saverDownloaded(dataDir), saverStarters()...)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"images": images})
	}
}

func handleSaverFile(dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/screensaver/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		kind, name := parts[0], parts[1]
		if name != filepath.Base(name) || strings.Contains(name, "..") || !strings.HasSuffix(name, ".jpg") {
			http.NotFound(w, r)
			return
		}
		var data []byte
		var err error
		switch kind {
		case "starter":
			data, err = saverFS.ReadFile("web/screensaver/" + name)
		case "dl":
			data, err = os.ReadFile(filepath.Join(saverDir(dataDir), name))
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}

/* ---------------- daily updater ---------------- */

func startSaverUpdater(dataDir string) {
	go func() {
		time.Sleep(20 * time.Second)
		for {
			runSaverUpdate(dataDir)
			time.Sleep(24 * time.Hour)
		}
	}()
}

func runSaverUpdate(dataDir string) {
	dir := saverDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	client := &http.Client{Transport: outboundTransport(), Timeout: 45 * time.Second}
	var fresh []saverFile
	fresh = append(fresh, fetchBing(dir)...)
	fresh = append(fresh, fetchMet(client, dir)...)
	if len(fresh) == 0 {
		log.Printf("submini: screensaver update found no new pictures, keeping existing set")
		return
	}
	items := mergeSaverIndex(readSaverIndex(dir), fresh, 24)
	idx, _ := json.Marshal(map[string]interface{}{"images": items})
	tmp := filepath.Join(dir, "index.json.tmp")
	if os.WriteFile(tmp, idx, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "index.json"))
	}
	keep := make(map[string]bool, len(items))
	for _, it := range items {
		keep[it.File] = true
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "ss-") && strings.HasSuffix(e.Name(), ".jpg") && !keep[e.Name()] {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	log.Printf("submini: screensaver updated, %d new pictures (set now %d)", len(fresh), len(items))
}

func readSaverIndex(dir string) []saverFile {
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil
	}
	var idx struct {
		Images []saverFile `json:"images"`
	}
	if json.Unmarshal(data, &idx) != nil {
		return nil
	}
	return idx.Images
}

// mergeSaverIndex puts fresh downloads first, then the previous set
// (skipping files a fresh download replaced), capped at limit.
func mergeSaverIndex(old, fresh []saverFile, limit int) []saverFile {
	seen := make(map[string]bool, len(fresh))
	out := make([]saverFile, 0, len(fresh)+len(old))
	for _, it := range fresh {
		if !seen[it.File] {
			seen[it.File] = true
			out = append(out, it)
		}
	}
	for _, it := range old {
		if !seen[it.File] {
			seen[it.File] = true
			out = append(out, it)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func saverGet(client *http.Client, rawurl string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "submini-screensaver/1.0 (personal iPad player)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawurl, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// storeSaverImage validates a downloaded picture (decodable, landscape,
// wide enough), downscales it to at most 1024 px wide, saves it as a
// JPEG named after its source URL, and returns the file name.
func storeSaverImage(dir, rawurl string, data []byte) (string, bool) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return "", false
	}
	if cfg.Width < 700 || cfg.Width <= int(float64(cfg.Height)*1.1) {
		return "", false
	}
	if cfg.Width > 1024 || format != "jpeg" {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", false
		}
		img = downscale(img, 1024)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
			return "", false
		}
		data = buf.Bytes()
	}
	sum := sha256.Sum256([]byte(rawurl))
	name := "ss-" + hex.EncodeToString(sum[:6]) + ".jpg"
	tmp := filepath.Join(dir, name+".tmp")
	if os.WriteFile(tmp, data, 0o644) != nil {
		return "", false
	}
	if os.Rename(tmp, filepath.Join(dir, name)) != nil {
		_ = os.Remove(tmp)
		return "", false
	}
	return name, true
}

// downscale shrinks img to maxW wide (never up), bilinear.
func downscale(src image.Image, maxW int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW {
		return src
	}
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, src, b.Min, draw.Src)
	nw := maxW
	nh := h * maxW / w
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := float64(y) * float64(h) / float64(nh)
		y0 := int(sy)
		y1 := y0 + 1
		if y1 > h-1 {
			y1 = h - 1
		}
		fy := sy - float64(y0)
		for x := 0; x < nw; x++ {
			sx := float64(x) * float64(w) / float64(nw)
			x0 := int(sx)
			x1 := x0 + 1
			if x1 > w-1 {
				x1 = w - 1
			}
			fx := sx - float64(x0)
			var r, g, bl, a float64
			for _, p := range []struct {
				x, y   int
				weight float64
			}{
				{x0, y0, (1 - fx) * (1 - fy)},
				{x1, y0, fx * (1 - fy)},
				{x0, y1, (1 - fx) * fy},
				{x1, y1, fx * fy},
			} {
				i := rgba.PixOffset(b.Min.X+p.x, b.Min.Y+p.y)
				r += float64(rgba.Pix[i]) * p.weight
				g += float64(rgba.Pix[i+1]) * p.weight
				bl += float64(rgba.Pix[i+2]) * p.weight
				a += float64(rgba.Pix[i+3]) * p.weight
			}
			j := dst.PixOffset(x, y)
			dst.Pix[j] = uint8(r + 0.5)
			dst.Pix[j+1] = uint8(g + 0.5)
			dst.Pix[j+2] = uint8(bl + 0.5)
			dst.Pix[j+3] = uint8(a + 0.5)
		}
	}
	return dst
}

/* ---------------- sources ---------------- */

type bingResponse struct {
	Images []struct {
		URL       string `json:"url"`
		Copyright string `json:"copyright"`
	} `json:"images"`
}

// bingCaption drops the trailing "(© …)" credit from Bing's caption,
// keeping the place/subject description.
func bingCaption(s string) string {
	if i := strings.Index(s, " (©"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// saverClient is the shared outbound client, or — when network is
// "tcp4" — one whose dialer forces IPv4 (a broken IPv6 path to a CDN
// often shows up as connects that die mid-stream with EOF).
func saverClient(network string) *http.Client {
	tr := outboundTransport()
	if network != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, _ string, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{Transport: tr, Timeout: 45 * time.Second}
}

type bingRoute struct {
	host    string
	network string // "" = default routing
}

// bingRoutes are tried in order until one serves the list; the same
// route is then used for that batch's images. www.bing.com resets
// (EOF) from some networks — notably mainland-China home broadband —
// while the Met stays reachable, so: default route, IPv4-forced, and
// finally Bing's China domain, which serves the same archive.
var bingRoutes = []bingRoute{
	{"www.bing.com", ""},
	{"www.bing.com", "tcp4"},
	{"cn.bing.com", ""},
}

func fetchBing(dir string) []saverFile {
	const listPath = "/HPImageArchive.aspx?format=js&idx=0&n=8&mkt=zh-CN"
	var lastErr error
	for _, rt := range bingRoutes {
		client := saverClient(rt.network)
		data, err := saverGet(client, "https://"+rt.host+listPath, 1<<20)
		if err != nil {
			lastErr = err
			continue
		}
		var br bingResponse
		if json.Unmarshal(data, &br) != nil {
			continue
		}
		var out []saverFile
		for _, im := range br.Images {
			imgURL := "https://" + rt.host + im.URL
			raw, err := saverGet(client, imgURL, 3<<20)
			if err != nil {
				continue
			}
			if name, ok := storeSaverImage(dir, imgURL, raw); ok {
				out = append(out, saverFile{File: name, Caption: bingCaption(im.Copyright), Kind: "photo"})
			}
		}
		if len(out) > 0 {
			if rt.host != "www.bing.com" || rt.network != "" {
				log.Printf("submini: screensaver bing via fallback route %s (%s)", rt.host, rt.network)
			}
			return out
		}
	}
	log.Printf("submini: screensaver bing unreachable on all routes: %v", lastErr)
	return nil
}

type metObject struct {
	IsPublicDomain    bool   `json:"isPublicDomain"`
	PrimaryImage      string `json:"primaryImage"`
	PrimaryImageSmall string `json:"primaryImageSmall"`
	Title             string `json:"title"`
	Artist            string `json:"artistDisplayName"`
	Classification    string `json:"classification"`
}

// Only flat art counts: department lists are full of photographed
// objects (dishes, jewellery, reliquaries) whose landscape-shaped
// photos would otherwise pass the aspect filter.
func (o *metObject) isScreensaverArt() bool {
	return o.IsPublicDomain && o.PrimaryImageSmall != "" &&
		(o.Classification == "Paintings" || o.Classification == "Prints")
}

// Met departments worth browsing for landscapes: 11 European
// Paintings, 6 Asian Art, 9 Drawings and Prints, 1 The American Wing.
// (The Met's /search endpoint answers 410 Gone, so the updater walks
// a department's object list instead — same daily-rotation idea.)
var metDepartments = []int{11, 6, 9, 1}

// fetchMet rotates a department and a window into its object list by
// day, so the paintings change daily without any stored state.
func fetchMet(client *http.Client, dir string) []saverFile {
	doy := time.Now().YearDay()
	dept := metDepartments[doy%len(metDepartments)]
	surl := fmt.Sprintf("https://collectionapi.metmuseum.org/public/collection/v1/objects?departmentIds=%d", dept)
	data, err := saverGet(client, surl, 4<<20)
	if err != nil {
		log.Printf("submini: screensaver met objects: %v", err)
		return nil
	}
	var sr struct {
		ObjectIDs []int `json:"objectIDs"`
	}
	if json.Unmarshal(data, &sr) != nil || len(sr.ObjectIDs) == 0 {
		return nil
	}
	ids := sr.ObjectIDs
	start := (doy * 13) % len(ids)
	var out []saverFile
	for n := 0; n < 40 && len(out) < 10; n++ {
		id := ids[(start+n)%len(ids)]
		odata, err := saverGet(client, fmt.Sprintf("https://collectionapi.metmuseum.org/public/collection/v1/objects/%d", id), 1<<20)
		if err != nil {
			continue
		}
		var obj metObject
		if json.Unmarshal(odata, &obj) != nil || !obj.isScreensaverArt() {
			continue
		}
		raw, err := saverGet(client, obj.PrimaryImageSmall, 3<<20)
		if err != nil {
			continue
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil || cfg.Width < 700 {
			// small preview too small or unreadable: try the original
			if obj.PrimaryImage == "" {
				continue
			}
			raw, err = saverGet(client, obj.PrimaryImage, 8<<20)
			if err != nil {
				continue
			}
		}
		caption := obj.Title
		if obj.Artist != "" {
			if caption != "" {
				caption += " · "
			}
			caption += obj.Artist
		}
		if name, ok := storeSaverImage(dir, obj.PrimaryImageSmall, raw); ok {
			out = append(out, saverFile{File: name, Caption: caption, Kind: "art"})
		}
	}
	return out
}
