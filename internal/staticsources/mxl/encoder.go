package mxl

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// EncoderParams configures the H264Encoder. Fields mirror the mxlH264* path
// configuration keys.
type EncoderParams struct {
	FFmpegPath string
	Width      uint32
	Height     uint32
	FPS        uint32
	Preset     string // x264 preset, e.g. "veryfast"
	Profile    string // "baseline" | "main" | "high"
	Bitrate    uint32 // target bitrate in bits/sec; 0 means use ffmpeg/x264 default
	IDRPeriod  uint32 // distance between IDR frames in frames; 0 means use FPS

	// OnData is invoked for every encoded access unit, in input order, from
	// the encoder's reader goroutine. The callback may block (write to a
	// substream, log, etc.); blocking propagates back as natural
	// backpressure on the encoder pipe.
	OnData func(au [][]byte)
}

// H264Encoder pipes planar YUV 4:2:0 frames into an ffmpeg subprocess and
// invokes EncoderParams.OnData for each H.264 access unit read back from
// stdout. Talking to ffmpeg over pipes (rather than linking libx264 directly)
// keeps any GPL bits out of the mediamtx binary and lets the operator pick a
// different encoder build at deploy time.
//
// Because the boundary of NAL unit N is only known once the start code of
// NAL unit N+1 has been read, there is an inherent one-frame latency
// between Encode and the corresponding OnData call.
type H264Encoder struct {
	params EncoderParams

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	yuvBuf []byte
	ySize  int
	cSize  int

	finalErr error

	terminate chan struct{}
	done      chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewH264Encoder starts an ffmpeg process configured to take raw yuv420p
// frames on stdin and emit H.264 Annex-B on stdout. Encoded access units are
// delivered via p.OnData. ffmpeg's stderr is inherited from the parent
// process so warnings surface where the operator expects them.
func NewH264Encoder(p EncoderParams) (*H264Encoder, error) {
	if p.Width%2 != 0 || p.Height%2 != 0 {
		return nil, fmt.Errorf("dimensions must be even, got %dx%d", p.Width, p.Height)
	}
	if p.FPS == 0 {
		return nil, fmt.Errorf("FPS must be non-zero")
	}
	if p.OnData == nil {
		return nil, fmt.Errorf("OnData callback is required")
	}
	if p.FFmpegPath == "" {
		p.FFmpegPath = "ffmpeg"
	}

	args := buildFFmpegArgs(p)

	cmd := exec.Command(p.FFmpegPath, args...) //nolint:gosec // operator-controlled
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	// Run ffmpeg in its own process group so a SIGINT to mediamtx itself
	// doesn't reach the child and we have a well-defined target for kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.FFmpegPath, err)
	}

	cSize := (int(p.Width) / 2) * (int(p.Height) / 2)
	e := &H264Encoder{
		params:    p,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		yuvBuf:    make([]byte, int(p.Width)*int(p.Height)+2*cSize),
		ySize:     int(p.Width) * int(p.Height),
		cSize:     cSize,
		terminate: make(chan struct{}),
		done:      make(chan struct{}),
	}

	go e.run()
	return e, nil
}

