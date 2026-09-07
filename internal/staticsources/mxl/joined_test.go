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

func TestBackOff(t *testing.T) {
	require.Equal(t, uint64(90), backOff(100, 10))
	// A flow that has only just started has nothing to back off into, and
	// underflowing would ask for an index near the top of the range.
	require.Equal(t, uint64(5), backOff(5, 10))
	require.Equal(t, uint64(10), backOff(10, 10))
}

// The rate every audio flow here runs at.
const audioRate48k = 48000

var audio48k = mxl.Rational{Num: audioRate48k, Den: 1}

// The case the shared origin exists for: two live flows whose heads name the
// same moment, so one instant converts into both index spaces.
func TestPathEpochPutsBothFlowsOnOneInstant(t *testing.T) {
	for _, videoRate := range []mxl.Rational{
		{Num: 24, Den: 1},
		{Num: 50, Den: 1},
		{Num: 60, Den: 1},
		// Not a whole number of samples per frame, so the two index spaces
		// never line up exactly and the arithmetic has to carry the remainder.
		{Num: 24000, Den: 1001},
		{Num: 30000, Den: 1001},
		{Num: 60000, Den: 1001},
	} {
		t.Run(fmt.Sprintf("%d/%d", videoRate.Num, videoRate.Den), func(t *testing.T) {
			videoIdx := mxl.CurrentIndex(videoRate)
			audioIdx := mxl.CurrentIndex(audio48k)
			require.NotEqual(t, mxl.UndefinedIndex, videoIdx)
			require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

			// What each track would start reading at on its own: one grain for
			// video, a read window for audio.
			videoStart := backOff(videoIdx, 1)
			audioStart := backOff(audioIdx, audioSafetyMargin)

			ns, v, a := pathEpoch(videoRate, audio48k, videoStart, audioStart)
			require.NotZero(t, a, "two live flows have to share an origin")

			// The origin has to be one instant, whichever index space it is
			// read back through. One video frame is as close as the coarser of
			// the two can land.
			vNs := mxl.IndexToTimestamp(videoRate, v)
			aNs := mxl.IndexToTimestamp(audio48k, a)
			frame := uint64(int64(time.Second) * videoRate.Den / videoRate.Num)
			require.Less(t, skewNs(vNs, aNs), frame,
				"the two epochs are %d ns apart, more than one frame", skewNs(vNs, aNs))
			require.Less(t, skewNs(ns, vNs), frame)

			// Neither track may be placed past where it starts reading: its
			// first packet would then carry a timestamp before zero.
			require.LessOrEqual(t, v, videoStart)
			require.LessOrEqual(t, a, audioStart)
		})
	}
}

// The case measured on an ST 2110 source whose audio essence stopped while its
// video ran at sixty grains a second throughout: the audio head stays ten
// minutes old. Anchoring the video timeline to it would put the first frame
// ten minutes in, so the video takes its own head and the audio is left
// without an origin until its flow catches up.
func TestPathEpochRefusesAStaleAudioHead(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audio48k)

	videoStart := backOff(videoIdx, 1)
	stale := backOff(audioIdx-600*audioRate48k, audioSafetyMargin)

	ns, v, a := pathEpoch(videoRate, audio48k, videoStart, stale)
	require.Zero(t, a, "a head ten minutes old is not a view of the video's moment")
	require.Equal(t, videoStart, v,
		"the video track keeps its own head as the origin")
	require.Equal(t, mxl.IndexToTimestamp(videoRate, videoStart), ns,
		"the path's origin is the video's, so a recovered audio flow can be "+
			"placed on it later")
}

