package mxl

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixturePackets is what ffprobe counts in the fixture, which is half a
// second of stereo 48 kHz encoded by the same libopus build the runtime
// image carries. Opus frames the fixture at 20 ms, so 25 packets cover the
// audio and one more carries the encoder's pre-skip.
const fixturePackets = 26

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sine-48k-stereo.opus.ogg")
	require.NoError(t, err)
	return b
}

// The extractor is fed from a pipe, so it must produce the same packets
// whatever the read boundaries fall on. One byte at a time is the pathological
// case: every page header, segment table and packet spans a call.
func TestOggOpusExtractorIsChunkIndependent(t *testing.T) {
	data := fixture(t)

	var want [][]byte
	for _, chunk := range []int{len(data), 4096, 251, 64, 7, 1} {
		e := &oggOpusExtractor{}
		var got [][]byte
		for off := 0; off < len(data); off += chunk {
			end := min(off+chunk, len(data))
			pkts, err := e.push(data[off:end])
			require.NoError(t, err)
			got = append(got, pkts...)
		}

		require.Len(t, got, fixturePackets, "chunk size %d", chunk)
		if want == nil {
			want = got
			continue
		}
		require.Equal(t, want, got, "chunk size %d changed the packets", chunk)
	}
}

// OpusHead and OpusTags describe the stream rather than carrying audio. RTP
// carries neither, and passing one on as a payload puts a decoder into an
// error it does not recover from.
func TestOggOpusExtractorDropsTheHeaderPackets(t *testing.T) {
	e := &oggOpusExtractor{}
	got, err := e.push(fixture(t))
	require.NoError(t, err)
	require.Len(t, got, fixturePackets)

	for i, p := range got {
		require.False(t, bytes.HasPrefix(p, []byte("OpusHead")), "packet %d is OpusHead", i)
		require.False(t, bytes.HasPrefix(p, []byte("OpusTags")), "packet %d is OpusTags", i)
		require.NotEmpty(t, p, "packet %d is empty", i)
	}
}

// A segment of exactly 255 bytes continues into the next, so a packet longer
// than that spans segments and a packet whose last segment ends a page spans
// pages. Neither boundary may split a packet.
func TestOggOpusExtractorJoinsContinuedPackets(t *testing.T) {
	// One packet of 300 bytes: segments 255 + 45.
	body := bytes.Repeat([]byte{0xAB}, 300)
	page := oggPage(t, []byte{255, 45}, body)

	e := &oggOpusExtractor{headersSeen: 2} // past the headers
	got, err := e.push(page)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, body, got[0])
}

func TestOggOpusExtractorSpansPages(t *testing.T) {
	// A packet whose final segment is the last on its page continues onto
	// the next page and is only complete there.
	first := oggPage(t, []byte{255}, bytes.Repeat([]byte{0x01}, 255))
	second := oggPage(t, []byte{10}, bytes.Repeat([]byte{0x02}, 10))

	e := &oggOpusExtractor{headersSeen: 2}
	got, err := e.push(first)
	require.NoError(t, err)
	require.Empty(t, got, "a packet continuing onto the next page is not complete yet")

	got, err = e.push(second)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0], 265)
}

// oggPage builds a minimal page. Only the capture pattern, the segment count
// and the table are read, so the rest is left zero.
func oggPage(t *testing.T, table []byte, body []byte) []byte {
	t.Helper()
	total := 0
	for _, l := range table {
		total += int(l)
	}
	require.Equal(t, total, len(body), "table does not describe the body")

	p := make([]byte, oggHeaderLen, oggHeaderLen+len(table)+len(body))
	copy(p, oggCapturePattern)
	p[oggSegCountOffset] = byte(len(table))
	p = append(p, table...)
	p = append(p, body...)
	return p
}
