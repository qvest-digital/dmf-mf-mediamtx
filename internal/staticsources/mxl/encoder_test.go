package mxl

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/qvest-digital/go-mxl/mxl"
	"github.com/stretchr/testify/require"
)

func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag {
			require.Less(t, i+1, len(args), "%s has no value", flag)
			return args[i+1]
		}
	}
	t.Fatalf("%s not present in %v", flag, args)
	return ""
}

func TestBuildFFmpegArgsRate(t *testing.T) {
	for _, ca := range []struct {
		name string
		num  int64
		den  int64
		rate string
		idr  string
	}{
		// A fractional rate has to survive as a fraction: rounded to 60 it
		// reaches the SPS and x264's rate control as a rate the flow does
		// not produce.
		{"59.94", 60000, 1001, "60000/1001", "30"},
		{"29.97", 30000, 1001, "30000/1001", "15"},
		{"60", 60, 1, "60/1", "30"},
		{"50", 50, 1, "50/1", "25"},
		{"30", 30, 1, "30/1", "15"},
		// Rounds up rather than down: 12 frames would put the GOP past half
		// a second, which is the bound the default exists to hold.
		{"25", 25, 1, "25/1", "13"},
		// A rate below two frames a second still needs an IDR per GOP.
		{"1", 1, 1, "1/1", "1"},
	} {
		t.Run(ca.name, func(t *testing.T) {
			args := buildFFmpegArgs(EncoderParams{
				Width: 1920, Height: 1080,
				RateNum: ca.num, RateDen: ca.den,
				Preset: "veryfast", Profile: "high",
			})
			require.Equal(t, ca.rate, argValue(t, args, "-r"))
			require.Equal(t, ca.idr, argValue(t, args, "-g"))
		})
	}
}

func TestBuildFFmpegArgsSlicesInsteadOfFrameThreads(t *testing.T) {
	// Frame threading holds threads-1 frames before any output, 120 ms at
	// 25 fps with four threads; sliced threads hold none. The count is fixed
	// rather than one and a half per host core, x264's own default.
	args := buildFFmpegArgs(EncoderParams{
		Width: 1920, Height: 1080,
		RateNum: 60000, RateDen: 1001,
		Preset: "veryfast", Profile: "high",
	})
	require.Equal(t, "4", argValue(t, args, "-threads"))
	require.Contains(t, strings.Split(argValue(t, args, "-x264-params"), ":"), "sliced-threads=1")
}

// annexB joins NAL units into an Annex-B byte stream.
func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

func TestReadAUsGroupsSlicesIntoOnePicture(t *testing.T) {
	// Header byte, then the slice header whose first bit is set exactly when
	// first_mb_in_slice is 0. One access unit per picture is what pairs each
	// one with its grain index; one per slice would run the queue dry and
	// stamp a picture's slices with different timestamps.
	sps, pps := []byte{0x67, 0x64}, []byte{0x68, 0xee}
	idrFirst, idrNext := []byte{0x65, 0x88, 1}, []byte{0x65, 0x40, 2}
	pFirst, pNext := []byte{0x41, 0x9a, 3}, []byte{0x41, 0x30, 4}
	stream := annexB(
		sps, pps, idrFirst, idrNext, idrNext, idrNext,
		pFirst, pNext, pNext, pNext,
		pFirst, pNext,
		// The last picture's end is never seen before EOF.
		pFirst,
	)

	var aus [][][]byte
	e := &H264Encoder{
		ffmpegProcess: &ffmpegProcess{stdout: io.NopCloser(bytes.NewReader(stream))},
		params:        EncoderParams{OnData: func(au [][]byte) { aus = append(aus, au) }},
	}
	require.NoError(t, e.readAUs())

	require.Equal(t, [][][]byte{
		{sps, pps, idrFirst, idrNext, idrNext, idrNext},
		{pFirst, pNext, pNext, pNext},
		{pFirst, pNext},
	}, aus)
}

