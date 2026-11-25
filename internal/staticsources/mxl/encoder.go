package mxl

import (
	"fmt"
	"io"
	"strconv"
	"time"
)

// EncoderParams configures the H264Encoder. Fields mirror the mxlH264* path
// configuration keys.
type EncoderParams struct {
	FFmpegPath string
	Width      uint32
	Height     uint32
	// RateNum/RateDen carry the flow's grain rate as the exact fraction the
	// flow declares. ffmpeg takes a fraction, so a 60000/1001 flow is encoded
	// as 59.94 rather than as the 60 an integer field would round it to; the
	// rounded value reaches the SPS and x264's rate control, both of which
	// then describe a stream the flow does not produce.
	RateNum   int64
	RateDen   int64
	Preset    string // x264 preset, e.g. "veryfast"
	Profile   string // "baseline" | "main" | "high"
	Bitrate   uint32 // target bitrate in bits/sec; 0 means use ffmpeg/x264 default
	IDRPeriod uint32 // distance between IDR frames in frames; 0 derives it from the rate
	// OutHeight scales the picture to this height, the width following the
	// aspect ratio; 0 or a height not below the input's keeps the input's.
	OutHeight uint32

	// OnData is invoked for every encoded access unit, in input order, from
	// the encoder's reader goroutine. The callback may block (write to a
	// substream, log, etc.); blocking propagates back as natural
	// backpressure on the encoder pipe.
	OnData func(au [][]byte)

	// OnFrameOut, when set, is called as a picture's output starts arriving
	// from ffmpeg.
	OnFrameOut func(t time.Time)
}

// H264Encoder pipes planar YUV 4:2:0 frames into an ffmpeg subprocess and
// invokes EncoderParams.OnData for each H.264 access unit read back from
// stdout. Talking to ffmpeg over pipes (rather than linking libx264 directly)
// keeps any GPL bits out of the mediamtx binary and lets the operator pick a
// different encoder build at deploy time.
//
// Because the end of a picture is only known once the start code of the next
// picture has been read, there is an inherent one-frame latency between
// Encode and the corresponding OnData call. x264 itself holds no frame; see
// encoderThreads.
type H264Encoder struct {
	*ffmpegProcess
	params EncoderParams

	yuvBuf []byte
	ySize  int
	cSize  int
}

// NewH264Encoder starts an ffmpeg process configured to take raw yuv420p
// frames on stdin and emit H.264 Annex-B on stdout. Encoded access units are
// delivered via p.OnData. ffmpeg's stderr is inherited from the parent
// process so warnings surface where the operator expects them.
func NewH264Encoder(p EncoderParams) (*H264Encoder, error) {
	if p.Width%2 != 0 || p.Height%2 != 0 {
		return nil, fmt.Errorf("dimensions must be even, got %dx%d", p.Width, p.Height)
	}
	if p.RateNum <= 0 || p.RateDen <= 0 {
		return nil, fmt.Errorf("grain rate must be positive, got %d/%d", p.RateNum, p.RateDen)
	}
	if p.OnData == nil {
		return nil, fmt.Errorf("OnData callback is required")
	}
	if p.FFmpegPath == "" {
		p.FFmpegPath = "ffmpeg"
	}

	proc, err := startFFmpeg(p.FFmpegPath, buildFFmpegArgs(p))
	if err != nil {
		return nil, err
	}

	cSize := (int(p.Width) / 2) * (int(p.Height) / 2)
	e := &H264Encoder{
		ffmpegProcess: proc,
		params:        p,
		yuvBuf:        make([]byte, int(p.Width)*int(p.Height)+2*cSize),
		ySize:         int(p.Width) * int(p.Height),
		cSize:         cSize,
	}
	proc.readAll = e.readAUs
	if encoderNUT {
		proc.readAll = e.readNUT
	}
	go proc.run()
	return e, nil
}

