// Package mxl contains the MXL (Media eXchange Layer) static source.
//
// It subscribes to a single discrete video flow on a tmpfs MXL domain, unpacks
// the v210 grains to planar YUV 4:2:0, hands the frames to an ffmpeg sidecar
// for H.264 encoding, and feeds the resulting access units to mediamtx as RTP.
package mxl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/qvest-digital/go-mxl/mxl"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	readTimeout = 500 * time.Millisecond

	// staleTimeout: once grains have started flowing, if the flow's head index
	// stops advancing for this long the writer has almost certainly torn down
	// and recreated the flow -- e.g. an SRT source reconnect makes the bridge
	// build a new flow generation. Our reader then holds a handle to the old,
	// now-static shared memory and would spin forever re-reading the last grain
	// (frozen video, no error). Returning lets the static-source handler re-run
	// Run() with a fresh instance/reader that re-discovers the current flow --
	// the same self-heal the demo-app compositor already does on FLOW_INVALID.
	staleTimeout = 2 * time.Second

	// firstGrainTimeout: how long we wait for the FIRST grain after (re)opening
	// the reader. Without it the loop can starve forever with the stale
	// watchdog above never arming (started is still false): the path wedges
	// silently with the encoder up but nothing published. Known causes: a
	// writer that only emits invalid or skipped grains (the whole ring stays
	// invalid while head keeps advancing), or a reader bound to a dead flow
	// generation after recreation.
	//
	// Returning re-runs Run() so the path keeps retrying, logs the
	// diag counters below, and comes online by itself once the source is
	// healthy again. Generous so a writer that is merely slow to start doesn't
	// flap the encoder.
	firstGrainTimeout = 10 * time.Second
)

// flowDef is the subset of the NMOS IS-04 flow definition we need.
type flowDef struct {
	MediaType   string `json:"media_type"`
	FrameWidth  int    `json:"frame_width"`
	FrameHeight int    `json:"frame_height"`
}

type parent interface {
	logger.Writer
	SetReady(req defs.PathSourceStaticSetReadyReq) defs.PathSourceStaticSetReadyRes
	SetNotReady(req defs.PathSourceStaticSetNotReadyReq)
}

// Source is the static source implementation for mxl:// URLs.
type Source struct {
	RTPMaxPayloadSize int
	LogLevel          conf.LogLevel
	Parent            parent
}

// Log implements logger.Writer.
func (s *Source) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[MXL source] "+format, args...)
}

// APISourceDescribe implements StaticSource.
func (*Source) APISourceDescribe() *defs.APIPathSource {
	return &defs.APIPathSource{
		Type: defs.APIPathSourceTypeMXLSource,
		ID:   "",
	}
}

// Run implements StaticSource.
func (s *Source) Run(params defs.StaticSourceRunParams) error {
	u, err := parseMXLURL(params.ResolvedSource)
	if err != nil {
		return err
	}
	domain, flowID := u.domain, u.flowID
	s.Log(logger.Info, "opening MXL domain %q flow %s", domain, flowID)

	inst, err := mxl.NewInstance(domain, "")
	if err != nil {
		return fmt.Errorf("open MXL instance: %w", err)
	}
	defer func() { _ = inst.Close() }()

	reader, err := inst.NewReader(flowID)
	if err != nil {
		return fmt.Errorf("open MXL reader: %w", err)
	}
	defer func() { _ = reader.Close() }()

	info, err := reader.Info()
	if err != nil {
		return fmt.Errorf("read flow info: %w", err)
	}
	// Audio is a continuous flow read as samples rather than grains, and it
	// publishes Opus rather than H.264, so it takes its own path from here.
	if info.Config.Common.Format == mxl.FormatAudio {
		if u.audioFlowID != "" {
			return fmt.Errorf("flow %s is audio, so it cannot also carry the audio of %s",
				flowID, u.audioFlowID)
		}
		return s.runAudio(params, reader, info, flowID, audioTrack{})
	}
	if info.Config.Common.Format != mxl.FormatVideo {
		return fmt.Errorf("flow %s is neither video nor audio (format=%s)",
			flowID, info.Config.Common.Format)
	}

	if u.audioFlowID != "" {
		return s.runJoined(params, inst, reader, info, u)
	}
	return s.runVideo(params, inst, reader, info, flowID, videoTrack{})
}

// videoMedia describes the H.264 track a video flow is published as.
func videoMedia() *description.Media {
	return &description.Media{
		Type: description.MediaTypeVideo,
		Formats: []format.Format{&format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
		}},
	}
}

// videoTrack is what runVideo publishes on and where it starts.
//
// A solo video path builds both here; a joined path passes its own
// publisher, carrying the audio media too, and a start index chosen to line
// up with the audio's on the MXL clock.
type videoTrack struct {
	pub *publisher
	// startIndex is the grain to begin at, or 0 to pick one from the head.
	startIndex uint64
}

