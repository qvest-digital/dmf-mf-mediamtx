package mxl

import (
	"fmt"
	"testing"
	"time"

	"github.com/qvest-digital/go-mxl/mxl"
	"github.com/stretchr/testify/require"
)

func TestParseMXLURLJoined(t *testing.T) {
	u, err := parseMXLURL("mxl:///run/mxl/domain/d4d00000-0000-0000-0000-000000000001" +
		"?audio=aea7b9e9-1e5b-4333-9ac4-8689053a77de")
	require.NoError(t, err)
	require.Equal(t, "/run/mxl/domain", u.domain)
	require.Equal(t, "d4d00000-0000-0000-0000-000000000001", u.flowID)
	require.Equal(t, "aea7b9e9-1e5b-4333-9ac4-8689053a77de", u.audioFlowID)
}

func TestParseMXLURLSoloCarriesNoAudio(t *testing.T) {
	u, err := parseMXLURL("mxl:///run/mxl/domain/d4d00000-0000-0000-0000-000000000001")
	require.NoError(t, err)
	require.Equal(t, "d4d00000-0000-0000-0000-000000000001", u.flowID)
	require.Empty(t, u.audioFlowID)
}

func TestParseMXLURLRejects(t *testing.T) {
	const video = "mxl:///run/mxl/domain/d4d00000-0000-0000-0000-000000000001"

	for _, ca := range []struct {
		name string
		in   string
	}{
		// The whole reason the query is validated rather than ignored: a typo
		// in the key would publish picture only, and read as the audio flow
		// being at fault.
		{"a misspelled key", video + "?audi=aea7b9e9-1e5b-4333-9ac4-8689053a77de"},
		{"an unknown key alongside a good one", video +
			"?audio=aea7b9e9-1e5b-4333-9ac4-8689053a77de&anc=c3000000-0000-0000-0000-000000000001"},
		{"an empty audio flow", video + "?audio="},
		{"two audio flows", video +
			"?audio=aea7b9e9-1e5b-4333-9ac4-8689053a77de&audio=ce343f8f-d204-4e3c-b843-5222623a292b"},
		// Joining a flow to itself would open two readers on one flow and
		// publish it as both tracks.
		{"a flow joined to itself", video + "?audio=d4d00000-0000-0000-0000-000000000001"},
		{"the wrong scheme", "rtsp://host/path"},
		{"a host", "mxl://host/domain/flow"},
		{"no domain", "mxl:///flow"},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, err := parseMXLURL(ca.in)
			require.Error(t, err)
		})
	}
}

func TestAlignStartsPutsBothFlowsAtOneInstant(t *testing.T) {
	// 50 fps video against 48 kHz audio: 960 samples to a frame, so the two
	// index spaces advance at wildly different rates and neither number means
	// anything to the other.
	videoRate := mxl.Rational{Num: 50, Den: 1}
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	// Start them deliberately out of step, the audio well ahead in wall-clock
	// terms, and check the alignment lands both on the same instant. Two
	// seconds of skew only aligns if the audio ring still holds the instant
	// the video is at, so the ring here is sized for the skew the test
	// introduces rather than for a live pair.
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)
	require.NotEqual(t, mxl.UndefinedIndex, videoIdx)
	require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

	v, a, err := alignStarts(videoRate, audioRate, videoIdx-100, audioIdx,
		testVideoRing, 3*testAudioRing)
	require.NoError(t, err)

	vTS := mxl.IndexToTimestamp(videoRate, v)
	aTS := mxl.IndexToTimestamp(audioRate, a)

	// Within one video frame of each other: the video index is the coarser of
	// the two, so it cannot land closer than its own period.
	const frameNs = int64(20_000_000)
	diff := int64(vTS) - int64(aTS)
	if diff < 0 {
		diff = -diff
	}
	require.Less(t, diff, frameNs,
		"aligned starts are %d ns apart, more than one 50 fps frame", diff)
}

func TestBackOff(t *testing.T) {
	require.Equal(t, uint64(90), backOff(100, 10))
	// A flow that has only just started has nothing to back off into, and
	// underflowing would ask for an index near the top of the range.
	require.Equal(t, uint64(5), backOff(5, 10))
	require.Equal(t, uint64(10), backOff(10, 10))
}

// Ring sizes a flow of each kind plausibly holds. Alignment is only valid
// inside them, so the tests have to name them rather than assume unlimited
// history.
const (
	testVideoRing = 300
	testAudioRing = 48000
)

