package mxl

import (
	"testing"

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
