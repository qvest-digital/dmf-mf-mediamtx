package mxl

import (
	"encoding/binary"
	"math"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildOpusArgs(t *testing.T) {
	args := buildOpusArgs(AudioEncoderParams{SampleRate: 44100, ChannelCount: 2, Bitrate: 96000})

	require.Equal(t, "pipe:0", argValue(t, args, "-i"))
	require.Equal(t, "96000", argValue(t, args, "-b:a"))
	require.Equal(t, "libopus", argValue(t, args, "-c:a"))

	// -ar appears twice: the flow's own rate before -i, and the forced
	// 48 kHz output rate after it, because that is the only rate RTP Opus
	// has. Position is what tells ffmpeg which stream each applies to.
	var rates []string
	for i, a := range args {
		if a == "-ar" {
			rates = append(rates, args[i+1])
		}
	}
	require.Equal(t, []string{"44100", "48000"}, rates)
}

func TestNewOpusEncoderRejectsUnusableParams(t *testing.T) {
	for _, ca := range []struct {
		name string
		p    AudioEncoderParams
	}{
		{"no rate", AudioEncoderParams{ChannelCount: 2, OnPacket: func([]byte) {}}},
		{"no channels", AudioEncoderParams{SampleRate: 48000, OnPacket: func([]byte) {}}},
		// RTP Opus carries no more than a stereo pair.
		{"too many channels", AudioEncoderParams{SampleRate: 48000, ChannelCount: 6, OnPacket: func([]byte) {}}},
		{"no callback", AudioEncoderParams{SampleRate: 48000, ChannelCount: 2}},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, err := NewOpusEncoder(ca.p)
			require.Error(t, err)
		})
	}
}

// End to end against the real encoder: interleaved float samples in, Opus
// packets out. This is what proves the extractor and the ffmpeg invocation
// agree about the framing, which no fixture can.
func TestOpusEncoderRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	var mu sync.Mutex
	var packets [][]byte
	enc, err := NewOpusEncoder(AudioEncoderParams{
		SampleRate:   48000,
		ChannelCount: 2,
		Bitrate:      128000,
		OnPacket: func(p []byte) {
			mu.Lock()
			packets = append(packets, append([]byte(nil), p...))
			mu.Unlock()
		},
	})
	require.NoError(t, err)

	// One second of a 440 Hz sine, stereo, in 20 ms blocks.
	const blocks = 50
	const framesPerBlock = 48000 / 50
	buf := make([]byte, framesPerBlock*2*4)
	phase := 0.0
	for range blocks {
		for f := range framesPerBlock {
			v := float32(math.Sin(phase) * 0.25)
			phase += 2 * math.Pi * 440 / 48000
			binary.LittleEndian.PutUint32(buf[f*8:], math.Float32bits(v))
			binary.LittleEndian.PutUint32(buf[f*8+4:], math.Float32bits(v))
		}
		require.NoError(t, enc.Encode(buf))
	}
	enc.Close()

	mu.Lock()
	defer mu.Unlock()
	// Opus frames at 20 ms, so a second is about 50 packets. The encoder
	// adds pre-skip and may hold a little back, so this is a range rather
	// than an equality.
	require.Greater(t, len(packets), 40, "expected roughly one packet per 20 ms")
	require.Less(t, len(packets), 60)
	for i, p := range packets {
		require.NotEmpty(t, p, "packet %d", i)
	}
}

func TestOpusEncoderCloseIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	enc, err := NewOpusEncoder(AudioEncoderParams{
		SampleRate: 48000, ChannelCount: 2, OnPacket: func([]byte) {},
	})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		enc.Close()
		enc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
}
