package mxl

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPTSClockTicks(t *testing.T) {
	for _, ca := range []struct {
		name    string
		num     int64
		den     int64
		indices []uint64
		want    []int64
	}{
		// A contiguous run puts frames exactly one grain period apart.
		{
			"30 contiguous",
			30, 1,
			[]uint64{100, 101, 102, 103},
			[]int64{0, 3000, 6000, 9000},
		},
		// The index need not start at zero; the first grain seen is the
		// origin, whatever the writer's head happened to be.
		{
			"origin is the first grain",
			50, 1,
			[]uint64{9_000_000, 9_000_001, 9_000_002},
			[]int64{0, 1800, 3600},
		},
		// The case the whole change exists for. Three grains dropped
		// between the second and third frame must advance the timeline
		// by four periods, not by one. A counter of output frames
		// yields 0, 3000, 6000 here and falls a tenth of a second
		// behind real time on this sequence alone.
		{
			"drops advance the timeline",
			30, 1,
			[]uint64{0, 1, 5},
			[]int64{0, 3000, 15000},
		},
		// A period that is not a whole number of ticks must not lose
		// the fraction: 60000/1001 puts a frame at 1501.5 ticks, so
		// rounding each frame down would shed half a tick per frame,
		// which is over a second an hour. Every second frame is exact.
		{
			"59.94 does not accumulate error",
			60000, 1001,
			[]uint64{0, 1, 2, 3, 4},
			[]int64{0, 1501, 3003, 4504, 6006},
		},
		{
			"29.97 is exact every frame",
			30000, 1001,
			[]uint64{0, 1, 2, 3},
			[]int64{0, 3003, 6006, 9009},
		},
		// The writer's head index has been observed stepping backwards.
		// The muxers require a strictly increasing timestamp, so a
		// repeat or a step back is clamped rather than emitted.
		{
			"a backwards index is clamped",
			30, 1,
			[]uint64{10, 11, 10, 12},
			[]int64{0, 3000, 3001, 6000},
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			c := ptsClock{rateNum: ca.num, rateDen: ca.den}
			got := make([]int64, 0, len(ca.indices))
			for _, idx := range ca.indices {
				got = append(got, c.ticks(idx))
			}
			require.Equal(t, ca.want, got)
		})
	}
}

// A run of a thousand frames at 59.94 must land where the rate says it
// should, which is what a per-frame integer period cannot do.
func TestPTSClockDoesNotDriftAt5994(t *testing.T) {
	c := ptsClock{rateNum: 60000, rateDen: 1001}
	var last int64
	for i := uint64(0); i <= 1000; i++ {
		last = c.ticks(i)
	}
	// 1000 frames at 1001/60000 s is 16.6833... s, which is 1501500 ticks.
	require.Equal(t, int64(1501500), last)

	// The naive per-frame period would be 90000*1001/60000 = 1501 ticks,
	// losing half a tick a frame.
	require.NotEqual(t, int64(1000)*1501, last)
}

func TestPTSClockAdvanceKeepsCadence(t *testing.T) {
	c := ptsClock{rateNum: 30, rateDen: 1}
	require.Equal(t, int64(0), c.ticks(0))
	require.Equal(t, int64(3000), c.ticks(1))
	// No grain to pair with: keep the cadence rather than drop the frame.
	require.Equal(t, int64(6000), c.advance())
	require.Equal(t, int64(9000), c.ticks(3))
}

func TestPendingIndicesFIFO(t *testing.T) {
	p := &pendingIndices{}

	_, ok := p.pop()
	require.False(t, ok, "an empty queue must report so rather than yield a zero index")

	require.True(t, p.push(7))
	require.True(t, p.push(8))
	require.Equal(t, 2, p.depth())

	got, ok := p.pop()
	require.True(t, ok)
	require.Equal(t, uint64(7), got)
	got, ok = p.pop()
	require.True(t, ok)
	require.Equal(t, uint64(8), got)
	require.Equal(t, 0, p.depth())
}

// The queue's depth is the encoder's pipeline latency. Growing past that
// means access units have stopped coming back one per frame, and the pairing
// between index and frame is no longer meaningful.
func TestPendingIndicesRefusesUnboundedGrowth(t *testing.T) {
	p := &pendingIndices{}
	for i := range maxPendingIndices {
		require.True(t, p.push(uint64(i)), "push %d", i)
	}
	require.False(t, p.push(maxPendingIndices))
	require.Equal(t, maxPendingIndices, p.depth())
}
