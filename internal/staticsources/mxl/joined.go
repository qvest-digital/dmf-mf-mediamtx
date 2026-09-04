package mxl

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/qvest-digital/go-mxl/mxl"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// errNoCommonStart reports that no single instant can be named in both flows'
// index spaces, so the two cannot be given a shared starting point.
//
// Every way of failing to align wraps this, because none of them is a reason
// to refuse the path. Alignment is an enhancement over playing the two tracks:
// losing it costs lip-sync, and the caller plays them unaligned instead. The
// wrapped text says which condition was met, since they call for different
// action and one of them is not a fault at all.
var errNoCommonStart = errors.New("no common start instant")

// alignStarts picks the index each flow should begin at so that the two land
// at the same instant.
//
// Both indices count on their own flow's rate and neither is comparable to
// the other, but libmxl maps either onto one absolute clock: IndexToTimestamp
// gives nanoseconds since the ST 2059 epoch, and TimestampToIndex comes back.
// So one of the two starting points is converted once and the other is
// derived from it, and from then on each track advances by exact rational
// arithmetic on its own rate. Doing it this way round keeps the sub-tick
// precision that converting every timestamp through nanoseconds would lose.
//
// This is what makes lip-sync a property of the flows rather than of the two
// encoders' output lag, which is what the wall clock would have measured.
//
// The instant is the earlier of the two, because it has to be one both flows
// have already written. Deriving it from the later start places the other
// flow's reader past its own head, waiting on entries the writer has not
// produced: the read never completes, no sample reaches the encoder, and the
// track fails without ever having been behind.
//
// videoRing and audioRing are how many entries each flow holds. A derived
// index outside its flow's ring is not a start but evidence that the two
// index spaces are not commensurable, and alignStarts reports
// errNoCommonStart rather than returning a position that cannot be read.
//
// Every error it returns wraps errNoCommonStart. There is no failure here the
// caller should treat as fatal: two flows that cannot be aligned are still two
// flows that can be played.
func alignStarts(
	videoRate, audioRate mxl.Rational,
	videoIdx, audioIdx, videoRing, audioRing uint64,
) (uint64, uint64, error) {
	// A flow nothing has written to yet sits at index 0, and index 0 converts
	// to timestamp 0 at every rate. Checked before the conversion so the two
	// are not confused: a flow waiting for its producer is the ordinary state
	// of a path opened early, not a malformed rate.
	if videoIdx == 0 || audioIdx == 0 {
		return 0, 0, fmt.Errorf("%w: a flow has no grains yet", errNoCommonStart)
	}

	videoTS := mxl.IndexToTimestamp(videoRate, videoIdx)
	audioTS := mxl.IndexToTimestamp(audioRate, audioIdx)
	if !usableTimestamp(videoTS) || !usableTimestamp(audioTS) {
		return 0, 0, fmt.Errorf("%w: a flow rate does not map onto the MXL clock",
			errNoCommonStart)
	}

	epoch := min(videoTS, audioTS)

	v := mxl.TimestampToIndex(videoRate, epoch)
	a := mxl.TimestampToIndex(audioRate, epoch)
	if v == mxl.UndefinedIndex || a == mxl.UndefinedIndex {
		return 0, 0, fmt.Errorf("%w: the MXL clock returned no index for the "+
			"chosen instant", errNoCommonStart)
	}
	if !readable(v, videoIdx, videoRing) || !readable(a, audioIdx, audioRing) {
		return 0, 0, fmt.Errorf("%w: the flows do not share an index epoch",
			errNoCommonStart)
	}
	return v, a, nil
}

// usableTimestamp reports whether a timestamp came back from the MXL clock as
// a real instant. UndefinedIndex is what the conversion returns when it cannot
// place the index, and zero is what an unusable rate produces; neither can be
// compared against the other flow's. Index 0 also converts to zero at every
// rate, which is why the caller rules that out first.
func usableTimestamp(ts uint64) bool {
	return ts != 0 && ts != mxl.UndefinedIndex
}

// readable reports whether derived is a position the flow can still be read
// from, given the start it would have used alone and how many entries it
// holds. Past that start is data the writer has not produced; further back
// than the ring is data it has already overwritten.
func readable(derived, own, ring uint64) bool {
	if derived > own {
		return false
	}
	return own-derived <= ring
}

