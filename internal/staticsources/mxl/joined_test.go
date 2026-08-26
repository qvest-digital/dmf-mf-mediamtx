package mxl

import (
	"testing"

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
	// terms, and check the alignment lands both on the same instant.
	videoIdx := mxl.CurrentIndex(videoRate)
	audioIdx := mxl.CurrentIndex(audioRate)
	require.NotEqual(t, mxl.UndefinedIndex, videoIdx)
	require.NotEqual(t, mxl.UndefinedIndex, audioIdx)

	v, a, err := alignStarts(videoRate, audioRate, videoIdx-100, audioIdx)
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