// Both flows are live and their heads sit at the same instant, which is the
// case alignment exists for. Neither reader may be placed past the start it
// would have used alone: that is data the writer has not produced, and the
// read blocks on it forever rather than returning short.
func TestAlignStartsNeverPlacesAReaderPastItsOwnStart(t *testing.T) {
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	for _, ca := range []struct {
		name      string
		videoRate mxl.Rational
	}{
		{"50 fps", mxl.Rational{Num: 50, Den: 1}},
		{"60 fps", mxl.Rational{Num: 60, Den: 1}},
		// Not a whole number of samples per frame, so the two index spaces
		// never line up exactly and the arithmetic has to carry the remainder.
		{"59.94 fps", mxl.Rational{Num: 60000, Den: 1001}},
		{"29.97 fps", mxl.Rational{Num: 30000, Den: 1001}},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoIdx := mxl.CurrentIndex(ca.videoRate)
			audioIdx := mxl.CurrentIndex(audioRate)
			require.NotEqual(t, mxl.UndefinedIndex, videoIdx)
			require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

			// What each solo path would use: one grain for video, a window
			// for audio, which is a longer back-off in wall-clock terms.
			videoStart := backOff(videoIdx, 1)
			audioStart := backOff(audioIdx, audioSafetyMargin)

			v, a, err := alignStarts(ca.videoRate, audioRate, videoStart, audioStart,
				testVideoRing, testAudioRing)
			require.NoError(t, err)

			require.LessOrEqual(t, v, videoStart,
				"video start %d is past the %d the flow would have used alone", v, videoStart)
			require.LessOrEqual(t, a, audioStart,
				"audio start %d is past the %d the flow would have used alone; "+
					"the reader would wait on samples the writer has not written", a, audioStart)
		})
	}
}

// The alignment still has to do its job: both readers begin at one instant,
// not merely at a safe one.
func TestAlignStartsLandsBothOnOneInstant(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)
	require.NotEqual(t, mxl.UndefinedIndex, videoIdx)
	require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

	v, a, err := alignStarts(videoRate, audioRate, backOff(videoIdx, 1),
		backOff(audioIdx, audioSafetyMargin), testVideoRing, testAudioRing)
	require.NoError(t, err)

	vTS := mxl.IndexToTimestamp(videoRate, v)
	aTS := mxl.IndexToTimestamp(audioRate, a)
	diff := int64(vTS) - int64(aTS)
	if diff < 0 {
		diff = -diff
	}
	// One video frame is as close as the coarser of the two index spaces can
	// land.
	require.Less(t, diff, int64(time.Second/60),
		"aligned starts are %d ns apart, more than one 60 fps frame", diff)
}

// A flow whose head trails the other by more than its ring holds cannot be
// aligned to it: the instant one has reached, the other overwrote. Reporting
// that beats returning an index whose data is gone.
func TestAlignStartsRejectsAFlowLaggingBeyondItsRing(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)

	// Half a minute of audio behind the video, which is far outside a ring
	// holding one second.
	lagging := audioIdx - 30*48000

	_, _, err := alignStarts(videoRate, audioRate, backOff(videoIdx, 1),
		backOff(lagging, audioSafetyMargin), testVideoRing, testAudioRing)
	require.ErrorIs(t, err, errNoCommonStart)
}

// A flow counting from its own creation and one counting from the ST 2059
// epoch are each valid alone and name no instant in common. Deriving one from
// the other puts a reader decades past its head, where it waits out the
// timeout having produced nothing, and the whole path is torn down.
func TestAlignStartsRejectsFlowsOnDifferentEpochs(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	videoIdx := mxl.CurrentIndex(videoRate)
	require.NotEqual(t, mxl.UndefinedIndex, videoIdx)

	// An hour of samples: a flow that has been running an hour, counting from
	// zero rather than from the epoch.
	const sinceCreation = 3600 * 48000

	_, _, err := alignStarts(videoRate, audioRate, backOff(videoIdx, 1),
		backOff(sinceCreation, audioSafetyMargin), testVideoRing, testAudioRing)
	require.ErrorIs(t, err, errNoCommonStart)

	// And the same the other way round, so neither ordering slips through.
	audioIdx := mxl.CurrentIndex(audioRate)
	_, _, err = alignStarts(videoRate, audioRate, backOff(3600*60, 1),
		backOff(audioIdx, audioSafetyMargin), testVideoRing, testAudioRing)
	require.ErrorIs(t, err, errNoCommonStart)
}

