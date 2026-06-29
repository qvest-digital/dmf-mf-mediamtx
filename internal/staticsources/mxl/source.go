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
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"

	mxl "github.com/qvest-digital/mxl-dmf-demo/go"
)

const (
	readTimeout = 500 * time.Millisecond

	// staleTimeout: once grains have started flowing, if the flow's head index
	// stops advancing for this long the writer has almost certainly torn down
	// and recreated the flow — e.g. an SRT source reconnect makes the bridge
	// build a new flow generation. Our reader then holds a handle to the old,
	// now-static shared memory and would spin forever re-reading the last grain
	// (frozen video, no error). Returning lets the static-source handler re-run
	// Run() with a fresh instance/reader that re-discovers the current flow —
	// the same self-heal the demo-app compositor already does on FLOW_INVALID.
	staleTimeout = 2 * time.Second
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
	domain, flowID, err := parseMXLURL(params.ResolvedSource)
	if err != nil {
		return err
	}
	s.Log(logger.Info, "opening MXL domain %q flow %s", domain, flowID)

	inst, err := mxl.NewInstance(domain, "")
	if err != nil {
		return fmt.Errorf("open MXL instance: %w", err)
	}
	defer inst.Close()

	reader, err := inst.NewReader(flowID)
	if err != nil {
		return fmt.Errorf("open MXL reader: %w", err)
	}
	defer reader.Close()

	info, err := reader.Info()
	if err != nil {
		return fmt.Errorf("read flow info: %w", err)
	}
	if info.Config.Common.Format != mxl.FormatVideo {
		return fmt.Errorf("flow %s is not video (format=%s)", flowID, info.Config.Common.Format)
	}

	defJSON, err := inst.FlowDef(flowID)
	if err != nil {
		return fmt.Errorf("read flow def: %w", err)
	}
	var def flowDef
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		return fmt.Errorf("parse flow def: %w", err)
	}
	if def.MediaType != "video/v210" {
		return fmt.Errorf("unsupported media_type %q (only video/v210 in v1)", def.MediaType)
	}
	width, height := def.FrameWidth, def.FrameHeight
	srcStride := int(info.Config.Discrete.SliceSizes[0])
	rate := info.Config.Common.GrainRate
	fps := uint32((rate.Num + rate.Den/2) / rate.Den)

	s.Log(logger.Info, "flow geometry %dx%d @ %d/%d (~%dfps), stride=%d, ringSize=%d",
		width, height, rate.Num, rate.Den, fps, srcStride, info.Config.Discrete.GrainCount)

	unpacker, err := NewV210Unpacker(width, height)
	if err != nil {
		return fmt.Errorf("v210 unpacker: %w", err)
	}

	media := &description.Media{
		Type: description.MediaTypeVideo,
		Formats: []format.Format{&format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
		}},
	}

	rtpEnc := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: s.RTPMaxPayloadSize,
	}
	if err := rtpEnc.Init(); err != nil {
		return fmt.Errorf("rtp encoder: %w", err)
	}

	// State owned by the OnData callback. OnData runs on the encoder's
	// reader goroutine; nothing else touches these.
	var subStream *stream.SubStream
	var startNTPSet bool
	var startNTP time.Time
	var lastPTS int64 = -1

	onData := func(au [][]byte) {
		if len(au) == 0 {
			return
		}
		// Wall-clock-derived PTS keeps timing correct even when the
		// consumer drops grains under CPU pressure (see freshest-grain
		// strategy in the main loop below). Two AUs may land in the
		// same 90 kHz tick when the encoder bursts; clamp so PTS stays
		// strictly monotonic (downstream RTSP/HLS muxers require it).
		now := time.Now()
		if !startNTPSet {
			startNTP = now
			startNTPSet = true
		}
		elapsed := now.Sub(startNTP)
		ptsTicks := elapsed.Nanoseconds() * 90000 / int64(time.Second)
		if ptsTicks <= lastPTS {
			ptsTicks = lastPTS + 1
		}
		lastPTS = ptsTicks
		ntp := now

		pkts, err := rtpEnc.Encode(au)
		if err != nil {
			s.Log(logger.Error, "rtp encode: %v", err)
			return
		}
		if subStream == nil {
			res := s.Parent.SetReady(defs.PathSourceStaticSetReadyReq{
				Desc:          &description.Session{Medias: []*description.Media{media}},
				UseRTPPackets: true,
			})
			if res.Err != nil {
				panic("should not happen")
			}
			subStream = res.SubStream
		}
		for _, pkt := range pkts {
			pkt.Timestamp = uint32(ptsTicks)
			subStream.WriteUnit(media, media.Formats[0], &unit.Unit{
				PTS:        ptsTicks,
				NTP:        ntp,
				RTPPackets: []*rtp.Packet{pkt},
			})
		}
	}

	defer func() {
		if subStream != nil {
			s.Parent.SetNotReady(defs.PathSourceStaticSetNotReadyReq{})
		}
	}()

	enc, err := NewH264Encoder(encoderParamsFromConf(params.Conf, width, height, fps, onData))
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
	// "live" — if encoding is slower than the writer's pace, we drop
	// frames instead of falling behind the ring buffer window. Wall-clock
	// PTS (above) keeps downstream timing correct regardless of drops.
	//
	// The margin keeps us from racing the writer at the leading edge of
	// the buffer; with a 5-grain ring this leaves ~3 grains of headroom.
	const safetyMargin uint64 = 1
	freshestIndex := func() (uint64, error) {
		rt, err := reader.Runtime()
		if err != nil {
			return 0, err
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
	var lastIdx uint64
	// Self-heal watchdog state: started flips true on the first decoded grain;
	// lastProgress is bumped whenever the head advances. See staleTimeout.
	var started bool
	lastProgress := time.Now()

	for {
		select {
		case err := <-encErr:
			if err != nil {
				return fmt.Errorf("encoder: %w", err)
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
			return fmt.Errorf("flow %s stalled for %v (writer likely recreated the flow); "+
				"restarting source to re-open the reader", flowID, staleTimeout)
		}

		grain, err := reader.GetGrain(idx, readTimeout)
		switch {
		case err == nil:
			// fall through
		case errors.Is(err, mxl.ErrTimeout):
			if idx, err = freshestIndex(); err != nil {
				return fmt.Errorf("resync after timeout: %w", err)
			}
			continue
		case errors.Is(err, mxl.ErrOutOfRangeLate):
			// Encoder + ring window can't keep up at all; jump to head.
			if idx, err = freshestIndex(); err != nil {
				return fmt.Errorf("resync after fall-behind: %w", err)
			}
			continue
		case errors.Is(err, mxl.ErrOutOfRangeEarly):
			time.Sleep(2 * time.Millisecond)
			continue
		default:
			return fmt.Errorf("get grain: %w", err)
		}

		if grain.Invalid() || !grain.Complete() {
			idx, err = freshestIndex()
			if err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		}

		// Skip if we re-pulled the same grain (writer hasn't advanced yet).
		if grain.Index == lastIdx {
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

		if err := unpacker.Unpack(grain.Payload, srcStride, yPlane, cbPlane, crPlane); err != nil {
			s.Log(logger.Error, "v210 unpack: %v", err)
			idx, err = freshestIndex()
			if err != nil {
				return fmt.Errorf("resync: %w", err)
			}
			continue
		}

		if err := enc.Encode(yPlane, cbPlane, crPlane); err != nil {
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
// produces the EncoderParams the encoder consumes. width/height/fps are
// derived from the flow, not configurable per-path. onData is the per-AU
// callback that does RTP packetization and substream publishing.
func encoderParamsFromConf(
	cnf *conf.Path,
	width, height int, fps uint32,
	onData func([][]byte),
) EncoderParams {
	return EncoderParams{
		FFmpegPath: cnf.MXLFFmpegPath,
		Width:      uint32(width),
		Height:     uint32(height),
		FPS:        fps,
		Preset:     cnf.MXLH264Preset,
		Profile:    cnf.MXLH264Profile,
		Bitrate:    uint32(cnf.MXLH264Bitrate),
		IDRPeriod:  uint32(cnf.MXLH264IDRPeriod),
		OnData:     onData,
	}
}

// parseMXLURL accepts URLs of the form:
//
//	mxl:///<absolute-domain-path>/<flow-uuid>
func parseMXLURL(s string) (domain, flowID string, err error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("parse mxl URL: %w", err)
	}
	if u.Scheme != "mxl" {
		return "", "", fmt.Errorf("not an mxl:// URL: %s", s)
	}
	if u.Host != "" {
		return "", "", fmt.Errorf("mxl URL host must be empty (use mxl:///path/flow), got %q", u.Host)
	}
	path := u.Path
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return "", "", fmt.Errorf("mxl URL path must contain /<domain>/<flow-uuid>: %s", s)
	}
	domain = path[:i]
	flowID = path[i+1:]
	if domain == "" || flowID == "" {
		return "", "", fmt.Errorf("mxl URL path malformed: %s", s)
	}
	return domain, flowID, nil
}