func TestH264EncoderOneAccessUnitPerFrame(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const w, h, frames = 640, 360, 20
	var mu sync.Mutex
	var slices []int
	enc, err := NewH264Encoder(EncoderParams{
		Width: w, Height: h, RateNum: 25, RateDen: 1,
		Preset: "superfast", Profile: "high", Bitrate: 1_000_000,
		OnData: func(au [][]byte) {
			n := 0
			for _, nal := range au {
				if typ := nal[0] & 0x1F; typ == 1 || typ == 5 {
					n++
				}
			}
			mu.Lock()
			slices = append(slices, n)
			mu.Unlock()
		},
	})
	require.NoError(t, err)

	y, c := make([]byte, w*h), make([]byte, w*h/4)
	for i := range frames {
		for j := range y {
			y[j] = byte(i*7 + j%251)
		}
		require.NoError(t, enc.Encode(y, c, c))
	}
	enc.Close()
	_ = enc.Wait()

	mu.Lock()
	defer mu.Unlock()
	// The last picture stays unfinished: its end would be the next one's
	// start.
	require.Len(t, slices, frames-1)
	for _, n := range slices {
		require.Equal(t, encoderThreads, n)
	}
}

func TestBuildFFmpegArgsIDRPeriodOverride(t *testing.T) {
	args := buildFFmpegArgs(EncoderParams{
		Width: 1920, Height: 1080,
		RateNum: 50, RateDen: 1,
		IDRPeriod: 100,
	})
	require.Equal(t, "100", argValue(t, args, "-g"))
}

func TestNewH264EncoderRejectsUnusableRate(t *testing.T) {
	for _, ca := range []struct {
		name string
		num  int64
		den  int64
	}{
		{"zero numerator", 0, 1},
		{"zero denominator", 30, 0},
		{"negative", -30, 1},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, err := NewH264Encoder(EncoderParams{
				Width: 1920, Height: 1080,
				RateNum: ca.num, RateDen: ca.den,
				OnData: func([][]byte) {},
			})
			require.Error(t, err)
		})
	}
}

func TestBuildFFmpegArgsScalesDownToMaxHeight(t *testing.T) {
	// A preview seen in a browser tile does not need the flow's full size,
	// and the encode is what a busy node spends its cores on.
	args := buildFFmpegArgs(EncoderParams{
		Width: 1920, Height: 1080,
		RateNum: 50, RateDen: 1,
		Preset: "veryfast", Profile: "high",
		OutHeight: 720,
	})
	require.Equal(t, "scale=-2:720", argValue(t, args, "-vf"))
}

func TestBuildFFmpegArgsNeverScalesUp(t *testing.T) {
	for _, out := range []uint32{0, 1080, 2160} {
		args := buildFFmpegArgs(EncoderParams{
			Width: 1920, Height: 1080,
			RateNum: 50, RateDen: 1,
			Preset: "veryfast", Profile: "high",
			OutHeight: out,
		})
		require.NotContains(t, args, "-vf", "OutHeight %d", out)
	}
}

func TestGrainStepKeepsTheRateAtOrBelowTheMax(t *testing.T) {
	for _, ca := range []struct {
		num, den int64
		max      uint
		want     int64
	}{
		{50, 1, 0, 1},        // no maximum: every grain
		{50, 1, 25, 2},       // 50 -> 25
		{50, 1, 30, 2},       // 50 -> 25, never above 30
		{25, 1, 25, 1},       // already at the maximum
		{60000, 1001, 30, 2}, // 59.94 -> 29.97
		{30000, 1001, 25, 2}, // 29.97 -> 14.99, never above 25
		{50, 1, 60, 1},       // maximum above the flow's rate
	} {
		got := grainStep(mxl.Rational{Num: ca.num, Den: ca.den}, ca.max)
		require.Equal(t, ca.want, got, "%d/%d max %d", ca.num, ca.den, ca.max)
	}
}