// defaultIDRPeriod returns half a second of frames, rounded.
//
// Not one second. A path is consumed over HLS as well as over WebRTC, and the
// HLS muxer closes a segment on the first IDR at or past the segment target.
// A GOP exactly the length of that target therefore lands on the comparison
// itself, and a fraction of a millisecond decides whether the segment closes
// now or a whole GOP later: measured on a 30/1 flow, segments alternated
// between 1.00 s and 2.00 s, and every jump moves the live edge a player has
// to re-seek to. Half a second of frames bounds the overshoot to half a
// second, whatever the segment target is set to.
func defaultIDRPeriod(rateNum, rateDen int64) uint32 {
	idr := (rateNum + rateDen) / (2 * rateDen)
	if idr < 1 {
		return 1
	}
	return uint32(idr)
}

// encoderThreads is the number of slices x264 cuts each picture into, one
// thread each.
//
// Sliced threads encode the slices of one picture in parallel and hold no
// frame back. Frame threading, x264's default, holds threads-1 frames before
// the first access unit comes back whatever the picture costs to encode:
// four frame threads measured 135 ms from pipe to output at 1080p25, four
// slices 14.5 ms, at the same CPU and the same browser-visible quality at
// 5 Mbit/s.
//
// One thread is as fast at 1080p but cannot keep up with 2160p, which a
// Xeon Gold 6430 core encodes at 18 to 23 fps. Every slice boundary costs
// some compression, so the count stays at four rather than following the
// host's cores.
const encoderThreads = 4

// buildFFmpegArgs assembles the ffmpeg command line from params. Kept
// separate to keep NewH264Encoder readable and to make these knobs easy to
// inspect in tests.
func buildFFmpegArgs(p EncoderParams) []string {
	idr := p.IDRPeriod
	if idr == 0 {
		idr = defaultIDRPeriod(p.RateNum, p.RateDen)
	}

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// Input: raw YUV from our pipe.
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", p.Width, p.Height),
		"-r", fmt.Sprintf("%d/%d", p.RateNum, p.RateDen),
		"-i", "pipe:0",
	}
	if p.OutHeight > 0 && p.OutHeight < p.Height {
		// -2 keeps the width even, which 4:2:0 needs.
		args = append(args, "-vf", fmt.Sprintf("scale=-2:%d", p.OutHeight))
	}
	args = append(args,
		// Encoder.
		"-c:v", "libx264",
		"-preset", p.Preset,
		"-threads", strconv.Itoa(encoderThreads),
		"-tune", "zerolatency",
		"-profile:v", p.Profile,
		"-pix_fmt", "yuv420p",
		"-g", strconv.FormatUint(uint64(idr), 10),
		// Sliced threads, which tune=zerolatency also sets; see
		// encoderThreads. scenecut=0 keeps IDRs strictly at -g intervals so
		// late-joining readers recover predictably.
		"-x264-params", "sliced-threads=1:scenecut=0",
		// Repeat SPS/PPS at every IDR so late-joining RTSP readers can decode.
		"-bsf:v", "dump_extra=freq=keyframe",
	)

	if p.Bitrate > 0 {
		args = append(args,
			"-b:v", strconv.FormatUint(uint64(p.Bitrate), 10),
			"-maxrate", strconv.FormatUint(uint64(p.Bitrate), 10),
			"-bufsize", strconv.FormatUint(uint64(p.Bitrate)*2, 10),
		)
	}

	format := "h264"
	if encoderNUT {
		format = "nut"
	}
	args = append(args,
		"-f", format,
		"-flush_packets", "1",
		"pipe:1",
	)
	return args
}

// Encode writes a frame to the encoder. The slices are fully consumed before
// the call returns; the caller may reuse the underlying buffers.
func (e *H264Encoder) Encode(y, cb, cr []byte) error {
	if len(y) != e.ySize {
		return fmt.Errorf("y plane size %d != %d", len(y), e.ySize)
	}
	if len(cb) != e.cSize || len(cr) != e.cSize {
		return fmt.Errorf("chroma plane size %d/%d != %d", len(cb), len(cr), e.cSize)
	}

	copy(e.yuvBuf[:e.ySize], y)
	copy(e.yuvBuf[e.ySize:e.ySize+e.cSize], cb)
	copy(e.yuvBuf[e.ySize+e.cSize:], cr)

	return e.write(e.yuvBuf)
}

