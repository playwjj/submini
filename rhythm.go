package main

// Rhythm analysis for the playing-state screensaver.
//
// The iPad cannot analyse audio itself without hurting playback: the
// only route is Web Audio's createMediaElementSource, which on iOS 9
// pulls the <audio> element into an AudioContext — background playback
// and the stream watchdog's reconnects stop working. So the server does
// it ahead of time instead: it fetches the same MP3 the page plays,
// decodes it, and reduces it to a compact per-frame envelope (eight
// frequency bands, overall loudness and detected beats) that the page
// lines up with audio.currentTime. Results are cached in memory and on
// disk, so each song is analysed once.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

const (
	rhythmFPS      = 20              // envelope frames per second
	rhythmBands    = 8               // frequency bands per frame
	rhythmStride   = rhythmBands + 2 // bands, loudness, beat
	rhythmMaxSecs  = 20 * 60         // longer tracks: the tail is left unanalysed
	rhythmMaxBytes = 80 << 20        // cap on the fetched MP3
	rhythmMemMax   = 48              // envelopes kept in memory
	rhythmDiskMax  = 48 << 20        // disk layer under DATA_DIR/rhythm
	rhythmTimeout  = 3 * time.Minute // fetch + decode, per song
	rhythmVersion  = 1               // bump when the analysis changes
	rhythmFailTTL  = time.Hour       // a failed song is not retried within this window
	rhythmMaxQueue = 4               // analyses waiting their turn; beyond this, refuse
)

// Band centre frequencies (Hz), roughly one per octave from sub-bass up.
var rhythmCenters = [rhythmBands]float64{55, 120, 260, 560, 1200, 2600, 5500, 11000}

type rhythmResult struct {
	Version int    `json:"v"`
	FPS     int    `json:"fps"`
	Bands   int    `json:"bands"`
	Stride  int    `json:"stride"`
	Frames  int    `json:"n"`
	Data    string `json:"data"` // base64 of Frames*Stride bytes
}

type rhythmCache struct {
	mu       sync.Mutex
	items    map[string]*rhythmResult
	order    []string
	inflight map[string]*rhythmCall
	fails    map[string]rhythmFail
	waiting  int           // analyses queued behind the one running
	sem      chan struct{} // one analysis at a time: it is CPU-bound
	dir      string
}

// rhythmFail is a recent analysis failure, kept so a song that cannot
// be analysed (upstream error, undecodable file) is not fetched and
// decoded again on every ask.
type rhythmFail struct {
	at  time.Time
	err error
}

var errRhythmBusy = errors.New("rhythm analysis queue full")

type rhythmCall struct {
	done chan struct{}
	res  *rhythmResult
	err  error
}

func newRhythmCache() *rhythmCache {
	return &rhythmCache{
		items:    make(map[string]*rhythmResult),
		inflight: make(map[string]*rhythmCall),
		fails:    make(map[string]rhythmFail),
		sem:      make(chan struct{}, 1),
	}
}

var rhythms = newRhythmCache()

func (c *rhythmCache) fileFor(key string) string {
	if c.dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".rhy")
}

func (c *rhythmCache) remember(key string, r *rhythmResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fails, key)
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
	}
	c.items[key] = r
	for len(c.order) > rhythmMemMax {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
}

// recordFail remembers an analysis failure for rhythmFailTTL and
// prunes expired entries while it is there.
func (c *rhythmCache) recordFail(key string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, f := range c.fails {
		if now.Sub(f.at) >= rhythmFailTTL {
			delete(c.fails, k)
		}
	}
	c.fails[key] = rhythmFail{at: now, err: err}
}

// Disk format: 4-byte version, 4-byte frame count, then the raw frames.
func (c *rhythmCache) diskGet(key string) *rhythmResult {
	p := c.fileFor(key)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) < 8 {
		return nil
	}
	if binary.LittleEndian.Uint32(b[0:4]) != rhythmVersion {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(b[4:8]))
	if len(b) != 8+n*rhythmStride {
		return nil
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now) // recently used: trimmed last
	return newRhythmResult(b[8:], n)
}

func (c *rhythmCache) persist(key string, frames []byte, n int) {
	p := c.fileFor(key)
	if p == "" {
		return
	}
	b := make([]byte, 8+len(frames))
	binary.LittleEndian.PutUint32(b[0:4], rhythmVersion)
	binary.LittleEndian.PutUint32(b[4:8], uint32(n))
	copy(b[8:], frames)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
		return
	}
	trimDir(c.dir, ".rhy", rhythmDiskMax)
}

