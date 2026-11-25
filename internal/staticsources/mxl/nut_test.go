package mxl

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNUTReaderReadsFFmpegFrames(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	const frames = 30
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-frames:v", "30",
		"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency", "-threads", "4",
		"-x264-params", "sliced-threads=1", "-g", "13",
		"-bsf:v", "dump_extra=freq=keyframe", "-f", "nut", "pipe:1").Output()
	require.NoError(t, err)

	r := newNUTReader(bytes.NewReader(out))
	var got int
	for {
		frame, err := r.next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		nals := splitAnnexB(frame)
		require.NotEmpty(t, nals)
		if got%13 == 0 {
			// dump_extra puts SPS and PPS in front of every IDR.
			require.Equal(t, byte(7), nals[0][0]&0x1F, "frame %d", got)
		}
		slices := 0
		for _, n := range nals {
			if isSlice(n) {
				slices++
			}
		}
		require.Equal(t, encoderThreads, slices, "frame %d", got)
		got++
	}
	require.Equal(t, frames, got)
}

func TestH264EncoderNUTHandsOnEachFrameAtOnce(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	// Annex-B shows a picture's end only once the next one starts, so the
	// last frame written stays unfinished however long the encoder idles;
	// NUT carries each frame's size and hands it on complete.
	for _, ca := range []struct {
		nut  bool
		want int
	}{{false, 4}, {true, 5}} {
		encoderNUT = ca.nut
		var mu sync.Mutex
		var aus int
		enc, err := NewH264Encoder(EncoderParams{
			Width: 640, Height: 360, RateNum: 25, RateDen: 1,
			Preset: "superfast", Profile: "high", Bitrate: 1_000_000,
			OnData: func([][]byte) { mu.Lock(); aus++; mu.Unlock() },
		})
		require.NoError(t, err)
		y, c := make([]byte, 640*360), make([]byte, 640*360/4)
		for i := range 5 {
			y[i] = byte(i)
			require.NoError(t, enc.Encode(y, c, c))
			time.Sleep(40 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		got := aus
		mu.Unlock()
		enc.Close()
		_ = enc.Wait()
		require.Equal(t, ca.want, got, "nut=%v", ca.nut)
	}
	encoderNUT = false
}
