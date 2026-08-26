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
