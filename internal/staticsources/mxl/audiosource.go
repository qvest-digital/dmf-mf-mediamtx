package mxl

import (
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"

	mxl "github.com/qvest-digital/go-mxl/mxl"
)

const (
	// opusFrameSamples is one Opus frame at 48 kHz. It is 20 ms because
	// buildOpusArgs pins -frame_duration to that, so this is true by
	// construction rather than by assumption: PTS advances by exactly this
	// per packet within a contiguous run.
	opusFrameSamples = opusClockRate / 50

	// audioReadSamples is how much is pulled per iteration, about 20 ms at
	// 48 kHz. Small enough that the reader adds no meaningful latency of its
	// own, large enough that the loop is not syscall-bound.
	audioReadSamples = 960

	// audioSafetyMargin keeps the reader off the writer's leading edge, where
	// samples are still being written. The video path takes one grain for the
	// same reason; audio needs a window rather than a frame because a sample
	// index moves 48000 times a second.
	audioSafetyMargin = 2 * audioReadSamples

	// audioStaleTimeout mirrors staleTimeout on the video path: once samples
	// have flowed, a head that stops advancing this long means the writer
	// recreated the flow and this reader holds a handle to the dead one.
	audioStaleTimeout = 2 * time.Second

	// audioIdleSleep is how long the loop waits when the writer has not yet
	// produced a full read's worth. A fraction of the read window, so the
	// loop tracks the writer without spinning on it.
	audioIdleSleep = 5 * time.Millisecond
)

// audioPair picks the two channels to publish.
//
// RTP Opus carries two channels at most, so a wider flow is heard a pair at a
// time and this is how the rest are reached. An out-of-range selection is
// refused rather than clamped: silently publishing channels 1 and 2 when the
// operator asked for 11 and 12 would look like the flow is empty there.
func audioPair(selected []int, channelCount uint32) ([]uint64, error) {
	if channelCount == 0 {
		return nil, errors.New("flow declares no channels")
	}

	if len(selected) == 0 {
		if channelCount == 1 {
			return []uint64{0}, nil
		}
		return []uint64{0, 1}, nil
	}

	out := make([]uint64, 0, len(selected))
	for _, ch := range selected {
		if uint32(ch) > channelCount {
			return nil, fmt.Errorf("channel %d selected, flow carries %d", ch, channelCount)
		}
		out = append(out, uint64(ch-1))
	}
	return out, nil
}

// sampleSize is one 32-bit float. Audio flows here declare audio/float32 and
// bit_depth 32; anything else would need converting rather than copying.
const sampleSize = 4

// interleaveFragments packs per-channel sample fragments into the f32le block
// the encoder expects.
//
// Each channel arrives as up to two fragments because the requested range can
// straddle the ring buffer's wraparound, and the second is non-empty only
// then. Split out from the view it comes from so the packing can be tested
// without libmxl: the arithmetic here is the part that goes wrong, and a
// wrong stride swaps the channels rather than failing.
func interleaveFragments(frags [][2][]byte, count int, dst []byte) error {
	stride := len(frags) * sampleSize
	if len(dst) < count*stride {
		return fmt.Errorf("destination holds %d bytes, need %d", len(dst), count*stride)
	}

	for slot, f := range frags {
		f1, f2 := f[0], f[1]
		if len(f1)+len(f2) < count*sampleSize {
			return fmt.Errorf("channel slot %d carries %d bytes, wanted %d",
				slot, len(f1)+len(f2), count*sampleSize)
		}
		off := slot * sampleSize
		for i := 0; i < count; i++ {
			src, at := f1, i*sampleSize
			if at >= len(f1) {
				src, at = f2, at-len(f1)
			}
			copy(dst[off+i*stride:off+i*stride+sampleSize], src[at:at+sampleSize])
		}
	}
	return nil
}

// interleave reads the selected channels out of a view and packs them. The
// fragments alias libmxl's shared memory, so they are copied here rather than
// retained.
func interleave(view *mxl.SamplesView, channels []uint64, count int, dst []byte) error {
	frags := make([][2][]byte, len(channels))
	for slot, ch := range channels {
		f1, f2, err := view.ChannelFragments(ch)
		if err != nil {
			return fmt.Errorf("channel %d: %w", ch, err)
		}
		frags[slot] = [2][]byte{f1, f2}
	}
	return interleaveFragments(frags, count, dst)
}