// runJoined publishes a video flow and an audio flow as one path with two
// tracks.
//
// Picture and sound are separate MXL flows and nothing downstream rejoins
// them, so a browser that wants both would otherwise play two paths and drift.
// One path with two tracks is what lets it play them in step.
func (s *Source) runJoined(
	params defs.StaticSourceRunParams,
	inst *mxl.Instance,
	videoReader *mxl.Reader,
	videoInfo mxl.FlowInfo,
	u mxlURL,
) error {
	audioReader, err := inst.NewReader(u.audioFlowID)
	if err != nil {
		return fmt.Errorf("open audio reader: %w", err)
	}
	defer func() { _ = audioReader.Close() }()

	audioInfo, err := audioReader.Info()
	if err != nil {
		return fmt.Errorf("read audio flow info: %w", err)
	}
	if audioInfo.Config.Common.Format != mxl.FormatAudio {
		return fmt.Errorf("flow %s is not audio (format=%s)",
			u.audioFlowID, audioInfo.Config.Common.Format)
	}

	videoRate := videoInfo.Config.Common.GrainRate
	audioRate := audioInfo.Config.Common.GrainRate
	if videoRate.Num <= 0 || videoRate.Den <= 0 || audioRate.Num <= 0 || audioRate.Den <= 0 {
		return fmt.Errorf("flow rates %d/%d and %d/%d cannot both be used",
			videoRate.Num, videoRate.Den, audioRate.Num, audioRate.Den)
	}

	// Where each reader would have started on its own, backed off the leading
	// edge the way each solo path does, before they are aligned to each other.
	videoRT, err := videoReader.Runtime()
	if err != nil {
		return fmt.Errorf("read video runtime: %w", err)
	}
	audioRT, err := audioReader.Runtime()
	if err != nil {
		return fmt.Errorf("read audio runtime: %w", err)
	}
	videoStart := backOff(videoRT.HeadIndex, 1)
	audioStart := backOff(audioRT.HeadIndex, audioSafetyMargin)

	v, a, err := alignStarts(videoRate, audioRate, videoStart, audioStart,
		uint64(videoInfo.Config.Discrete.GrainCount), uint64(audioInfo.Config.Continuous.BufferLength))
	if err != nil {
		// Each track still plays from its own head. Lip-sync is lost, which is
		// the lesser fault: a path that plays out of step beats one that never
		// starts, and a flow still waiting for its producer would otherwise
		// keep the path down for as long as it takes to arrive.
		s.Log(logger.Warn, "flows %s and %s cannot be given a shared start "+
			"(%v), playing each from its own head without lip-sync alignment",
			u.flowID, u.audioFlowID, err)
	} else {
		videoStart, audioStart = v, a
	}

	// Both medias are named up front: a substream carries the set it was
	// created with, so a track that arrives later has nowhere to go.
	selected, err := conf.ParseAudioChannels(params.Conf.MXLAudioChannels)
	if err != nil {
		return fmt.Errorf("mxlAudioChannels: %w", err)
	}
	channels, err := audioPair(selected, audioInfo.Config.Continuous.ChannelCount)
	if err != nil {
		return err
	}

	vMedia := videoMedia()
	aMedia := audioMedia(len(channels))
	pub := &publisher{
		parent: s.Parent,
		medias: []*description.Media{vMedia, aMedia},
	}
	defer pub.close()

	s.Log(logger.Info, "joining audio flow %s to video flow %s, starting at grain %d and sample %d",
		u.audioFlowID, u.flowID, videoStart, audioStart)

	// Either track failing takes the path down. A path that keeps serving
	// picture after its sound has stopped is worse than one that restarts:
	// the reader would have no way to tell, and the static-source handler
	// re-runs this against both flows.
	// A derived context rather than the handler's own: whichever track stops
	// first has to stop the other, and without this the surviving goroutine
	// would run until the handler cancelled, which it has no reason to do
	// while this call has not returned.
	ctx, cancel := context.WithCancel(params.Context)
	defer cancel()

	audioParams, videoParams := params, params
	audioParams.Context, videoParams.Context = ctx, ctx

	errs := make(chan error, 2)
	go func() {
		errs <- s.runAudio(audioParams, audioReader, audioInfo, u.audioFlowID,
			audioTrack{pub: pub, media: aMedia, startIndex: audioStart})
	}()
	go func() {
		errs <- s.runVideo(videoParams, inst, videoReader, videoInfo, u.flowID,
			videoTrack{pub: pub, media: vMedia, startIndex: videoStart})
	}()

	first := <-errs
	cancel()
	// Wait for the other, so neither reader outlives this call and the
	// handler cannot re-run against flows still being read.
	<-errs
	return first
}

// backOff keeps a reader off the writer's leading edge, where the newest
// entries are still being written.
func backOff(head, margin uint64) uint64 {
	if head <= margin {
		return head
	}
	return head - margin
}