// trimDir deletes the oldest files with the given suffix until the
// directory's total is under max bytes.
func trimDir(dir, suffix string, max int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		path string
		size int64
		mod  time.Time
	}
	var files []f
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
		files = append(files, f{filepath.Join(dir, e.Name()), fi.Size(), fi.ModTime()})
	}
	if total <= max {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, x := range files {
		if total <= max {
			break
		}
		if os.Remove(x.path) == nil {
			total -= x.size
		}
	}
}

func newRhythmResult(frames []byte, n int) *rhythmResult {
	return &rhythmResult{
		Version: rhythmVersion, FPS: rhythmFPS, Bands: rhythmBands,
		Stride: rhythmStride, Frames: n,
		Data: base64.StdEncoding.EncodeToString(frames),
	}
}

// get returns the envelope for key, analysing the stream from open()
// once even when several requests for the same song arrive together.
func (c *rhythmCache) get(ctx context.Context, key string, open func(context.Context) (io.ReadCloser, error)) (*rhythmResult, error) {
	c.mu.Lock()
	if r, ok := c.items[key]; ok {
		c.mu.Unlock()
		return r, nil
	}
	if f, ok := c.fails[key]; ok {
		if time.Since(f.at) < rhythmFailTTL {
			c.mu.Unlock()
			return nil, f.err
		}
		delete(c.fails, key)
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.res, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &rhythmCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	// The analysis runs detached from the request: if the iPad gives up
	// waiting, the result still lands in the cache for its next ask.
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
			close(call.done)
		}()
		if r := c.diskGet(key); r != nil {
			c.remember(key, r)
			call.res = r
			return
		}
		// One analysis runs at a time; a short queue may wait, but a
		// burst of track-skipping must not line up unbounded work.
		// Refusing is not the song's failure: no negative cache.
		c.mu.Lock()
		if c.waiting >= rhythmMaxQueue {
			c.mu.Unlock()
			call.err = errRhythmBusy
			return
		}
		c.waiting++
		c.mu.Unlock()
		c.sem <- struct{}{}
		c.mu.Lock()
		c.waiting--
		c.mu.Unlock()
		defer func() { <-c.sem }()
		actx, cancel := context.WithTimeout(context.Background(), rhythmTimeout)
		defer cancel()
		body, err := open(actx)
		if err != nil {
			c.recordFail(key, err)
			call.err = err
			return
		}
		defer body.Close()
		frames, n, err := analyzeMP3(io.LimitReader(body, rhythmMaxBytes))
		if err != nil {
			c.recordFail(key, err)
			call.err = err
			return
		}
		c.persist(key, frames, n)
		r := newRhythmResult(frames, n)
		c.remember(key, r)
		call.res = r
	}()

	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// biquad is an RBJ band-pass filter (constant 0 dB peak gain).
type biquad struct {
	b0, b2, a1, a2 float64
	x1, x2, y1, y2 float64
}

func newBandpass(fc, q, sr float64) biquad {
	w := 2 * math.Pi * fc / sr
	alpha := math.Sin(w) / (2 * q)
	a0 := 1 + alpha
	return biquad{
		b0: alpha / a0, b2: -alpha / a0,
		a1: -2 * math.Cos(w) / a0, a2: (1 - alpha) / a0,
	}
}