// runAudio publishes a continuous MXL flow as one Opus track.
//
// The shape mirrors the video path: read from the flow's own clock, encode
// through an ffmpeg sidecar, and stamp PTS from the index rather than from
// arrival time. What differs is that samples are continuous, so the reader
// consumes a contiguous range and advances by exactly what it took, and a
// discontinuity is a deliberate resync rather than the normal case.
func (s *Source) runAudio(
	params defs.StaticSourceRunParams,
	reader *mxl.Reader,
	info mxl.FlowInfo,
	flowID string,
) error {
	rate := info.Config.Common.GrainRate
	if rate.Num <= 0 || rate.Den <= 0 {
		return fmt.Errorf("flow %s declares sample rate %d/%d", flowID, rate.Num, rate.Den)
	}
	sampleRate := uint32(rate.Num / rate.Den)
	channelCount := info.Config.Continuous.ChannelCount

	selected, err := conf.ParseAudioChannels(params.Conf.MXLAudioChannels)
	if err != nil {
		return fmt.Errorf("mxlAudioChannels: %w", err)
	}
	channels, err := audioPair(selected, channelCount)
	if err != nil {
		return err
	}

	s.Log(logger.Info, "flow %s carries %d channels at %d/%d Hz, publishing %v",
		flowID, channelCount, rate.Num, rate.Den, channels)

	media := &description.Media{
		Type: description.MediaTypeAudio,
		Formats: []format.Format{&format.Opus{
			PayloadTyp:   96,
			ChannelCount: len(channels),
		}},
	}

	var subStream *stream.SubStream
	var startNTPSet bool
	var startNTP time.Time
	// pts is the running Opus timestamp. Within a contiguous run it advances
	// one frame per packet; a resync re-anchors it to the sample clock, which
	// is what keeps the timeline honest when the reader has to skip.
	var pts int64
	var reanchor int64 = -1

	onPacket := func(pkt []byte) {
		if reanchor >= 0 {
			pts = reanchor
			reanchor = -1
		}

		if !startNTPSet {
			startNTP = time.Now()
			startNTPSet = true
		}
		ntp := startNTP.Add(time.Duration(pts) * time.Second / opusClockRate)

		if subStream == nil {
			res := s.Parent.SetReady(defs.PathSourceStaticSetReadyReq{
				Desc:          &description.Session{Medias: []*description.Media{media}},
				UseRTPPackets: true,
			})
			if res.Err != nil {
				return
			}
			subStream = res.SubStream
		}

		subStream.WriteUnit(media, media.Formats[0], &unit.Unit{
			PTS: pts,
			NTP: ntp,
			RTPPackets: []*rtp.Packet{{
				Header: rtp.Header{
					Version:        2,
					Marker:         true,
					PayloadType:    96,
					SequenceNumber: uint16(pts / opusFrameSamples), //nolint:gosec // wraps by design
					Timestamp:      uint32(pts),                    //nolint:gosec // wraps by design
				},
				Payload: pkt,
			}},
		})

		pts += opusFrameSamples
	}

	defer func() {
		if subStream != nil {
			s.Parent.SetNotReady(defs.PathSourceStaticSetNotReadyReq{})
		}
	}()

	enc, err := NewOpusEncoder(AudioEncoderParams{
		FFmpegPath:   params.Conf.MXLFFmpegPath,
		SampleRate:   sampleRate,
		ChannelCount: uint32(len(channels)),
		Bitrate:      uint32(params.Conf.MXLOpusBitrate),
		OnPacket:     onPacket,
	})
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}
	defer enc.Close()

	encErr := make(chan error, 1)
	go func() { encErr <- enc.Wait() }()

	maxRead, err := reader.GetMaxReadLengthSamples()
	if err != nil {
		return fmt.Errorf("read max read length: %w", err)
	}
	want := uint64(audioReadSamples)
	if maxRead > 0 && want > maxRead {
		want = maxRead
	}

	block := make([]byte, int(want)*len(channels)*4)
	// next is the exclusive end of what has been consumed. GetSamples returns
	// the range ENDING at the index it is given, so a read of `want` samples
	// ending at next+want is the range starting at next.
	var next uint64
	var started bool
	lastProgress := time.Now()

	// firstSample anchors the timeline. Everything downstream is relative to
	// it, so a flow that has been running for hours starts at zero here.
	var firstSample uint64

	resync := func() error {
		rt, rerr := reader.Runtime()
		if rerr != nil {
			return rerr
		}
		if rt.HeadIndex <= audioSafetyMargin {
			next = 0
		} else {
			next = rt.HeadIndex - audioSafetyMargin
		}
		if !started {
			firstSample = next
		}
		// Re-anchor the published timeline to where the reader actually
		// landed, so samples skipped here cost time on the timeline instead
		// of silently shortening it.
		reanchor = int64(next-firstSample) * opusClockRate / int64(sampleRate)
		return nil
	}

	if err = resync(); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	for {
		select {
		case exitErr := <-encErr:
			if exitErr != nil {
				return fmt.Errorf("encoder: %w", exitErr)
			}
			return errors.New("encoder exited")
		case <-params.Context.Done():
			return nil
		default:
		}

		if started && time.Since(lastProgress) > audioStaleTimeout {
			return fmt.Errorf("flow %s stalled for %v (writer likely recreated the flow); "+
				"restarting source to re-open the reader", flowID, audioStaleTimeout)
		}
		if !started && time.Since(lastProgress) > firstGrainTimeout {
			return fmt.Errorf("no samples within %v of opening flow %s; "+
				"restarting source to re-open the reader", firstGrainTimeout, flowID)
		}

		rt, rerr := reader.Runtime()
		if rerr != nil {
			return fmt.Errorf("read runtime: %w", rerr)
		}

		// The writer has not produced a full read yet.
		if rt.HeadIndex < next+want+audioSafetyMargin {
			time.Sleep(audioIdleSleep)
			continue
		}

		// Fallen out of the ring: the encoder could not keep up, or the
		// process was descheduled long enough for the writer to lap us.
		if rt.HeadIndex-next > uint64(info.Config.Continuous.BufferLength) {
			s.Log(logger.Warn, "reader fell %d samples behind a %d-sample ring, resyncing",
				rt.HeadIndex-next, info.Config.Continuous.BufferLength)
			if err = resync(); err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		}

		view, verr := reader.GetSamplesNonBlocking(next+want, int(want))
		switch {
		case verr == nil:
		case errors.Is(verr, mxl.ErrOutOfRangeEarly):
			// Per the C API this is what waiting for unavailable data looks
			// like; ErrTimeout is never returned here.
			time.Sleep(audioIdleSleep)
			continue
		case errors.Is(verr, mxl.ErrOutOfRangeLate):
			if err = resync(); err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		default:
			return fmt.Errorf("get samples: %w", verr)
		}

		if err = interleave(view, channels, int(want), block); err != nil {
			return fmt.Errorf("interleave: %w", err)
		}
		if err = enc.Encode(block); err != nil {
			return fmt.Errorf("encoder write: %w", err)
		}

		next += want
		started = true
		lastProgress = time.Now()
	}
}
