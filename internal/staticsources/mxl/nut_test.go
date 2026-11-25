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

// nutPacket frames a body as a startcode packet. Checksums are zero; the
// reader skips them.
func nutPacket(startcode uint64, body []byte) []byte {
	var b []byte
	for i := 7; i >= 0; i-- {
		b = append(b, byte(startcode>>(8*i)))
	}
	body = append(body, 0, 0, 0, 0)
	b = append(b, nutVBytes(uint64(len(body)))...)
	if len(body) > 4096 {
		b = append(b, 0, 0, 0, 0) // header checksum
	}
	return append(b, body...)
}

const nutInfoStartcode = 0xAB68B596BA78 + uint64('N'<<8+'I')<<48

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

func TestNUTReaderFrameHeaderFields(t *testing.T) {
	// Group 1 sets the size multiplier to 4 and elision header 1 for code 0.
	// Group 2 sets only the pts field, so codes 1-4 keep both, and its
	// flags make the frame header carry coded flags.
	main := nutVs(3, 1, 65536, 1, 1, 25)
	main = append(main, nutVs(nutFlagSizeMSB, 8, 0, 4, 0, 0, 0, 1, 0, 1)...)
	main = append(main, nutVs(nutFlagSizeMSB|nutFlagCoded, 1, 0)...)
	main = append(main, nutVs(nutFlagSizeMSB, 6, 0, 1, 0, 0, 0, 250)...)
	main = append(main, nutVs(1, 3)...)
	main = append(main, 0, 0, 1)

	stream := []byte(nutIDString)
	stream = append(stream, nutPacket(nutMainStartcode, main)...)
	// An info packet above 4096 bytes carries a header checksum.
	stream = append(stream, nutPacket(nutInfoStartcode, make([]byte, 5000))...)

	// Code 0: size 4*2 = 8, three of them elided.
	stream = append(stream, 0)
	stream = append(stream, nutVs(2)...)
	stream = append(stream, 0x65, 0x88, 1, 2, 3)
	// Code 2: coded flags add a frame checksum; size LSB 1 (its place in
	// group 2) + 4*1 = 5, three elided, multiplier and header carried over
	// from group 1.
	stream = append(stream, 2)
	stream = append(stream, nutVs(nutFlagChecksum, 1)...)
	stream = append(stream, 9, 9, 9, 9)
	stream = append(stream, 0x41, 0x9a)
	// Code 1: a frame above 4096 bytes has no elided header.
	big := make([]byte, 4*1025)
	copy(big, []byte{0, 0, 1, 0x41, 0x9a})
	stream = append(stream, 1)
	stream = append(stream, nutVs(0, 1025)...)
	stream = append(stream, big...)

	r := newNUTReader(bytes.NewReader(stream))
	for _, want := range [][]byte{
		{0, 0, 1, 0x65, 0x88, 1, 2, 3},
		{0, 0, 1, 0x41, 0x9a},
		big,
	} {
		got, err := r.next()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestNUTReaderRejectsCorruptMainHeader(t *testing.T) {
	// A field count no packet could hold must fail, not allocate it.
	main := nutVs(3, 1, 65536, 1, 1, 25)
	main = append(main, nutVs(nutFlagSizeMSB, 1<<40)...)
	stream := append([]byte(nutIDString), nutPacket(nutMainStartcode, main)...)
	_, err := newNUTReader(bytes.NewReader(stream)).next()
	require.ErrorContains(t, err, "frame code fields")
}

func TestNUTReaderRejectsOtherStreams(t *testing.T) {
	_, err := newNUTReader(bytes.NewReader([]byte("\x00\x00\x00\x01\x67not a nut stream at all"))).next()
	require.Error(t, err)
}

func TestNUTReaderMatchesFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	// Noise at a high rate makes frames larger than 64 KiB, which ffmpeg
	// writes with a frame checksum and coded flags, as a busy 1080p preview
	// does.
	const frames = 26
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25,noise=alls=40:allf=t", "-frames:v", "26",
		"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency", "-threads", "4",
		"-x264-params", "sliced-threads=1", "-g", "13", "-b:v", "40M",
		"-bsf:v", "dump_extra=freq=keyframe", "-f", "nut", "pipe:1").Output()
	require.NoError(t, err)

	// ffmpeg's own demuxer, remuxed to a raw stream, is the reference.
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "nut", "-i", "pipe:0", "-c", "copy", "-f", "h264", "pipe:1")
	cmd.Stdin = bytes.NewReader(out)
	want, err := cmd.Output()
	require.NoError(t, err)

	r := newNUTReader(bytes.NewReader(out))
	var got []byte
	var n, large int
	for {
		frame, err := r.next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		nals := splitAnnexB(frame)
		require.NotEmpty(t, nals)
		if n%13 == 0 {
			// dump_extra puts SPS and PPS in front of every IDR.
			require.Equal(t, byte(7), nals[0][0]&0x1F, "frame %d", n)
		}
		slices := 0
		for _, nal := range nals {
			if typ := nal[0] & 0x1F; typ == 1 || typ == 5 {
				slices++
			}
		}
		require.Equal(t, encoderThreads, slices, "frame %d", n)
		if len(frame) > 64<<10 {
			large++
		}
		got = append(got, frame...)
		n++
	}
	require.Equal(t, frames, n)
	require.Positive(t, large, "no frame above 64 KiB")
	require.Equal(t, want, got)
}
