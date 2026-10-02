package mxl

import (
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

func TestBuildFFmpegArgsCapsThreads(t *testing.T) {
	// Without a count of its own x264 takes one and a half per host core and
	// holds one frame per thread, which is where the latency this bounds
	// comes from -- and past 64 frames in flight the source stops entirely.
	args := buildFFmpegArgs(EncoderParams{
		Width: 1920, Height: 1080,
		RateNum: 60000, RateDen: 1001,
		Preset: "veryfast", Profile: "high",
	})
	require.Equal(t, "4", argValue(t, args, "-threads"))
	require.Less(t, encoderThreads, maxPendingIndices)
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