// readAUs parses ffmpeg's stdout into Annex-B NAL units, groups them into
// access units, and invokes the OnData callback for each complete AU.
//
// AU grouping rule: a picture is several VCL NALs (types 1=non-IDR slice or
// 5=IDR slice), one per slice, preceded by any SPS/PPS/SEI that
// `-bsf:v dump_extra=freq=keyframe` puts in front of an IDR. An AU that holds
// a slice is complete, and the callback fires, as soon as the head of the
// next NAL shows a non-VCL NAL or the first slice of the next picture.
//
// That head only arrives with the next picture's output, so the first AU
// emerges only after the second frame is pushed.
func (e *H264Encoder) readAUs() error {
	parseBuf := make([]byte, 0, 1<<17)
	readBuf := make([]byte, 8192)
	var currentAU [][]byte
	var auHasSlice, headStamped bool

	for {
		n, err := e.stdout.Read(readBuf)
		now := time.Now()
		if n > 0 {
			parseBuf = append(parseBuf, readBuf[:n]...)
			for {
				sc1, sc1Len := findStartCode(parseBuf)
				if sc1 < 0 {
					break
				}
				start := sc1 + sc1Len
				if !headStamped && e.params.OnFrameOut != nil && start+1 < len(parseBuf) {
					if isSlice(parseBuf[start:]) && parseBuf[start+1]&0x80 != 0 {
						e.params.OnFrameOut(now)
					}
					headStamped = true
				}
				// The next NAL's first two bytes decide whether the picture
				// before it is complete, so it is handed on before the rest
				// of that NAL arrives.
				if auHasSlice && start+1 < len(parseBuf) && startsPicture(parseBuf[start:]) {
					e.params.OnData(currentAU)
					currentAU, auHasSlice = nil, false
				}
				sc2, _ := findStartCode(parseBuf[start:])
				if sc2 < 0 {
					if sc1 > 0 {
						parseBuf = parseBuf[sc1:]
					}
					break
				}
				end := start + sc2
				nal := make([]byte, end-start)
				copy(nal, parseBuf[start:end])
				headStamped = false
				currentAU = append(currentAU, nal)
				auHasSlice = auHasSlice || isSlice(nal)
				parseBuf = parseBuf[end:]
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("ffmpeg stdout: %w", err)
		}
	}
}

func isSlice(nal []byte) bool {
	return len(nal) > 0 && (nal[0]&0x1F == 1 || nal[0]&0x1F == 5)
}

// startsPicture reports whether a NAL that follows a slice begins the next
// access unit: any non-VCL NAL, or a slice with first_mb_in_slice 0, whose
// ue(v) code is the single bit 1. nal holds at least two bytes.
func startsPicture(nal []byte) bool {
	return !isSlice(nal) || nal[1]&0x80 != 0
}

// readNUT reads NUT frames from ffmpeg's stdout, each one access unit, and
// hands each on as soon as its last byte has arrived.
func (e *H264Encoder) readNUT() error {
	r := newNUTReader(e.stdout)
	if e.params.OnFrameOut != nil {
		r.onFrameStart = func() { e.params.OnFrameOut(time.Now()) }
	}
	for {
		frame, err := r.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ffmpeg stdout: %w", err)
		}
		if au := splitAnnexB(frame); len(au) > 0 {
			e.params.OnData(au)
		}
	}
}

// findStartCode returns (offset, length) of the first Annex-B start code in
// buf, or (-1, 0) if none is found.
func findStartCode(buf []byte) (int, int) {
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] == 0 && buf[i+1] == 0 {
			if buf[i+2] == 1 {
				return i, 3
			}
			if i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1 {
				return i, 4
			}
		}
	}
	return -1, 0
}
