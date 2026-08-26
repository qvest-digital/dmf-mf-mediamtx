package mxl

import (
	"fmt"
	"io"
	"strconv"
)

// opusClockRate is the only rate RFC 7587 gives Opus over RTP, and the only
// one gortsplib's format.Opus accepts. A flow at any other rate is resampled
// on the way through rather than published at its own rate.
const opusClockRate = 48000

// AudioEncoderParams configures OpusEncoder. Fields mirror the mxlOpus* path
// configuration keys.
type AudioEncoderParams struct {
	FFmpegPath string
	// SampleRate is the flow's own rate, which the encoder resamples to
	// opusClockRate.
	SampleRate uint32
	// ChannelCount is what is handed to ffmpeg, not what the flow carries.
	// RTP Opus is stereo at most, so a wider flow is published a pair at a
	// time and this is 2 (or 1 for a flow with a single channel).
	ChannelCount uint32
	Bitrate      uint32

	// OnPacket is invoked for every encoded Opus packet, in order, from the
	// encoder's reader goroutine.
	OnPacket func(pkt []byte)
}

// OpusEncoder pipes interleaved 32-bit float samples into an ffmpeg
// subprocess and invokes OnPacket for each Opus packet read back.
//
// ffmpeg has no bare-packet output format for Opus, so it is asked for Ogg
// and oggOpusExtractor undoes the framing. Talking to ffmpeg over pipes
// rather than linking libopus keeps the binary's ability to start
// independent of the base image: libopus is present there only as a
// transitive dependency of ffmpeg.
type OpusEncoder struct {
	*ffmpegProcess
	params AudioEncoderParams
}

// NewOpusEncoder starts an ffmpeg process taking interleaved f32le on stdin
// and emitting Ogg-Opus on stdout.
func NewOpusEncoder(p AudioEncoderParams) (*OpusEncoder, error) {
	if p.SampleRate == 0 {
		return nil, fmt.Errorf("sample rate must be non-zero")
	}
	if p.ChannelCount == 0 || p.ChannelCount > 2 {
		return nil, fmt.Errorf("channel count must be 1 or 2, got %d", p.ChannelCount)
	}
	if p.OnPacket == nil {
		return nil, fmt.Errorf("OnPacket callback is required")
	}
	if p.FFmpegPath == "" {
		p.FFmpegPath = "ffmpeg"
	}

	proc, err := startFFmpeg(p.FFmpegPath, buildOpusArgs(p))
	if err != nil {
		return nil, err
	}

	e := &OpusEncoder{ffmpegProcess: proc, params: p}
	proc.readAll = e.readPackets
	go proc.run()
	return e, nil
}

// buildOpusArgs assembles the ffmpeg command line. Kept separate to make the
// knobs easy to inspect in tests.
func buildOpusArgs(p AudioEncoderParams) []string {
	bitrate := p.Bitrate
	if bitrate == 0 {
		bitrate = 128000
	}
	return []string{
		"-hide_banner", "-loglevel", "warning",
		// Input: interleaved float samples from our pipe, at the flow's rate.
		"-f", "f32le",
		"-ar", strconv.FormatUint(uint64(p.SampleRate), 10),
		"-ac", strconv.FormatUint(uint64(p.ChannelCount), 10),
		"-i", "pipe:0",
		"-c:a", "libopus",
		"-b:a", strconv.FormatUint(uint64(bitrate), 10),
		// Pinned rather than left to libopus's default, because the source
		// advances PTS by exactly one frame per packet within a contiguous
		// run. opusFrameSamples is that duration; letting ffmpeg choose would
		// make the two disagree silently.
		"-frame_duration", "20",
		// RTP Opus is 48 kHz whatever the flow declares.
		"-ar", strconv.FormatUint(opusClockRate, 10),
		// No bare-packet muxer exists; oggOpusExtractor undoes this framing.
		"-f", "opus",
		"-flush_packets", "1",
		"pipe:1",
	}
}

// Encode writes one block of interleaved samples.
func (e *OpusEncoder) Encode(interleaved []byte) error {
	if len(interleaved) == 0 {
		return nil
	}
	return e.write(interleaved)
}

func (e *OpusEncoder) readPackets() error {
	extractor := &oggOpusExtractor{}
	buf := make([]byte, 8192)
	for {
		n, err := e.stdout.Read(buf)
		if n > 0 {
			pkts, perr := extractor.push(buf[:n])
			if perr != nil {
				return fmt.Errorf("ogg: %w", perr)
			}
			for _, p := range pkts {
				e.params.OnPacket(p)
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
