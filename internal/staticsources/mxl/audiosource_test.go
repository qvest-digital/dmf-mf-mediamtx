package mxl

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAudioPair(t *testing.T) {
	for _, ca := range []struct {
		name     string
		selected []int
		channels uint32
		want     []uint64
	}{
		// The common case: nothing asked for, so the first pair.
		{"default on a stereo flow", nil, 2, []uint64{0, 1}},
		{"default on a wide flow", nil, 12, []uint64{0, 1}},
		// A mono flow has no pair to take, and asking ffmpeg for two
		// channels out of one would fail to negotiate.
		{"default on a mono flow", nil, 1, []uint64{0}},
		// 1-based in, 0-based out: what an operator reads off a router is
		// not what indexes the ring.
		{"explicit pair", []int{3, 4}, 12, []uint64{2, 3}},
		{"explicit single", []int{7}, 12, []uint64{6}},
		{"the last pair of a wide flow", []int{11, 12}, 12, []uint64{10, 11}},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got, err := audioPair(ca.selected, ca.channels)
			require.NoError(t, err)
			require.Equal(t, ca.want, got)
		})
	}
}

func TestAudioPairRefusesWhatTheFlowCannotCarry(t *testing.T) {
	// Refused rather than clamped. Publishing 1 and 2 when 11 and 12 were
	// asked for would read as those channels being silent.
	_, err := audioPair([]int{11, 12}, 2)
	require.Error(t, err)

	_, err = audioPair(nil, 0)
	require.Error(t, err)
}

// f32 builds a fragment of consecutive float32 samples, so a mis-strided
// interleave shows up as the wrong number rather than as noise.
func f32(vals ...float32) []byte {
	out := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

func readF32(b []byte, i int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
}

func TestInterleaveFragments(t *testing.T) {
	// Two channels, no wraparound: L0 R0 L1 R1 ...
	left := f32(1, 2, 3)
	right := f32(10, 20, 30)
	dst := make([]byte, 3*2*4)

	err := interleaveFragments([][2][]byte{{left, nil}, {right, nil}}, 3, dst)
	require.NoError(t, err)

	got := make([]float32, 6)
	for i := range got {
		got[i] = readF32(dst, i)
	}
	require.Equal(t, []float32{1, 10, 2, 20, 3, 30}, got)
}

func TestInterleaveFragmentsAcrossTheWraparound(t *testing.T) {
	// The range straddles the ring's end, so each channel arrives in two
	// pieces and the split falls at a different point per channel. Getting
	// this wrong is what silently swaps or repeats samples.
	dst := make([]byte, 4*2*4)
	err := interleaveFragments([][2][]byte{
		{f32(1, 2), f32(3, 4)},
		{f32(10), f32(20, 30, 40)},
	}, 4, dst)
	require.NoError(t, err)

	got := make([]float32, 8)
	for i := range got {
		got[i] = readF32(dst, i)
	}
	require.Equal(t, []float32{1, 10, 2, 20, 3, 30, 4, 40}, got)
}

func TestInterleaveFragmentsRefusesShortInput(t *testing.T) {
	// A channel that came back short means the view did not hold what was
	// asked for; packing it anyway would publish another channel's samples.
	dst := make([]byte, 3*2*4)
	err := interleaveFragments([][2][]byte{
		{f32(1, 2, 3), nil},
		{f32(10), nil},
	}, 3, dst)
	require.Error(t, err)
}

func TestInterleaveFragmentsRefusesShortDestination(t *testing.T) {
	err := interleaveFragments([][2][]byte{{f32(1), nil}, {f32(2), nil}}, 1, make([]byte, 4))
	require.Error(t, err)
}

// A contiguous run advances one frame a packet, and the sequence number one
// per packet, which is all a receiver needs to reassemble it.
func TestAudioTimelineStampsAContiguousRun(t *testing.T) {
	tl := &audioTimeline{}

	for i := range 4 {
		pts, seq := tl.stamp(int64(i)*opusFrameSamples, -1)
		require.Equal(t, int64(i)*opusFrameSamples, pts)
		require.Equal(t, uint16(i), seq) //nolint:gosec // small loop bound
	}
}

// A re-anchor is what makes samples the reader skipped over cost time on the
// timeline rather than silently shortening it.
func TestAudioTimelineFollowsAReanchorForwards(t *testing.T) {
	tl := &audioTimeline{}
	tl.stamp(0, 0)

	// The reader resynced two seconds further on.
	pts, _ := tl.stamp(opusFrameSamples, 2*opusClockRate)
	require.Equal(t, int64(2*opusClockRate), pts)
}

// The defect a source that stops and comes back produced: the track was
// restarted, began its own count at zero, and published a timestamp behind
// everything already sent. RTSP and HLS both refuse that, and a receiver reads
// it as a different stream.
func TestAudioTimelineNeverStepsBackwards(t *testing.T) {
	tl := &audioTimeline{}
	tl.stamp(0, 0)
	tl.stamp(opusFrameSamples, -1)

	// A restarted track: its own count is back at zero and its resync landed
	// somewhere behind what the timeline has already published.
	pts, _ := tl.stamp(0, opusFrameSamples/2)
	require.Greater(t, pts, int64(opusFrameSamples),
		"a restart must continue the timeline, not restate it")

	// And a re-anchor that lands behind is refused rather than followed.
	next, _ := tl.stamp(pts+opusFrameSamples, 0)
	require.Greater(t, next, pts)
}

// The sequence number is a counter, not a function of the timestamp. Audio
// resuming after a ten-minute outage jumps the timeline by ten minutes, and a
// derived sequence number would jump with it: RFC 3550 has a receiver treat a
// jump that large as a different stream and drop packets until two arrive in
// order, so the gap in the sound would cost the packets after it as well.
func TestAudioTimelineSequenceSurvivesAnOutage(t *testing.T) {
	tl := &audioTimeline{}
	_, first := tl.stamp(0, 0)

	// Ten minutes later, on a fresh attempt whose own count starts at zero.
	pts, second := tl.stamp(0, 600*opusClockRate)
	require.Equal(t, int64(600*opusClockRate), pts)
	require.Equal(t, first+1, second)
}
