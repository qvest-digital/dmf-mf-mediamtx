package mxl

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"
)

// The packetizer and the format it is published under have to agree. A
// receiver reads the mode out of the fmtp and depacketizes accordingly, so a
// mismatch is not a startup error but a stream nobody can decode.
func TestRTPEncoderMatchesTheAdvertisedFormat(t *testing.T) {
	enc, err := newRTPEncoder(1450)
	require.NoError(t, err)

	h264, ok := videoMedia().Formats[0].(*format.H264)
	require.True(t, ok, "the video track is not H264")

	require.Equal(t, h264.PacketizationMode, enc.PacketizationMode)
	require.Equal(t, h264.PayloadTyp, enc.PayloadType)
}

// gortsplib rejects any mode but 1, and used to accept the zero value. Left
// unset the encoder therefore stopped initialising on a dependency bump, with
// every video preview failing at startup and nothing in the tree to catch it.
func TestRTPEncoderInitialises(t *testing.T) {
	enc, err := newRTPEncoder(1450)
	require.NoError(t, err)
	require.Equal(t, 1, enc.PacketizationMode)
}