func (s *Source) runVideo(
	params defs.StaticSourceRunParams,
	inst *mxl.Instance,
	reader *mxl.Reader,
	info mxl.FlowInfo,
	flowID string,
	track videoTrack,
) error {
	defJSON, err := inst.FlowDef(flowID)
	if err != nil {
		return fmt.Errorf("read flow def: %w", err)
	}
	var def flowDef
	err = json.Unmarshal([]byte(defJSON), &def)
	if err != nil {
		return fmt.Errorf("parse flow def: %w", err)
	}
	if def.MediaType != "video/v210" {
		return fmt.Errorf("unsupported media_type %q (only video/v210 in v1)", def.MediaType)
	}
	width, height := def.FrameWidth, def.FrameHeight
	srcStride := int(info.Config.Discrete.SliceSizes[0])
	rate := info.Config.Common.GrainRate
	if rate.Num <= 0 || rate.Den <= 0 {
		return fmt.Errorf("flow %s declares grain rate %d/%d", flowID, rate.Num, rate.Den)
	}

	s.Log(logger.Info, "flow geometry %dx%d @ %d/%d (%.3ffps), stride=%d, ringSize=%d",
		width, height, rate.Num, rate.Den, rate.Float64(), srcStride, info.Config.Discrete.GrainCount)

	unpacker, err := NewV210Unpacker(width, height)
	if err != nil {
		return fmt.Errorf("v210 unpacker: %w", err)
	}

	media := videoMedia()

	rtpEnc := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: s.RTPMaxPayloadSize,
	}
	err = rtpEnc.Init()
	if err != nil {
		return fmt.Errorf("rtp encoder: %w", err)
	}

	// State owned by the OnData callback. OnData runs on the encoder's
	// reader goroutine; nothing else touches these.
	pub := track.pub
	if pub == nil {
		pub = &publisher{parent: s.Parent, medias: []*description.Media{media}}
		defer pub.close()
	}

	var published bool
	clock := ptsClock{rateNum: rate.Num, rateDen: rate.Den}
	pending := &pendingIndices{}
	var warnedStarved, warnedStalled bool

	onData := func(au [][]byte) {
		if len(au) == 0 {
			return
		}
		// PTS comes from the grain's own index rather than from the
		// moment the access unit left ffmpeg. Read and encode times
		// jitter by milliseconds on a loaded node, and stamping that
		// jitter onto the timeline is what a player renders as uneven
		// motion at an otherwise clean frame rate. The index also
		// survives the freshest-grain strategy in the loop below: a
		// dropped grain advances it by more than one, so the timeline
		// keeps pace with real time instead of falling behind by every
		// frame that was skipped.
		var ptsTicks int64
		if index, ok := pending.pop(); ok {
			before := clock.last
			ptsTicks = clock.ticks(index)
			// The writer's head index has been seen stepping backwards.
			// ticks() clamps so the muxers still get a strictly
			// increasing timestamp, but the fault is worth naming once.
			if !warnedStalled && published && ptsTicks == before+1 {
				warnedStalled = true
				s.Log(logger.Warn, "grain %d did not advance the timeline; "+
					"the writer's head index has gone backwards", index)
			}
		} else {
			// One access unit per input frame is what the encoder's
			// flags promise. Carry on at the nominal cadence rather
			// than dropping the frame if that ever stops holding.
			if !warnedStarved {
				warnedStarved = true
				s.Log(logger.Warn, "access unit with no grain queued; "+
					"PTS continues at the nominal rate")
			}
			ptsTicks = clock.advance()
		}

		pkts, rtpErr := rtpEnc.Encode(au)
		if rtpErr != nil {
			s.Log(logger.Error, "rtp encode: %v", rtpErr)
			return
		}
		for _, pkt := range pkts {
			pkt.Timestamp = uint32(ptsTicks) //nolint:gosec // wraps by design
		}
		pub.write(media, ptsTicks, rtpClockRate, pkts)
		published = true
	}

	enc, err := NewH264Encoder(encoderParamsFromConf(params.Conf, width, height, rate, onData))
	if err != nil {
		return fmt.Errorf("h264 encoder: %w", err)
	}
	defer enc.Close()
	s.Log(logger.Info, "started ffmpeg encoder (%s)", enc.params.FFmpegPath)

	encErr := make(chan error, 1)
	go func() {
		encErr <- enc.Wait()
	}()

	yPlane := make([]byte, width*height)
	cbPlane := make([]byte, (width/2)*(height/2))
	crPlane := make([]byte, (width/2)*(height/2))

	// freshestIndex picks the grain we'll request next: the writer's
	// current head minus a small safety margin. This makes the consumer
	// "live" -- if encoding is slower than the writer's pace, we drop
	// frames instead of falling behind the ring buffer window. Wall-clock
	// PTS (above) keeps downstream timing correct regardless of drops.
	//
	// The margin keeps us from racing the writer at the leading edge of
	// the buffer; with a 5-grain ring this leaves ~3 grains of headroom.
	const safetyMargin uint64 = 1
	freshestIndex := func() (uint64, error) {
		rt, rtErr := reader.Runtime()
		if rtErr != nil {
			return 0, rtErr
		}
		if rt.HeadIndex > safetyMargin {
			return rt.HeadIndex - safetyMargin, nil
		}
		return rt.HeadIndex, nil
	}

	idx, err := freshestIndex()
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}
	if track.startIndex != 0 {
		// A joined path chose this to line up with the audio track, so take
		// it rather than picking off the head independently.
		idx = track.startIndex
	}
	var lastIdx uint64
	// Self-heal watchdog state: started flips true on the first decoded grain;
	// lastProgress is bumped whenever the head advances. See staleTimeout.
	var started bool
	lastProgress := time.Now()
	// Diagnostic counters, reported when a watchdog fires so the log tells
	// WHICH loop path starved the reader instead of a bare "no grain".
	var nTimeout, nLate, nEarly, nInvalid, nSameIdx uint64
	diag := func() string {
		rt, rerr := reader.Runtime()
		head := uint64(0)
		if rerr == nil {
			head = rt.HeadIndex
		}
		return fmt.Sprintf("head=%d lastIdx=%d timeouts=%d late=%d early=%d invalid=%d sameIdx=%d",
			head, lastIdx, nTimeout, nLate, nEarly, nInvalid, nSameIdx)
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

		// Once the flow has been live, a prolonged head stall means the writer
		// recreated the flow (new generation) and our reader is stuck on the
		// dead one. Return so the handler re-runs Run() against the fresh flow.
		if started && time.Since(lastProgress) > staleTimeout {
			return fmt.Errorf("flow %s stalled for %v (writer likely recreated the flow; %s); "+
				"restarting source to re-open the reader", flowID, staleTimeout, diag())
		}

		// Same idea for the pre-start phase: if no grain ever arrives the
		// reader is likely bound to a stale mirror of a recreated flow.
		if !started && time.Since(lastProgress) > firstGrainTimeout {
			return fmt.Errorf("no grain within %v of opening flow %s (%s); "+
				"restarting source to re-open the reader", firstGrainTimeout, flowID, diag())
		}

		var grain mxl.Grain
		grain, err = reader.GetGrain(idx, readTimeout)
		switch {
		case err == nil:
			// fall through
		case errors.Is(err, mxl.ErrTimeout):
			nTimeout++
			if idx, err = freshestIndex(); err != nil {
				return fmt.Errorf("resync after timeout: %w", err)
			}
			continue
		case errors.Is(err, mxl.ErrOutOfRangeLate):
			nLate++
			// Encoder + ring window can't keep up at all; jump to head.
			if idx, err = freshestIndex(); err != nil {
				return fmt.Errorf("resync after fall-behind: %w", err)
			}
			continue
		case errors.Is(err, mxl.ErrOutOfRangeEarly):
			nEarly++
			time.Sleep(2 * time.Millisecond)
			continue
		default:
			return fmt.Errorf("get grain: %w", err)
		}

		if grain.Invalid() || !grain.Complete() {
			nInvalid++
			// One-shot forensic scan: how far behind head do grains become
			// valid? Tells whether safetyMargin is simply too small for
			// same-node (mirror-less) consumption.
			if nInvalid == 1 {
				rt, rerr := reader.Runtime()
				if rerr == nil {
					scan := ""
					for back := uint64(1); back <= 6 && rt.HeadIndex > back; back++ {
						g, gerr := reader.GetGrain(rt.HeadIndex-back, 10*time.Millisecond)
						switch {
						case gerr != nil:
							scan += fmt.Sprintf(" head-%d:err(%v)", back, gerr)
						case g.Invalid():
							scan += fmt.Sprintf(" head-%d:invalid", back)
						case !g.Complete():
							scan += fmt.Sprintf(" head-%d:incomplete", back)
						default:
							scan += fmt.Sprintf(" head-%d:OK", back)
						}
					}
					s.Log(logger.Warn, "grain validity scan (head=%d):%s", rt.HeadIndex, scan)
				}
			}
			// Adaptive fallback: freshest grain keeps being invalid (e.g.
			// same-node read hits uncommitted/skip grains at the head), so
			// step further back instead of hammering the same index.
			backoff := min(1+nInvalid/1000, uint64(info.Config.Discrete.GrainCount)-1)
			rt, rerr := reader.Runtime()
			if rerr != nil {
				return fmt.Errorf("resync: %w", rerr)
			}
			if rt.HeadIndex > backoff {
				idx = rt.HeadIndex - backoff
			} else {
				idx = rt.HeadIndex
			}
			continue
		}

		// Skip if we re-pulled the same grain (writer hasn't advanced yet).
		if grain.Index == lastIdx {
			nSameIdx++
			time.Sleep(time.Millisecond)
			idx, err = freshestIndex()
			if err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		}
		lastIdx = grain.Index
		started = true
		lastProgress = time.Now()

		err = unpacker.Unpack(grain.Payload, srcStride, yPlane, cbPlane, crPlane)
		if err != nil {
			s.Log(logger.Error, "v210 unpack: %v", err)
			idx, err = freshestIndex()
			if err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		}

		// Queue the index before the frame, so the encoder's goroutine
		// can never pop for an access unit whose frame has not been
		// recorded yet.
		if !pending.push(grain.Index) {
			return fmt.Errorf("encoder is %d frames behind; access units are not "+
				"coming back one per frame", pending.depth())
		}

		err = enc.Encode(yPlane, cbPlane, crPlane)
		if err != nil {
			return fmt.Errorf("encoder write: %w", err)
		}

		// Always pick the freshest available grain next, dropping any
		// frames produced by the writer while we were busy encoding.
		idx, err = freshestIndex()
		if err != nil {
			return fmt.Errorf("resync: %w", err)
		}
	}
}

