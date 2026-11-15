package main

import (
	"os"
	"testing"
	"time"
)

func TestAnalyzeLocalMP3(t *testing.T) {
	p := os.Getenv("RHYTHM_MP3")
	if p == "" {
		t.Skip("RHYTHM_MP3 not set")
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
	beats := 0
	for i := 0; i < n; i++ {
		if frames[i*rhythmStride+rhythmBands+1] > 0 {
			beats++
		}
	}
	t.Logf("frames=%d (%.0fs) beats=%d (%.1f/s) took %v", n, float64(n)/rhythmFPS, beats, float64(beats)*rhythmFPS/float64(n), time.Since(t0))
	for i := 600; i < 640; i++ {
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
