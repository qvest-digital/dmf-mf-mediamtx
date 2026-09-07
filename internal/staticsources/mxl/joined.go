package mxl

import (
	"context"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/qvest-digital/go-mxl/mxl"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	// maxLipSyncSkew is how far apart the two flows' heads may be and still be
	// read as two views of one moment.
	//
	// A healthy pair is tens of milliseconds apart: each track backs off its
	// own head by a margin, one grain for video and a read window for audio.
	// Past this the two are not two views of anything -- an ST 2110 source
	// whose audio essence has stopped keeps a head minutes old while its video
	// runs at full rate -- and anchoring the video timeline to it would put
	// the first frame minutes into the stream.
	maxLipSyncSkew = 500 * time.Millisecond

	// audioRetryDelay is how long the audio track waits before trying again,
	// doubling to audioRetryMaxDelay while it keeps failing. A flow whose
	// source has gone quiet costs an attempt every firstGrainTimeout on top,
	// so the ceiling is what keeps a picture-only path from starting an
	// encoder every few seconds for as long as it is open.
	audioRetryDelay    = time.Second
	audioRetryMaxDelay = 30 * time.Second
)

// pathEpoch picks the instant a joined path's timeline calls zero, and the
// index that instant has in each flow.
//
// Both tracks are stamped from their own flow's index, which advances in exact
// media periods and carries none of the jitter read and encode times pick up.
// Neither index means anything to the other, but libmxl maps either onto one
// absolute clock: IndexToTimestamp gives nanoseconds since the ST 2059 epoch
// and TimestampToIndex comes back. Naming one instant and converting it into
// both index spaces is therefore what makes the two timelines one, and lip
// sync a property of the flows rather than of the two encoders' output lag.
//
// The instant is only ever the origin of the timeline. It is not where either
// reader starts -- each takes its own head, the only place a live flow can be
// read from -- so an origin the flow no longer holds costs nothing. Keeping
// the two apart is what lets an audio track that stopped and came back land
// where the flow says it belongs rather than back at zero.
//
// It is the earlier of the two heads, so neither track's first sample falls
// before it and no timestamp starts negative. Only while the two are within
// maxLipSyncSkew, though: past that the audio head is not a view of the same
// moment, and the video takes its own head as the origin instead. The audio
// then reports a zero epoch, which runAudio reads as "the position I start at
// is the origin": the path plays without lip sync rather than not at all.
func pathEpoch(videoRate, audioRate mxl.Rational, videoStart, audioStart uint64) (ns, v, a uint64) {
	if videoStart == 0 {
		return 0, 0, 0
	}
	videoNs := mxl.IndexToTimestamp(videoRate, videoStart)
	if !usableTimestamp(videoNs) {
		return 0, 0, 0
	}

	ns = videoNs
	audioNs := mxl.IndexToTimestamp(audioRate, audioStart)
	aligned := audioStart != 0 && usableTimestamp(audioNs) &&
		skewNs(videoNs, audioNs) <= uint64(maxLipSyncSkew.Nanoseconds())
	if aligned && audioNs < videoNs {
		ns = audioNs
	}

	v = mxl.TimestampToIndex(videoRate, ns)
	if v == mxl.UndefinedIndex {
		return 0, 0, 0
	}
	if aligned {
		if a = mxl.TimestampToIndex(audioRate, ns); a == mxl.UndefinedIndex {
			a = 0
		}
	}
	return ns, v, a
}

// audioEpochAt places a path's origin in the audio flow's index space, for a
// track about to start or restart against a reader that has just been opened.
//
// The origin is an instant, so it converts whether or not the flow still holds
// it. What it cannot survive is a head that has not reached the origin, whose
// samples would land before zero, or one far past the MXL clock, which is an
// index that has run away from real time rather than a flow that has caught up
// with it. Either way the answer is zero: the track takes its own head as the
// origin and the path plays without lip sync until the flow agrees with the
// clock again.
func audioEpochAt(rate mxl.Rational, epochNs uint64, rt mxl.FlowRuntime) uint64 {
	start := backOff(rt.HeadIndex, audioSafetyMargin)
	if start == 0 || epochNs == 0 {
		return 0
	}
	headNs := mxl.IndexToTimestamp(rate, start)
	if !usableTimestamp(headNs) || headNs < epochNs {
		return 0
	}
	if now := mxl.Now(); usableTimestamp(now) &&
		headNs > now+uint64(maxLipSyncSkew.Nanoseconds()) {
		return 0
	}
	idx := mxl.TimestampToIndex(rate, epochNs)
	if idx == mxl.UndefinedIndex {
		return 0
	}
	return idx
}

