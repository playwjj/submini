package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAnalyzeMP3 runs the decoder against a real MP3: the checked-in
// fixture by default (a 6-second synthetic track with a kick every
// 0.5s and an off-beat blip, so onsets land 4 times a second), or any
// file named by RHYTHM_MP3 for manual diagnosis of a real song.
func TestAnalyzeMP3(t *testing.T) {
	p := os.Getenv("RHYTHM_MP3")
	fixture := p == ""
	if fixture {
		p = "testdata/rhythm-test.mp3"
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t0 := time.Now()
	frames, n, err := analyzeMP3(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != n*rhythmStride {
		t.Fatalf("frames bytes = %d, want %d", len(frames), n*rhythmStride)
	}
	beats := 0
	for i := 0; i < n; i++ {
		if frames[i*rhythmStride+rhythmBands+1] > 0 {
			beats++
		}
	}
	t.Logf("frames=%d (%.0fs) beats=%d (%.1f/s) took %v", n, float64(n)/rhythmFPS, beats, float64(beats)*rhythmFPS/float64(n), time.Since(t0))
	if fixture {
		if n < 110 || n > 130 { // 6s at 20fps, plus MP3 padding frames
			t.Errorf("frames = %d, want ~120", n)
		}
		if beats < 12 || beats > 30 { // 24 onsets in the fixture
			t.Errorf("beats = %d, want ~24", beats)
		}
	}
	// a short window of frames from the middle, for eyeballing
	lo := n / 2
	hi := lo + 8
	if hi > n {
		hi = n
	}
	for i := lo; i < hi; i++ {
		t.Logf("%v", frames[i*rhythmStride:(i+1)*rhythmStride])
	}
}

// A synthetic kick every half second must come out as beats on (or
// right next to) those frames, and nowhere else.
func TestEnvelopeBeats(t *testing.T) {
	n := 400
	raw := make([][rhythmBands + 1]float64, n)
	for i := range raw {
		for b := range raw[i] {
			raw[i][b] = -3 + 0.01*float64((i*7+b*3)%5) // quiet noise floor
		}
		if i%10 == 0 { // kick: lows and loudness jump
			raw[i][0], raw[i][1], raw[i][rhythmBands] = -1, -1.2, -1.5
		}
	}
	out := envelope(raw)
	if len(out) != n*rhythmStride {
		t.Fatalf("len = %d, want %d", len(out), n*rhythmStride)
	}
	hits, stray := 0, 0
	for i := 0; i < n; i++ {
		if out[i*rhythmStride+rhythmBands+1] == 0 {
			continue
		}
		if i%10 == 0 {
			hits++
		} else {
			stray++
		}
	}
	if hits < 35 || stray > 0 {
		t.Errorf("beats on kicks = %d (want >= 35), stray = %d", hits, stray)
	}
	if out[10*rhythmStride] < 200 || out[11*rhythmStride] > 50 {
		t.Errorf("kick band not normalised: on=%d off=%d", out[10*rhythmStride], out[11*rhythmStride])
	}
}

func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/rhythm-test.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// A failed analysis is remembered: the song is not fetched again on
// the next ask. A successful one is served from memory.
func TestRhythmNegativeCache(t *testing.T) {
	ctx := context.Background()

	c := newRhythmCache()
	c.dir = t.TempDir()
	var calls atomic.Int32
	failOpen := func(context.Context) (io.ReadCloser, error) {
		calls.Add(1)
		return nil, errors.New("upstream down")
	}
	if _, err := c.get(ctx, "bad-song", failOpen); err == nil {
		t.Fatal("first get: want error")
	}
	if _, err := c.get(ctx, "bad-song", failOpen); err == nil {
		t.Fatal("second get: want cached error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("open called %d times, want 1 (negative cache)", got)
	}

	c2 := newRhythmCache()
	c2.dir = t.TempDir()
	mp3 := fixtureBytes(t)
	var calls2 atomic.Int32
	okOpen := func(context.Context) (io.ReadCloser, error) {
		calls2.Add(1)
		return io.NopCloser(bytes.NewReader(mp3)), nil
	}
	r1, err := c2.get(ctx, "good-song", okOpen)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c2.get(ctx, "good-song", okOpen)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Error("second get: want the same cached result")
	}
	if got := calls2.Load(); got != 1 {
		t.Errorf("open called %d times, want 1 (memory cache)", got)
	}
}

// With one analysis running and the queue full, a further song is
// refused (not queued, not negative-cached); once the queue drains it
// can be analysed normally.
func TestRhythmQueueCap(t *testing.T) {
	ctx := context.Background()
	c := newRhythmCache()
	c.dir = t.TempDir()
	mp3 := fixtureBytes(t)
	release := make(chan struct{})
	var started atomic.Int32
	blockingOpen := func(context.Context) (io.ReadCloser, error) {
		started.Add(1)
		<-release
		return io.NopCloser(bytes.NewReader(mp3)), nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 1+rhythmMaxQueue)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.get(ctx, string(rune('a'+i)), blockingOpen)
		}(i)
	}
	waitFor(t, "first analysis to start", func() bool { return started.Load() == 1 })
	waitFor(t, "queue to fill", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.waiting == rhythmMaxQueue
	})

	busyDone := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "one-too-many", blockingOpen)
		busyDone <- err
	}()
	select {
	case err := <-busyDone:
		if !errors.Is(err, errRhythmBusy) {
			t.Fatalf("overflow get: err = %v, want errRhythmBusy", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("overflow get did not return")
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("open started %d times, want 1 (overflow never fetched)", got)
	}

	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("queued get %d: %v", i, err)
		}
	}
	// the refused song was not negative-cached: it analyses fine now
	r, err := c.get(ctx, "one-too-many", blockingOpen)
	if err != nil {
		t.Fatalf("refused song after drain: %v", err)
	}
	if r == nil || r.Frames == 0 {
		t.Error("refused song after drain: empty result")
	}
}
