package mxl

import (
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// publisher owns a path's substream and hands it to whichever track produces
// first.
//
// A substream is created once, with every media the path will ever carry named
// up front, so a joined path cannot let each of its two tracks create its own.
// Creation is therefore deferred to the first packet from either side and
// guarded, and the two goroutines share one wall-clock anchor so they cannot
// disagree about when the path started.
type publisher struct {
	parent parent
	medias []*description.Media

	mu     sync.Mutex
	sub    *stream.SubStream
	anchor time.Time
	failed bool
}

// acquire returns the substream, creating it on the first call. It reports
// false once creation has failed, so a caller stops rather than retrying it
// per packet.
func (p *publisher) acquire() (*stream.SubStream, time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failed {
		return nil, time.Time{}, false
	}
	if p.sub != nil {
		return p.sub, p.anchor, true
	}

	res := p.parent.SetReady(defs.PathSourceStaticSetReadyReq{
		Desc:          &description.Session{Medias: p.medias},
		UseRTPPackets: true,
	})
	if res.Err != nil {
		p.failed = true
		return nil, time.Time{}, false
	}
	p.sub = res.SubStream
	p.anchor = time.Now()
	return p.sub, p.anchor, true
}

// write publishes packets on one of the path's tracks.
//
// pts is in that track's own clock rate, which is what the muxers downstream
// expect; clockRate is what turns it into the offset the NTP timestamp
// carries. NTP follows PTS rather than the wall clock so the absolute
// timestamps are as uniform as the timeline, and so the two tracks of a
// joined path stay in step with each other rather than each with its own
// encoder's output lag.
func (p *publisher) write(media *description.Media, pts int64, clockRate int64, pkts []*rtp.Packet) {
	sub, anchor, ok := p.acquire()
	if !ok {
		return
	}

	ntp := anchor.Add(time.Duration(pts) * time.Second / time.Duration(clockRate))
	for _, pkt := range pkts {
		sub.WriteUnit(media, media.Formats[0], &unit.Unit{
			PTS:        pts,
			NTP:        ntp,
			RTPPackets: []*rtp.Packet{pkt},
		})
	}
}

// close tells the path nothing further is coming, once, and only if anything
// was ever published.
func (p *publisher) close() {
	p.mu.Lock()
	published := p.sub != nil
	p.sub = nil
	p.mu.Unlock()

	if published {
		p.parent.SetNotReady(defs.PathSourceStaticSetNotReadyReq{})
	}
}
