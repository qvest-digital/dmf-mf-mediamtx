package mxl

import (
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
	"github.com/qvest-digital/go-mxl/mxl"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
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
		for i := range count {
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

// audioMedia describes the Opus track an audio flow is published as.
func audioMedia(channels int) *description.Media {
	return &description.Media{
		Type: description.MediaTypeAudio,
		Formats: []format.Format{&format.Opus{
			PayloadTyp:   96,
			ChannelCount: channels,
		}},
	}
}

// audioTimeline is the published Opus timeline of one path.
//
// It outlives a single run of runAudio, because a joined path retries its
// audio track in place rather than taking the path down with it. What has to
// survive a retry is where the timeline started and how far it has got: a
// track that came back and began again at zero would publish timestamps the
// muxers reject and a receiver reads as a new stream.
//
// Written only from the encoder's reader goroutine and read by the supervisor
// between attempts, which joins that goroutine before looking. No lock,
// because the two never run at once.
type audioTimeline struct {
	// epoch is the sample index the timeline calls zero. Zero itself means no
	// origin was named, so the reader's own starting position becomes one.
	epoch uint64
	// live reports whether anything has been published on this timeline.
	live bool
	// last is the highest timestamp published on it.
	last int64
	// seq is the RTP sequence number of the next packet. A counter rather
	// than a function of the timestamp: audio that resumes after an outage
	// jumps the timeline by the length of the outage, and a sequence number
	// derived from it would jump with it. RFC 3550 has a receiver treat a
	// jump that large as a different stream and drop packets until two
	// arrive in order, so the gap in the sound would cost the packets after
	// it as well.
	seq uint16
}

// stamp records one packet on the timeline and returns what to send it with.
//
// pts is where the running count has reached and reanchor what the reader's
// position says it should be, or -1 for nothing pending. The reader's answer
// wins when it is ahead, which is how samples skipped over cost time on the
// timeline instead of silently shortening it.
//
// It never wins when it is behind. RTSP and HLS both require a strictly
// increasing timestamp and a receiver reads a backwards jump as a different
// stream, so a correction that would move the timeline back is worth less
// than the timeline it would break. The same guard carries the timeline
// across a restart of the track, which begins its own count at zero.
func (t *audioTimeline) stamp(pts, reanchor int64) (int64, uint16) {
	if reanchor > pts {
		pts = reanchor
	}
	if t.live && pts <= t.last {
		pts = t.last + opusFrameSamples
	}
	t.live, t.last = true, pts
	seq := t.seq
	t.seq++
	return pts, seq
}

// audioTrack is what runAudio publishes on, and the timeline it publishes on.
//
// A solo audio path builds both here; a joined path passes its own publisher,
// carrying the video media too, and a timeline whose origin is the instant the
// video track is stamped from.
type audioTrack struct {
	pub *publisher
	// media is the description the publisher was created with. See videoTrack
	// for why writing with any other pointer is fatal to the whole server.
	media *description.Media
	// timeline is the path's, or nil on a solo path, which owns one of its own
	// because nothing else publishes on it.
	timeline *audioTimeline
}

// runAudio publishes a continuous MXL flow as one Opus track.
//
// The shape mirrors the video path: read from the flow's own clock, encode
// through an ffmpeg sidecar, and stamp PTS from the index rather than from
// arrival time. What differs is that samples are continuous, so the reader
// consumes a contiguous range and advances by exactly what it took, and a
// discontinuity is a deliberate resync rather than the normal case.
//
// It returns when the flow stops producing, which on a joined path is not the
// end of anything: superviseAudio runs it again against a fresh reader while
// the video track keeps the path online.
func (s *Source) runAudio(
	params defs.StaticSourceRunParams,
	reader *mxl.Reader,
	info mxl.FlowInfo,
	flowID string,
	track audioTrack,
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

	media, pub := track.media, track.pub
	if pub == nil {
		media = audioMedia(len(channels))
		pub = &publisher{parent: s.Parent, medias: []*description.Media{media}}
		defer pub.close()
	} else if media == nil {
		return errors.New("joined audio track carries a publisher but no media")
	}
	tl := track.timeline
	if tl == nil {
		// A solo path is the only thing that publishes on its timeline, so it
		// owns one rather than being given one.
		tl = &audioTimeline{}
	}

	// pts is the running Opus timestamp. Within a contiguous run it advances
	// one frame per packet; a resync re-anchors it to the sample clock, which
	// is what keeps the timeline honest when the reader has to skip.
	var pts int64
	var reanchor int64 = -1

	onPacket := func(pkt []byte) {
		var seq uint16
		pts, seq = tl.stamp(pts, reanchor)
		reanchor = -1

		pub.write(media, pts, opusClockRate, []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    96,
				SequenceNumber: seq,
				Timestamp:      uint32(pts), //nolint:gosec // wraps by design
			},
			Payload: pkt,
		}})

		pts += opusFrameSamples
	}

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

	// resync places the reader at the flow's leading edge and says what that
	// position is worth on the published timeline.
	//
	// Where to read and what the timeline calls that position are separate
	// questions. The reader always takes the head, which is the only place a
	// flow can be read from live; the timeline's origin is the path's, taken
	// once and kept, so a resync here and a restart of this whole call both
	// land where the flow says they belong rather than back at zero.
	resync := func() error {
		rt, rerr := reader.Runtime()
		if rerr != nil {
			return rerr
		}
		next = backOff(rt.HeadIndex, audioSafetyMargin)
		if tl.epoch == 0 || next < tl.epoch {
			// Nothing named an origin, or the flow has not reached the one it
			// was given: a position before the origin has no timestamp on
			// this timeline, so the reader's own becomes the origin instead.
			tl.epoch = next
		}
		// Anchor the published timeline to where the reader actually landed,
		// so samples skipped here cost time on the timeline instead of
		// silently shortening it.
		reanchor = int64(next-tl.epoch) * opusClockRate / int64(sampleRate)
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
