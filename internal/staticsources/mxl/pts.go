package mxl

import "sync"

// rtpClockRate is the RTP clock every video format here is timed against.
const rtpClockRate = 90000

// maxPendingIndices bounds the reader-to-encoder queue below. The encoder's
// pipeline latency is one frame, so anything beyond a handful means ffmpeg is
// buffering frames it was told not to buffer.
const maxPendingIndices = 64

// ptsClock turns an MXL grain index into an RTP timestamp.
//
// The grain index is the writer's own clock: it advances in exact grain
// periods and carries none of the jitter that read and encode times pick up
// on a loaded node. Deriving PTS from it rather than from time.Now() is what
// keeps output frames an exact grain period apart, and deriving it from the
// index rather than from a count of output frames is what keeps the timeline
// correct when grains are dropped -- the reader drops by design, and a
// dropped grain advances the index by more than one.
type ptsClock struct {
	// rateNum/rateDen are the flow's grain rate as the flow declares it.
	rateNum int64
	rateDen int64

	firstSet bool
	first    uint64
	last     int64
}

// ticks returns the RTP timestamp for a grain index.
//
// The multiplication happens before the division so a rate whose period is
// not a whole number of ticks does not accumulate error: 60000/1001 puts a
// frame at 1501.5 ticks, and rounding each frame down would lose half a tick
// per frame, which is a second and a quarter every hour. Carrying the whole
// index into the numerator keeps every second frame exact and bounds the
// error below one tick for the rest.
func (c *ptsClock) ticks(index uint64) int64 {
	if !c.firstSet {
		c.first = index
		c.firstSet = true
		c.last = 0
		return 0
	}

	var out int64
	if index >= c.first {
		out = int64(index-c.first) * rtpClockRate * c.rateDen / c.rateNum
	}

	// The head index has been observed stepping backwards, and a reader that
	// re-reads an older grain would otherwise emit a PTS the muxers reject:
	// RTSP and HLS both require it strictly increasing. Clamping keeps the
	// stream well-formed, and the caller logs the first occurrence because a
	// backwards index is a fault in the writer, not here.
	if out <= c.last {
		out = c.last + 1
	}
	c.last = out
	return out
}

// pendingIndices carries a grain index from the reader goroutine, which
// pushes one per frame handed to the encoder, to the encoder's output
// goroutine, which pops one per access unit.
//
// The encoder runs zerolatency with no B-frames and emits access units in
// input order, one per input frame, so this is a plain FIFO. Its steady-state
// depth is one: a NAL unit's boundary is only known once the next start code
// has been read, so the first access unit emerges only after the second frame
// has been pushed.
type pendingIndices struct {
	mu       sync.Mutex
	q        []uint64
	overflow bool
}

// push appends an index. It reports false once the queue has grown past what
// the encoder's pipeline latency can explain, which means access units are
// not coming back one per frame and the pairing has been lost.
func (p *pendingIndices) push(index uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.q) >= maxPendingIndices {
		p.overflow = true
		return false
	}
	p.q = append(p.q, index)
	return true
}

// pop removes and returns the oldest index. It reports false when the queue
// is empty, which means the encoder produced an access unit for a frame that
// was never pushed.
func (p *pendingIndices) pop() (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.q) == 0 {
		return 0, false
	}
	index := p.q[0]
	p.q = p.q[1:]
	return index, true
}

// advance returns the timestamp one grain period after the last one issued,
// for the case where an access unit arrives with no grain index to pair it
// with. It keeps the cadence rather than dropping the frame.
func (c *ptsClock) advance() int64 {
	out := c.last + rtpClockRate*c.rateDen/c.rateNum
	if out <= c.last {
		out = c.last + 1
	}
	c.last = out
	return out
}

// depth reports the current queue length, for diagnostics.
func (p *pendingIndices) depth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q)
}
