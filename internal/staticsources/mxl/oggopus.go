package mxl

import (
	"bytes"
	"fmt"
)

// Ogg framing, RFC 3533 section 6. Only the fields needed to walk pages and
// recover packet boundaries are named.
const (
	oggCapturePattern = "OggS"
	oggHeaderLen      = 27 // through the segment count, before the segment table
	oggSegCountOffset = 26
	oggMaxSegments    = 255
	oggSegmentFull    = 255 // a 255-byte segment continues into the next one
)

// oggOpusExtractor turns the Ogg-Opus stream ffmpeg writes on stdout back
// into the Opus packets it encoded.
//
// ffmpeg has no bare-packet output format for Opus, so the encoder is asked
// for Ogg and the framing is undone here: a page carries a segment table, and
// a packet is the concatenation of segments up to and including the first one
// shorter than 255 bytes. One Opus packet is one RTP payload.
//
// The first two packets of an Ogg-Opus stream are the OpusHead and OpusTags
// headers (RFC 7845). They describe the stream rather than carrying audio and
// are dropped: the RTP format is built from the flow's own declaration.
type oggOpusExtractor struct {
	buf []byte

	// partial accumulates a packet spanning more than one page, which
	// happens when a packet's last segment is the last on its page.
	partial []byte

	headersSeen int
}

// push appends bytes read from the encoder and returns whatever complete
// audio packets they finished, in order.
//
// Ownership: returned slices are freshly allocated, so a caller may retain
// them past the next call.
func (e *oggOpusExtractor) push(b []byte) ([][]byte, error) {
	e.buf = append(e.buf, b...)
	var out [][]byte

	for {
		// Resynchronise on the capture pattern. A well-formed stream is
		// already aligned; scanning covers a mid-stream restart.
		i := bytes.Index(e.buf, []byte(oggCapturePattern))
		if i < 0 {
			// Keep the last few bytes: a capture pattern may straddle
			// the boundary between this read and the next.
			if len(e.buf) > len(oggCapturePattern)-1 {
				e.buf = e.buf[len(e.buf)-(len(oggCapturePattern)-1):]
			}
			return out, nil
		}
		if i > 0 {
			e.buf = e.buf[i:]
		}
		if len(e.buf) < oggHeaderLen {
			return out, nil
		}

		nsegs := int(e.buf[oggSegCountOffset])
		if nsegs > oggMaxSegments {
			return nil, fmt.Errorf("ogg page declares %d segments", nsegs)
		}
		pageLen := oggHeaderLen + nsegs
		if len(e.buf) < pageLen {
			return out, nil
		}

		table := e.buf[oggHeaderLen:pageLen]
		body := 0
		for _, l := range table {
			body += int(l)
		}
		if len(e.buf) < pageLen+body {
			return out, nil
		}

		payload := e.buf[pageLen : pageLen+body]
		out = e.splitSegments(table, payload, out)
		e.buf = e.buf[pageLen+body:]
	}
}

// splitSegments walks one page's segment table, appending each packet it
// completes. A segment of exactly 255 bytes continues into the next segment,
// so a packet ends at the first segment shorter than that; a packet whose
// final segment is the last on the page carries over to the following page.
func (e *oggOpusExtractor) splitSegments(table, payload []byte, out [][]byte) [][]byte {
	off := 0
	for _, l := range table {
		e.partial = append(e.partial, payload[off:off+int(l)]...)
		off += int(l)
		if l == oggSegmentFull {
			continue
		}
		pkt := e.partial
		e.partial = nil
		if len(pkt) == 0 {
			continue
		}
		// OpusHead and OpusTags describe the stream, not audio.
		if e.headersSeen < 2 {
			e.headersSeen++
			continue
		}
		out = append(out, pkt)
	}
	return out
}