func (f *biquad) step(x float64) float64 {
	y := f.b0*x + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// analyzeMP3 decodes an MP3 stream and returns the envelope frames.
func analyzeMP3(r io.Reader) ([]byte, int, error) {
	// go-mp3 reads in tiny pieces: buffer, or syscalls dominate
	dec, err := mp3.NewDecoder(bufio.NewReaderSize(r, 64<<10))
	if err != nil {
		return nil, 0, err
	}
	sr := float64(dec.SampleRate())
	if sr <= 0 {
		return nil, 0, errors.New("bad sample rate")
	}
	hop := int(sr) / rhythmFPS
	var filters [rhythmBands]biquad
	for i, fc := range rhythmCenters {
		if fc > sr*0.45 {
			fc = sr * 0.45
		}
		filters[i] = newBandpass(fc, 1.2, sr)
	}
	maxFrames := rhythmMaxSecs * rhythmFPS
	// raw per-frame values in log scale: bands then loudness
	raw := make([][rhythmBands + 1]float64, 0, 6000)
	var acc [rhythmBands + 1]float64
	count := 0
	buf := make([]byte, 16384)
	var carry []byte
	for len(raw) < maxFrames {
		nr, rerr := dec.Read(buf)
		data := buf[:nr]
		if len(carry) > 0 {
			data = append(carry, data...)
			carry = nil
		}
		usable := len(data) &^ 3
		if usable < len(data) {
			carry = append([]byte(nil), data[usable:]...)
		}
		for i := 0; i < usable; i += 4 {
			l := int16(binary.LittleEndian.Uint16(data[i:]))
			rr := int16(binary.LittleEndian.Uint16(data[i+2:]))
			x := (float64(l) + float64(rr)) / 65536
			for b := range filters {
				y := filters[b].step(x)
				acc[b] += y * y
			}
			acc[rhythmBands] += x * x
			count++
			if count == hop {
				var fr [rhythmBands + 1]float64
				for b := range acc {
					fr[b] = math.Log10(math.Sqrt(acc[b]/float64(hop)) + 1e-5)
					acc[b] = 0
				}
				raw = append(raw, fr)
				count = 0
				if len(raw) >= maxFrames {
					break
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// a truncated tail still leaves a usable envelope
			if len(raw) > rhythmFPS*5 {
				break
			}
			return nil, 0, rerr
		}
	}
	if len(raw) == 0 {
		return nil, 0, errors.New("no audio")
	}
	return envelope(raw), len(raw), nil
}

// envelope normalises each channel to the song's own range (so a quiet
// recording still fills the screen) and adds a beat channel.
func envelope(raw [][rhythmBands + 1]float64) []byte {
	n := len(raw)
	norm := make([][rhythmBands + 1]float64, n)
	vals := make([]float64, n)
	for ch := 0; ch <= rhythmBands; ch++ {
		for i := range raw {
			vals[i] = raw[i][ch]
		}
		sort.Float64s(vals)
		lo := vals[n*15/100]
		hi := vals[(n-1)*985/1000]
		if hi-lo < 0.3 { // near-silent channel: keep it low, don't amplify noise
			lo = hi - 0.3
		}
		for i := range raw {
			v := (raw[i][ch] - lo) / (hi - lo)
			norm[i][ch] = math.Max(0, math.Min(1, v))
		}
	}
	// Onset strength: rises in the low bands (kick, bass) weigh most,
	// with the mids adding snares and plucks.
	weights := [rhythmBands]float64{1.0, 1.0, 0.6, 0.4, 0.3, 0.3, 0.2, 0.1}
	flux := make([]float64, n)
	for i := 1; i < n; i++ {
		s := 0.0
		for b := 0; b < rhythmBands; b++ {
			if d := norm[i][b] - norm[i-1][b]; d > 0 {
				s += d * weights[b]
			}
		}
		flux[i] = s
	}
	// Peak picking against a moving average, at most ~5 beats a second.
	beat := make([]float64, n)
	const win = 10
	last := -100
	maxStrength := 0.0
	for i := 1; i < n-1; i++ {
		lo, hi := i-win, i+win
		if lo < 0 {
			lo = 0
		}
		if hi > n-1 {
			hi = n - 1
		}
		sum := 0.0
		for j := lo; j <= hi; j++ {
			sum += flux[j]
		}
		mean := sum / float64(hi-lo+1)
		th := mean*1.5 + 0.05
		if flux[i] > th && flux[i] >= flux[i-1] && flux[i] >= flux[i+1] && i-last >= 4 {
			beat[i] = flux[i] - mean
			last = i
			if beat[i] > maxStrength {
				maxStrength = beat[i]
			}
		}
	}
	out := make([]byte, n*rhythmStride)
	for i := 0; i < n; i++ {
		o := i * rhythmStride
		for ch := 0; ch <= rhythmBands; ch++ {
			out[o+ch] = byte(math.Round(norm[i][ch] * 255))
		}
		if beat[i] > 0 && maxStrength > 0 {
			// floor at 0.35 so every detected beat is visible
			out[o+rhythmBands+1] = byte(math.Round((0.35 + 0.65*math.Min(1, beat[i]/(maxStrength*0.6))) * 255))
		}
	}
	return out
}

// handleAPIRhythm serves /api/rhythm?id=<song id>.
func handleAPIRhythm(w http.ResponseWriter, r *http.Request) {
	c := current()
	if c == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not configured"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	key := c.Server + "\x00" + c.Username + "\x00" + id
	open := func(ctx context.Context) (io.ReadCloser, error) {
		u := *c.base
		u.Path = joinPaths(u.Path, "/rest/stream.view")
		q := u.Query()
		q.Set("id", id)
		q.Set("format", "mp3") // what the page plays: Navidrome's transcode cache is shared
		q.Set("u", c.Username)
		q.Set("t", c.Token)
		q.Set("s", c.Salt)
		q.Set("v", "1.16.1")
		q.Set("c", "submini")
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := (&http.Client{Transport: outboundTransport()}).Do(req)
		if err != nil {
			return nil, err
		}
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode != http.StatusOK || strings.Contains(ct, "json") || strings.Contains(ct, "xml") {
			resp.Body.Close()
			return nil, errors.New("upstream stream: " + resp.Status)
		}
		return resp.Body, nil
	}
	res, err := rhythms.get(r.Context(), key, open)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("submini: rhythm %s: %v", id, err)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "analysis failed"})
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	writeJSON(w, http.StatusOK, res)
}