// usableTimestamp reports whether a timestamp came back from the MXL clock as
// a real instant. UndefinedIndex is what the conversion returns when it cannot
// place the index, and zero is what an unusable rate produces, as well as what
// a flow nothing has written to yet converts to at every rate.
func usableTimestamp(ts uint64) bool {
	return ts != 0 && ts != mxl.UndefinedIndex
}

// skewNs is how far apart two instants are.
func skewNs(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// runJoined publishes a video flow and an audio flow as one path with two
// tracks.
//
// Picture and sound are separate MXL flows and nothing downstream rejoins
// them, so a browser that wants both would otherwise play two paths and drift.
// One path with two tracks is what lets it play them in step.
//
// The video track owns the path: it is the one whose failure ends this call
// and has the static-source handler build the path again. The audio track is
// supervised instead, because the two flows fail independently and a picture
// that is still arriving is worth keeping. See superviseAudio.
func (s *Source) runJoined(
	params defs.StaticSourceRunParams,
	inst *mxl.Instance,
	videoReader *mxl.Reader,
	videoInfo mxl.FlowInfo,
	u mxlURL,
) error {
	audioReader, audioInfo, err := openAudio(inst, u.audioFlowID)
	if err != nil {
		return err
	}

	videoRate := videoInfo.Config.Common.GrainRate
	audioRate := audioInfo.Config.Common.GrainRate
	if videoRate.Num <= 0 || videoRate.Den <= 0 || audioRate.Num <= 0 || audioRate.Den <= 0 {
		_ = audioReader.Close()
		return fmt.Errorf("flow rates %d/%d and %d/%d cannot both be used",
			videoRate.Num, videoRate.Den, audioRate.Num, audioRate.Den)
	}

	// Where each reader would start on its own, backed off the leading edge
	// the way each solo path does. The origin is derived from these; where
	// each track actually begins reading is decided by the track.
	videoRT, err := videoReader.Runtime()
	if err != nil {
		_ = audioReader.Close()
		return fmt.Errorf("read video runtime: %w", err)
	}
	audioRT, err := audioReader.Runtime()
	if err != nil {
		_ = audioReader.Close()
		return fmt.Errorf("read audio runtime: %w", err)
	}

	epochNs, videoEpoch, audioEpoch := pathEpoch(videoRate, audioRate,
		backOff(videoRT.HeadIndex, 1), backOff(audioRT.HeadIndex, audioSafetyMargin))
	if audioEpoch == 0 {
		s.Log(logger.Warn, "flows %s and %s do not name one instant, so the audio "+
			"track plays from its own head without lip-sync alignment",
			u.flowID, u.audioFlowID)
	}

	// Both medias are named up front: a substream carries the set it was
	// created with, so a track that arrives later has nowhere to go. That
	// includes an audio track that arrives several retries late.
	selected, err := conf.ParseAudioChannels(params.Conf.MXLAudioChannels)
	if err != nil {
		_ = audioReader.Close()
		return fmt.Errorf("mxlAudioChannels: %w", err)
	}
	channels, err := audioPair(selected, audioInfo.Config.Continuous.ChannelCount)
	if err != nil {
		_ = audioReader.Close()
		return err
	}

	vMedia := videoMedia()
	aMedia := audioMedia(len(channels))
	pub := &publisher{
		parent: s.Parent,
		medias: []*description.Media{vMedia, aMedia},
	}
	defer pub.close()

	s.Log(logger.Info, "joining audio flow %s to video flow %s, timeline zero at "+
		"grain %d and sample %d", u.audioFlowID, u.flowID, videoEpoch, audioEpoch)

	// A derived context so the audio supervisor stops when the video track
	// returns: without it that goroutine would run until the handler cancelled,
	// which it has no reason to do while this call has not returned.
	ctx, cancel := context.WithCancel(params.Context)
	defer cancel()

	audioParams, videoParams := params, params
	audioParams.Context, videoParams.Context = ctx, ctx

	audioDone := make(chan struct{})
	go func() {
		defer close(audioDone)
		s.superviseAudio(audioParams, inst, audioReader, audioInfo, u.audioFlowID,
			epochNs, audioTrack{
				pub:      pub,
				media:    aMedia,
				timeline: &audioTimeline{epoch: audioEpoch},
			})
	}()

	err = s.runVideo(videoParams, inst, videoReader, videoInfo, u.flowID,
		videoTrack{pub: pub, media: vMedia, epochIndex: videoEpoch})

	cancel()
	// Wait for the supervisor, so no reader outlives this call and the handler
	// cannot re-run against flows still being read.
	<-audioDone
	return err
}

// superviseAudio keeps a joined path's audio track running for as long as the
// path does, and never ends the path itself.
//
// The two essences of one source fail independently. On ST 2110 they are
// separate multicast groups from separate senders, and an audio essence has
// been measured absent for eighteen minutes while the video ran at its full
// sixty grains a second throughout. Returning that as the path's error tore
// down a picture that was still arriving, and the static-source handler built
// the whole path again five seconds later: a source with no sound produced a
// preview that reconnected every seventeen seconds, which is worse for a
// viewer than one that is simply silent.
//
// Each attempt takes a fresh reader. A writer that recreated the flow left a
// new generation behind, and the handle this call was given can only see the
// dead one -- which is what audioStaleTimeout exists to notice.
//
// The channel count is fixed by the first attempt, because the media the
// substream was created with names it. A flow that comes back wider or
// narrower is refused rather than published through a description that no
// longer matches it.
func (s *Source) superviseAudio(
	params defs.StaticSourceRunParams,
	inst *mxl.Instance,
	reader *mxl.Reader,
	info mxl.FlowInfo,
	flowID string,
	epochNs uint64,
	track audioTrack,
) {
	channels := info.Config.Continuous.ChannelCount
	delay := audioRetryDelay

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			var err error
			reader, info, err = reopenAudio(inst, flowID, channels)
			if err != nil {
				s.Log(logger.Warn, "audio flow %s cannot be reopened (%v); the path "+
					"keeps playing picture only", flowID, err)
				if !sleepCtx(params.Context, delay) {
					return
				}
				delay = min(2*delay, audioRetryMaxDelay)
				continue
			}
		}

		// Only while nothing has been published: once the timeline is live its
		// origin is fixed, because moving it would restate every timestamp
		// already sent.
		if !track.timeline.live {
			if rt, err := reader.Runtime(); err == nil {
				track.timeline.epoch = audioEpochAt(info.Config.Common.GrainRate, epochNs, rt)
			}
		}

		liveBefore, lastBefore := track.timeline.live, track.timeline.last
		err := s.runAudio(params, reader, info, flowID, track)
		_ = reader.Close()

		if params.Context.Err() != nil {
			return
		}
		if err != nil {
			s.Log(logger.Warn, "audio track of flow %s stopped (%v); retrying it "+
				"while the path keeps playing", flowID, err)
		}

		// An attempt that published something was not a bad attempt, whatever
		// ended it, so the wait starts over. One that published nothing means
		// the flow is not producing, and hammering it costs an encoder start
		// per attempt for as long as the card is open.
		if track.timeline.live && (!liveBefore || track.timeline.last > lastBefore) {
			delay = audioRetryDelay
		} else {
			delay = min(2*delay, audioRetryMaxDelay)
		}
		if !sleepCtx(params.Context, delay) {
			return
		}
	}
}