// encoderParamsFromConf reads the mxlH264* fields off the path config and
// produces the EncoderParams the encoder consumes. Geometry and rate are
// derived from the flow, not configurable per-path. onData is the per-AU
// callback that does RTP packetization and substream publishing.
func encoderParamsFromConf(
	cnf *conf.Path,
	width, height int, rate mxl.Rational,
	onData func([][]byte),
) EncoderParams {
	return EncoderParams{
		FFmpegPath: cnf.MXLFFmpegPath,
		Width:      uint32(width),
		Height:     uint32(height),
		RateNum:    rate.Num,
		RateDen:    rate.Den,
		Preset:     cnf.MXLH264Preset,
		Profile:    cnf.MXLH264Profile,
		Bitrate:    uint32(cnf.MXLH264Bitrate),
		IDRPeriod:  uint32(cnf.MXLH264IDRPeriod),
		OnData:     onData,
	}
}

// mxlURL is what an mxl:// source string names: a domain, the flow to read,
// and optionally a second flow whose audio is published on the same path.
type mxlURL struct {
	domain      string
	flowID      string
	audioFlowID string
}

// parseMXLURL accepts URLs of the form:
//
//	mxl:///<absolute-domain-path>/<flow-uuid>
//	mxl:///<absolute-domain-path>/<video-uuid>?audio=<audio-uuid>
func parseMXLURL(s string) (u mxlURL, err error) {
	parsed, err := url.Parse(s)
	if err != nil {
		return mxlURL{}, fmt.Errorf("parse mxl URL: %w", err)
	}
	if parsed.Scheme != "mxl" {
		return mxlURL{}, fmt.Errorf("not an mxl:// URL: %s", s)
	}
	if parsed.Host != "" {
		return mxlURL{}, fmt.Errorf("mxl URL host must be empty (use mxl:///path/flow), got %q", parsed.Host)
	}

	path := parsed.Path
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return mxlURL{}, fmt.Errorf("mxl URL path must contain /<domain>/<flow-uuid>: %s", s)
	}
	u.domain = path[:i]
	u.flowID = path[i+1:]
	if u.domain == "" || u.flowID == "" {
		return mxlURL{}, fmt.Errorf("mxl URL path malformed: %s", s)
	}

	// An unknown query is refused rather than ignored. The audio flow of a
	// joined path is carried here, so a typo in the key would otherwise
	// publish picture only and look like the audio flow was the problem.
	q := parsed.Query()
	for key := range q {
		if key != "audio" {
			return mxlURL{}, fmt.Errorf("mxl URL carries unknown query %q: %s", key, s)
		}
	}
	if vals := q["audio"]; len(vals) > 0 {
		if len(vals) > 1 {
			return mxlURL{}, fmt.Errorf("mxl URL names %d audio flows, one at most: %s", len(vals), s)
		}
		u.audioFlowID = vals[0]
		if u.audioFlowID == "" {
			return mxlURL{}, fmt.Errorf("mxl URL carries an empty audio flow id: %s", s)
		}
		if u.audioFlowID == u.flowID {
			return mxlURL{}, fmt.Errorf("mxl URL joins flow %s to itself", u.flowID)
		}
	}
	return u, nil
}