func TestReadable(t *testing.T) {
	// At the start the flow would have used alone, and one entry back.
	require.True(t, readable(100, 100, 10))
	require.True(t, readable(99, 100, 10))
	// Exactly the oldest entry the ring still holds, and one past it.
	require.True(t, readable(90, 100, 10))
	require.False(t, readable(89, 100, 10))
	// Past the flow's own start is data the writer has not produced.
	require.False(t, readable(101, 100, 10))
}

// A flow nothing has written to yet sits at index 0, which converts to
// timestamp 0 at every rate. That is the ordinary state of a path opened
// before its producer has started, and it must not be reported as a rate
// fault: the rates are fine and the operator sent to check them finds nothing.
func TestAlignStartsReportsAnEmptyFlowWithoutBlamingTheRate(t *testing.T) {
	videoRate := mxl.Rational{Num: 24, Den: 1}
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)

	for _, ca := range []struct {
		name  string
		video uint64
		audio uint64
	}{
		{"video has no grains", 0, backOff(audioIdx, audioSafetyMargin)},
		{"audio has no samples", backOff(videoIdx, 1), 0},
		{"neither has been written", 0, 0},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, _, err := alignStarts(videoRate, audioRate, ca.video, ca.audio,
				testVideoRing, testAudioRing)
			require.ErrorIs(t, err, errNoCommonStart)
			require.NotContains(t, err.Error(), "rate",
				"an unwritten flow is not a rate fault and must not be reported as one")
		})
	}
}

// The pairing seen in the field is 24 fps video against 48 kHz audio. Nothing
// about it is special: it maps onto the MXL clock and round-trips like any
// other, so it has to align rather than fall back.
func TestAlignStartsAlignsTheFieldRatePairings(t *testing.T) {
	audioRate := mxl.Rational{Num: 48000, Den: 1}

	for _, videoRate := range []mxl.Rational{
		{Num: 24, Den: 1},
		{Num: 24000, Den: 1001},
	} {
		t.Run(fmt.Sprintf("%d/%d", videoRate.Num, videoRate.Den), func(t *testing.T) {
			videoIdx := mxl.CurrentIndex(videoRate)
			audioIdx := mxl.CurrentIndex(audioRate)
			require.NotEqual(t, mxl.UndefinedIndex, videoIdx)

			videoStart := backOff(videoIdx, 1)
			audioStart := backOff(audioIdx, audioSafetyMargin)

			v, a, err := alignStarts(videoRate, audioRate, videoStart, audioStart,
				testVideoRing, testAudioRing)
			require.NoError(t, err)
			require.LessOrEqual(t, v, videoStart)
			require.LessOrEqual(t, a, audioStart)
		})
	}
}

// Whatever stops the two being aligned, the caller has to be able to carry on
// and play them unaligned. A failure that does not wrap errNoCommonStart is
// one runJoined would have no way to tell apart from a fatal fault, and the
// path would be torn down and retried for as long as the condition lasted.
func TestAlignStartsAlwaysLetsThePathOpen(t *testing.T) {
	audioRate := mxl.Rational{Num: 48000, Den: 1}
	videoRate := mxl.Rational{Num: 24, Den: 1}
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)

	// Rates libmxl cannot place an index on at all: it returns
	// UndefinedIndex for each of these.
	unmappable := mxl.Rational{Num: 0, Den: 1}

	for _, ca := range []struct {
		name                               string
		videoRate, audioRate               mxl.Rational
		video, audio, videoRing, audioRing uint64
	}{
		{
			"an unwritten video flow", videoRate, audioRate,
			0, backOff(audioIdx, audioSafetyMargin), testVideoRing, testAudioRing,
		},
		{
			"a video rate that does not map", unmappable, audioRate,
			backOff(videoIdx, 1), backOff(audioIdx, audioSafetyMargin), testVideoRing, testAudioRing,
		},
		{
			"an audio rate that does not map", videoRate, unmappable,
			backOff(videoIdx, 1), backOff(audioIdx, audioSafetyMargin), testVideoRing, testAudioRing,
		},
		{
			"a flow lagging beyond its ring", videoRate, audioRate,
			backOff(videoIdx, 1), backOff(audioIdx-30*48000, audioSafetyMargin), testVideoRing, testAudioRing,
		},
		{
			"flows on different epochs", videoRate, audioRate,
			backOff(videoIdx, 1), backOff(3600*48000, audioSafetyMargin), testVideoRing, testAudioRing,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, _, err := alignStarts(ca.videoRate, ca.audioRate, ca.video, ca.audio,
				ca.videoRing, ca.audioRing)
			require.ErrorIs(t, err, errNoCommonStart,
				"every alignment failure has to be one the caller can play through")
		})
	}
}
