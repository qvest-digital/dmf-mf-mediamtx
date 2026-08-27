package mxl

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// countingParent stands in for a path. SetReady is answered with a nil
// SubStream on purpose: a caller that gets past the media check would then
// dereference it, so the test tells "refused early" from "went on to write"
// without needing a real stream.
type countingParent struct {
	ready    int
	notReady int
}

func (p *countingParent) Log(logger.Level, string, ...any) {}

func (p *countingParent) SetReady(defs.PathSourceStaticSetReadyReq) defs.PathSourceStaticSetReadyRes {
	p.ready++
	return defs.PathSourceStaticSetReadyRes{}
}

func (p *countingParent) SetNotReady(defs.PathSourceStaticSetNotReadyReq) {
	p.notReady++
}

func TestPublisherRefusesAMediaItDoesNotCarry(t *testing.T) {
	// A substream looks its writers up by media pointer and dereferences the
	// result without checking it, so writing with a media it was not created
	// with is a nil dereference that takes down the process and every other
	// path on it. Two media values that describe the same track are still two
	// pointers, which is exactly how a joined path got this wrong: the
	// publisher was built with one and each track wrote with its own.
	registered := videoMedia()
	stranger := videoMedia()
	require.NotSame(t, registered, stranger,
		"videoMedia must return a fresh value, or this proves nothing")

	parent := &countingParent{}
	pub := &publisher{parent: parent, medias: []*description.Media{registered}}

	require.NotPanics(t, func() {
		pub.write(stranger, 0, 90000, []*rtp.Packet{{}})
	})
	require.Zero(t, parent.ready,
		"a media the publisher does not carry must be refused before the path"+
			" is asked for a stream")
}

func TestPublisherCloseIsQuietUntilSomethingWasPublished(t *testing.T) {
	// Refusing a write must not leave the path thinking a source came and
	// went: SetNotReady on a path that was never made ready is a state change
	// nobody asked for.
	parent := &countingParent{}
	pub := &publisher{parent: parent, medias: []*description.Media{videoMedia()}}

	pub.write(videoMedia(), 0, 90000, []*rtp.Packet{{}})
	pub.close()

	require.Zero(t, parent.notReady)
}