// Whatever the audio flow is doing, the video track has to come away with an
// origin: it owns the path, and a path that cannot open is the fault this
// whole arrangement exists to avoid.
func TestPathEpochAlwaysGivesTheVideoAnOrigin(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audio48k)
	videoStart := backOff(videoIdx, 1)

	for _, ca := range []struct {
		name      string
		audioRate mxl.Rational
		audio     uint64
	}{
		// The ordinary state of a path opened before its producer started.
		{"an unwritten audio flow", audio48k, 0},
		// A rate libmxl cannot place an index on at all.
		{
			"an audio rate that does not map",
			mxl.Rational{Num: 0, Den: 1},
			backOff(audioIdx, audioSafetyMargin),
		},
		// A flow counting from its own creation rather than from the ST 2059
		// epoch: valid alone, and naming no instant the video also names.
		{"an audio flow on another epoch", audio48k, 3600 * audioRate48k},
		// An index that has run ahead of real time, which is what a writer
		// leaves behind when it charges a silent stretch to loss and skips
		// the position past it.
		{"an audio index ahead of the clock", audio48k, audioIdx + 1800*audioRate48k},
	} {
		t.Run(ca.name, func(t *testing.T) {
			ns, v, a := pathEpoch(videoRate, ca.audioRate, videoStart, ca.audio)
			require.Equal(t, videoStart, v)
			require.NotZero(t, ns)
			require.Zero(t, a)
		})
	}
}

// A video flow nothing has written to yet has no origin to give, so the path
// has nothing to stamp either track from and both fall back to what they read
// first.
func TestPathEpochOnAnUnwrittenVideoFlow(t *testing.T) {
	ns, v, a := pathEpoch(mxl.Rational{Num: 60, Den: 1}, audio48k, 0,
		backOff(mxl.CurrentIndex(audio48k), audioSafetyMargin))
	require.Zero(t, ns)
	require.Zero(t, v)
	require.Zero(t, a)
}

// Two heads a hair apart are one moment, and the origin has to be the earlier
// of them: taking the later would place the other track past its own head,
// where its first packet carries a timestamp before zero.
func TestPathEpochTakesTheEarlierHead(t *testing.T) {
	videoRate := mxl.Rational{Num: 60, Den: 1}
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audio48k)

	videoStart := backOff(videoIdx, 1)
	// A hundred milliseconds of audio behind the video, inside the tolerance.
	audioStart := backOff(audioIdx-audioRate48k/10, audioSafetyMargin)

	ns, v, a := pathEpoch(videoRate, audio48k, videoStart, audioStart)
	require.NotZero(t, a)
	require.LessOrEqual(t, ns, mxl.IndexToTimestamp(videoRate, videoStart))
	require.LessOrEqual(t, v, videoStart)
	require.LessOrEqual(t, a, audioStart)
}

// The regression the supervisor exists for. An audio flow that stopped and
// came back has to be placed on the origin the path has been stamping video
// from all along, not at zero: starting it over would publish a timeline
// running minutes behind the picture beside it.
func TestAudioEpochAtPlacesARecoveredFlowOnThePathsOrigin(t *testing.T) {
	audioIdx := mxl.CurrentIndex(audio48k)
	require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

	// The path opened ten minutes ago and the flow has only now caught up.
	head := backOff(audioIdx, audioSafetyMargin)
	origin := head - 600*audioRate48k
	epochNs := mxl.IndexToTimestamp(audio48k, origin)

	got := audioEpochAt(audio48k, epochNs, mxl.FlowRuntime{HeadIndex: audioIdx})
	require.Equal(t, origin, got,
		"the origin is an instant, so it converts whether or not the ring "+
			"still holds it")
}

func TestAudioEpochAtRefusesWhatItCannotPlace(t *testing.T) {
	audioIdx := mxl.CurrentIndex(audio48k)
	head := backOff(audioIdx, audioSafetyMargin)
	headNs := mxl.IndexToTimestamp(audio48k, head)

	for _, ca := range []struct {
		name    string
		epochNs uint64
		rt      mxl.FlowRuntime
	}{
		// The path named no origin, so there is nothing to place.
		{"no origin", 0, mxl.FlowRuntime{HeadIndex: audioIdx}},
		// A flow that has not reached the origin: its first sample would
		// carry a timestamp before zero.
		{
			"a head before the origin", headNs + uint64(time.Second),
			mxl.FlowRuntime{HeadIndex: audioIdx},
		},
		// An index half an hour past the MXL clock has run away from real
		// time; placing the track on the origin would put its first packet
		// half an hour into the timeline.
		{
			"an index ahead of the clock", headNs,
			mxl.FlowRuntime{HeadIndex: audioIdx + 1800*audioRate48k},
		},
		// Nothing has been written, so there is no head to compare.
		{"an unwritten flow", headNs, mxl.FlowRuntime{HeadIndex: 0}},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Zero(t, audioEpochAt(audio48k, ca.epochNs, ca.rt))
		})
	}
}
