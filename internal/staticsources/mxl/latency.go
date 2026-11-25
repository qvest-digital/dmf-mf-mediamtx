package mxl

import (
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

// Test build only: per-frame timestamps at each station of the video path,
// logged as percentiles every 10 s.

// encoderNUT switches ffmpeg's output from raw Annex-B to NUT.
var encoderNUT = os.Getenv("MXL_TEST_NUT") == "1"

type frameTimes struct {
	read, unpacked, piped, x264 time.Time
}

type latencyProbe struct {
	log func(string, ...any)

	mu      sync.Mutex
	q       []*frameTimes
	samples [6][]float64
	frames  int
	since   time.Time
}

var stationNames = [6]string{
	"read>unpacked", "unpacked>piped", "piped>x264out", "x264out>handed", "handed>sent", "read>sent",
}

func newLatencyProbe(log func(string, ...any)) *latencyProbe {
	return &latencyProbe{log: log, since: time.Now()}
}

// push queues a frame about to be written to the encoder.
func (p *latencyProbe) push(f *frameTimes) {
	p.mu.Lock()
	p.q = append(p.q, f)
	p.mu.Unlock()
}

func (p *latencyProbe) setPiped(f *frameTimes, t time.Time) {
	p.mu.Lock()
	f.piped = t
	p.mu.Unlock()
}

// x264Out stamps the oldest frame whose first slice has not been seen yet.
func (p *latencyProbe) x264Out(t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.q {
		if f.x264.IsZero() {
			f.x264 = t
			return
		}
	}
}

// pop takes the frame whose access unit has just been handed on.
func (p *latencyProbe) pop() *frameTimes {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.q) == 0 {
		return nil
	}
	f := p.q[0]
	p.q = p.q[1:]
	return f
}

func ms(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() {
		return -1
	}
	return float64(b.Sub(a).Microseconds()) / 1000
}

func (p *latencyProbe) record(f *frameTimes, handed, sent time.Time) {
	if f == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, d := range []float64{
		ms(f.read, f.unpacked), ms(f.unpacked, f.piped), ms(f.piped, f.x264),
		ms(f.x264, handed), ms(handed, sent), ms(f.read, sent),
	} {
		if d >= 0 {
			p.samples[i] = append(p.samples[i], d)
		}
	}
	p.frames++
	if el := time.Since(p.since); el >= 10*time.Second {
		line := fmt.Sprintf("latency-probe nut=%v frames=%d fps=%.2f",
			encoderNUT, p.frames, float64(p.frames)/el.Seconds())
		for i, s := range p.samples {
			if len(s) == 0 {
				continue
			}
			slices.Sort(s)
			line += fmt.Sprintf(" %s=%.1f/%.1f/%.1f", stationNames[i],
				s[len(s)/2], s[len(s)*99/100], s[len(s)-1])
			p.samples[i] = s[:0]
		}
		p.frames, p.since = 0, time.Now()
		p.log("%s", line)
	}
}
