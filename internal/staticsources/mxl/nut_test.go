package mxl

import (
	"bytes"
	"io"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// nutVBytes encodes a NUT variable-length unsigned integer.
func nutVBytes(x uint64) []byte {
	b := []byte{byte(x & 0x7f)}
	for x >>= 7; x > 0; x >>= 7 {
		b = append([]byte{byte(x&0x7f) | 0x80}, b...)
	}
	return b
}

func nutVs(xs ...uint64) []byte {
	var b []byte
	for _, x := range xs {
		b = append(b, nutVBytes(x)...)
	}
	return b
}

// nutPacket frames a body as a startcode packet with a zero checksum.
func nutPacket(startcode uint64, body []byte) []byte {
	var b []byte
	for i := 7; i >= 0; i-- {
		b = append(b, byte(startcode>>(8*i)))
	}
	body = append(body, 0, 0, 0, 0)
	b = append(b, nutVBytes(uint64(len(body)))...)
	return append(b, body...)
}

func TestNUTReaderReadsFrames(t *testing.T) {
	// Main header, version 3, one stream, one time base. Frame codes 0-128
	// ('N' excluded) carry their size in a coded MSB; 129 onwards elide
	// header 1 and carry a reserved field. Each code's size LSB is its
	// position in its group, so 0 and 129 have none.
	main := nutVs(3, 1, 65536, 1, 1, 25)
	main = append(main, nutVs(nutFlagSizeMSB, 6, 0, 1, 0, 0, 0, 128)...)
	main = append(main, nutVs(nutFlagSizeMSB|nutFlagReserved, 8, 0, 1, 0, 0, 0, 127, 0, 1)...)
	main = append(main, nutVs(1, 3)...)
	main = append(main, 0, 0, 1)

	frame1 := []byte{0, 0, 1, 0x65, 0x88, 0xaa}
	frame2 := []byte{0, 0, 1, 0x41, 0x9a, 0xbb, 0xcc}
	stream := []byte(nutIDString)
	stream = append(stream, nutPacket(nutMainStartcode, main)...)
	// A stream header, skipped whole.
	stream = append(stream, nutPacket(0x11405BF2F9DB+uint64('N'<<8+'S')<<48, nutVs(0, 0))...)
	stream = append(stream, 0)
	stream = append(stream, nutVBytes(uint64(len(frame1)))...)
	stream = append(stream, frame1...)
	// Code 129: header 1 (00 00 01) elided, one reserved value, size counts
	// the elided bytes.
	stream = append(stream, 129)
	stream = append(stream, nutVs(uint64(len(frame2)), 1, 42)...)
	stream = append(stream, frame2[3:]...)

	r := newNUTReader(bytes.NewReader(stream))
	got, err := r.next()
	require.NoError(t, err)
	require.Equal(t, frame1, got)
	got, err = r.next()
	require.NoError(t, err)
	require.Equal(t, frame2, got)
	_, err = r.next()
	require.ErrorIs(t, err, io.EOF)

	require.Equal(t, [][]byte{{0x65, 0x88, 0xaa}, {0x41, 0x9a, 0xbb, 0xcc}},
		append(splitAnnexB(frame1), splitAnnexB(frame2)...))
}

func TestNUTReaderRejectsOtherStreams(t *testing.T) {
	_, err := newNUTReader(bytes.NewReader([]byte("\x00\x00\x00\x01\x67not a nut stream at all"))).next()
	require.Error(t, err)
}

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
			if typ := n[0] & 0x1F; typ == 1 || typ == 5 {
				slices++
			}
		}
		require.Equal(t, encoderThreads, slices, "frame %d", got)
		got++
	}
	require.Equal(t, frames, got)
}