// openAudio opens a reader on the audio flow and checks it is one.
func openAudio(inst *mxl.Instance, flowID string) (*mxl.Reader, mxl.FlowInfo, error) {
	reader, err := inst.NewReader(flowID)
	if err != nil {
		return nil, mxl.FlowInfo{}, fmt.Errorf("open audio reader: %w", err)
	}
	info, err := reader.Info()
	if err != nil {
		_ = reader.Close()
		return nil, mxl.FlowInfo{}, fmt.Errorf("read audio flow info: %w", err)
	}
	if info.Config.Common.Format != mxl.FormatAudio {
		_ = reader.Close()
		return nil, mxl.FlowInfo{}, fmt.Errorf("flow %s is not audio (format=%s)",
			flowID, info.Config.Common.Format)
	}
	return reader, info, nil
}

// reopenAudio opens a fresh reader and checks the flow still carries what the
// path's audio media was built for.
func reopenAudio(inst *mxl.Instance, flowID string, channels uint32) (*mxl.Reader, mxl.FlowInfo, error) {
	reader, info, err := openAudio(inst, flowID)
	if err != nil {
		return nil, mxl.FlowInfo{}, err
	}
	if got := info.Config.Continuous.ChannelCount; got != channels {
		_ = reader.Close()
		return nil, mxl.FlowInfo{}, fmt.Errorf("flow now carries %d channels, "+
			"the path publishes %d", got, channels)
	}
	return reader, info, nil
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// backOff keeps a reader off the writer's leading edge, where the newest
// entries are still being written.
func backOff(head, margin uint64) uint64 {
	if head <= margin {
		return head
	}
	return head - margin
}