// buildFFmpegArgs assembles the ffmpeg command line from params. Kept
// separate to keep NewH264Encoder readable and to make these knobs easy to
// inspect in tests.
func buildFFmpegArgs(p EncoderParams) []string {
	idr := p.IDRPeriod
	if idr == 0 {
		idr = p.FPS
	}

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// Input: raw YUV from our pipe.
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", p.Width, p.Height),
		"-r", strconv.FormatUint(uint64(p.FPS), 10),
		"-i", "pipe:0",
		// Encoder.
		"-c:v", "libx264",
		"-preset", p.Preset,
		"-tune", "zerolatency",
		"-profile:v", p.Profile,
		"-pix_fmt", "yuv420p",
		"-g", strconv.FormatUint(uint64(idr), 10),
		// `tune=zerolatency` enables sliced-threads (multi-slice frames),
		// which breaks the VCL=AU-boundary grouping in readAUs. scenecut=0
		// keeps IDRs strictly at -g intervals so late-joining readers
		// recover predictably.
		"-x264-params", "sliced-threads=0:scenecut=0",
		// Repeat SPS/PPS at every IDR so late-joining RTSP readers can decode.
		"-bsf:v", "dump_extra=freq=keyframe",
	}

	if p.Bitrate > 0 {
		args = append(args,
			"-b:v", strconv.FormatUint(uint64(p.Bitrate), 10),
			"-maxrate", strconv.FormatUint(uint64(p.Bitrate), 10),
			"-bufsize", strconv.FormatUint(uint64(p.Bitrate)*2, 10),
		)
	}

	args = append(args,
		"-f", "h264",
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

	if _, err := e.stdin.Write(e.yuvBuf); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// Close shuts the encoder down. Safe to call multiple times; blocks until
// the encoder goroutine has exited.
func (e *H264Encoder) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		<-e.done
		return
	}
	e.closed = true
	e.mu.Unlock()

	close(e.terminate)
	<-e.done
}

// Wait blocks until the encoder goroutine exits and returns the terminal
// error (nil on clean shutdown).
func (e *H264Encoder) Wait() error {
	<-e.done
	return e.finalErr
}

func (e *H264Encoder) run() {
	defer close(e.done)
	e.finalErr = e.runInner()
}

// runInner is the shutdown choreography: three things can end the encoder —
// ffmpeg exits on its own, the stdout reader errors, or Close is called.
// Each branch closes the pipes in an order that lets the other goroutines
// unblock and reports an appropriate terminal error.
func (e *H264Encoder) runInner() error {
	cmdDone := make(chan error, 1)
	go func() { cmdDone <- e.cmd.Wait() }()

	readDone := make(chan error, 1)
	go func() { readDone <- e.readAUs() }()

	select {
	case err := <-cmdDone:
		// ffmpeg exited unexpectedly; close pipes so the reader unblocks.
		_ = e.stdin.Close()
		_ = e.stdout.Close()
		<-readDone
		if err != nil {
			return fmt.Errorf("ffmpeg exited: %w", err)
		}
		return fmt.Errorf("ffmpeg exited unexpectedly")

	case err := <-readDone:
		// stdout reader errored; ask ffmpeg to exit and wait for it.
		_ = e.stdin.Close()
		<-cmdDone
		_ = e.stdout.Close()
		return err

	case <-e.terminate:
		// Cooperative shutdown. EOF on stdin is ffmpeg's "flush and exit"
		// signal; SIGINT to the process group is belt-and-suspenders for
		// any helper threads ffmpeg may have spawned.
		_ = e.stdin.Close()
		if e.cmd.Process != nil {
			_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGINT)
		}
		<-cmdDone
		_ = e.stdout.Close()
		<-readDone
		return nil
	}
}

// readAUs parses ffmpeg's stdout into Annex-B NAL units, groups them into
// access units, and invokes the OnData callback for each complete AU.
//
// AU grouping rule: append NAL units to a "current AU" buffer; when we see
// a VCL NAL (types 1=non-IDR slice or 5=IDR slice), the AU is complete and
// the callback fires. SPS/PPS/SEI NALs that precede a VCL NAL end up
// grouped with it, which matches what `-bsf:v dump_extra=freq=keyframe`
// produces.
//
// A NAL unit's boundary is only known once we see the next start code (or
// EOF), so the first AU emerges only after the second frame is pushed.
func (e *H264Encoder) readAUs() error {
	parseBuf := make([]byte, 0, 1<<17)
	readBuf := make([]byte, 8192)
	var currentAU [][]byte

	for {
		n, err := e.stdout.Read(readBuf)
		if n > 0 {
			parseBuf = append(parseBuf, readBuf[:n]...)
			for {
				sc1, sc1Len := findStartCode(parseBuf)
				if sc1 < 0 {
					break
				}
				start := sc1 + sc1Len
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
				currentAU = append(currentAU, nal)
				if len(nal) > 0 && (nal[0]&0x1F == 1 || nal[0]&0x1F == 5) {
					e.params.OnData(currentAU)
					currentAU = nil
				}
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
