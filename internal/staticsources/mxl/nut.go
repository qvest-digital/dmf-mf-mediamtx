package mxl

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// nutReader reads frames from a NUT stream, the container ffmpeg's nut muxer
// writes (libavformat/nut.txt). NUT states each frame's size in its header,
// so a frame is complete the moment its last byte arrives, where a raw
// Annex-B stream only shows a picture's end once the next picture starts.
//
// Only what a single-stream reader needs is decoded: the frame code table
// and elision headers of the main header, and the frame headers. Stream,
// syncpoint, index and info packets are skipped whole, and checksums are
// not verified.
type nutReader struct {
	r        *bufio.Reader
	codes    [256]nutFrameCode
	headers  [][]byte
	haveMain bool
	// onFrameStart, when set, is called as a frame's first byte arrives.
	onFrameStart func()
}

type nutFrameCode struct {
	flags         uint64
	sizeMul       uint64
	sizeLSB       uint64
	reservedCount uint64
	headerIdx     uint64
}

const (
	nutFlagCodedPTS  = 8
	nutFlagStreamID  = 16
	nutFlagSizeMSB   = 32
	nutFlagChecksum  = 64
	nutFlagReserved  = 128
	nutFlagSMData    = 256
	nutFlagHeaderIdx = 1024
	nutFlagMatchTime = 2048
	nutFlagCoded     = 4096
	nutFlagInvalid   = 8192

	nutMainStartcode = 0x7A561F5F04AD + uint64('N'<<8+'M')<<48

	nutIDString = "nut/multimedia container\x00"
	// Larger than any frame or packet this source writes; a value past it
	// means the stream is not what it was assumed to be.
	nutMaxSize = 64 << 20
)

func newNUTReader(r io.Reader) *nutReader {
	return &nutReader{r: bufio.NewReaderSize(r, 1<<16)}
}

func (n *nutReader) v() (uint64, error) {
	return nutV(n.r)
}

// nutV reads NUT's variable-length unsigned integer: 7 bits per byte, most
// significant first, the top bit set on every byte but the last.
func nutV(r io.ByteReader) (uint64, error) {
	var x uint64
	for range 10 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		x = x<<7 | uint64(b&0x7f)
		if b&0x80 == 0 {
			return x, nil
		}
	}
	return 0, errors.New("nut: varint too long")
}

func (n *nutReader) skip(size uint64) error {
	_, err := n.r.Discard(int(size))
	return err
}

// next returns the payload of the next frame, elision header included.
func (n *nutReader) next() ([]byte, error) {
	if !n.haveMain {
		id := make([]byte, len(nutIDString))
		if _, err := io.ReadFull(n.r, id); err != nil {
			return nil, err
		}
		if string(id) != nutIDString {
			return nil, errors.New("nut: missing file ID string")
		}
	}
	for {
		code, err := n.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if code == 'N' {
			if err := n.readPacket(); err != nil {
				return nil, err
			}
			continue
		}
		if !n.haveMain {
			return nil, errors.New("nut: frame before main header")
		}
		if n.onFrameStart != nil {
			n.onFrameStart()
		}
		return n.readFrame(code)
	}
}

// readPacket reads a startcode packet whose leading 'N' has been consumed.
func (n *nutReader) readPacket() error {
	var rest [7]byte
	if _, err := io.ReadFull(n.r, rest[:]); err != nil {
		return err
	}
	startcode := uint64('N')
	for _, b := range rest {
		startcode = startcode<<8 | uint64(b)
	}
	size, err := n.v()
	if err != nil {
		return err
	}
	if size > 4096 {
		if err := n.skip(4); err != nil {
			return err
		}
	}
	if size > nutMaxSize {
		return fmt.Errorf("nut: packet of %d bytes", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(n.r, body); err != nil {
		return err
	}
	if startcode == nutMainStartcode {
		if err := n.parseMain(body); err != nil {
			return fmt.Errorf("nut: main header: %w", err)
		}
		n.haveMain = true
	}
	return nil
}

func (n *nutReader) parseMain(body []byte) error {
	if len(body) < 4 {
		return errors.New("short packet")
	}
	// The packet ends in its checksum.
	b := bytes.NewReader(body[:len(body)-4])
	v := func(count int) (uint64, error) {
		var x uint64
		var err error
		for range count {
			if x, err = nutV(b); err != nil {
				return 0, err
			}
		}
		return x, nil
	}
	version, err := v(1)
	if err != nil {
		return err
	}
	if version > 3 {
		if _, err := v(1); err != nil { // minor version
			return err
		}
	}
	// Stream count and max distance, then the time bases.
	if _, err := v(2); err != nil {
		return err
	}
	timeBases, err := v(1)
	if err != nil {
		return err
	}
	if _, err := v(2 * int(timeBases)); err != nil {
		return err
	}

	var mul, headIdx uint64 = 1, 0
	for i := 0; i < 256; {
		flags, err := v(1)
		if err != nil {
			return err
		}
		fields, err := v(1)
		if err != nil {
			return err
		}
		vals := make([]uint64, fields)
		for f := range vals {
			if vals[f], err = nutV(b); err != nil {
				return err
			}
		}
		// Fields 0 (pts delta), 2 (stream) and 6 (match time) persist or
		// are not needed here.
		var size, res uint64
		if fields > 1 {
			mul = vals[1]
		}
		if fields > 3 {
			size = vals[3]
		}
		if fields > 4 {
			res = vals[4]
		}
		count := mul - size
		if fields > 5 {
			count = vals[5]
		}
		if fields > 7 {
			headIdx = vals[7]
		}
		if count == 0 || count > 256 {
			return fmt.Errorf("frame code count %d", count)
		}
		for j := uint64(0); j < count && i < 256; i++ {
			if i == 'N' {
				n.codes[i] = nutFrameCode{flags: nutFlagInvalid}
				continue
			}
			n.codes[i] = nutFrameCode{
				flags: flags, sizeMul: mul, sizeLSB: size + j,
				reservedCount: res, headerIdx: headIdx,
			}
			j++
		}
	}

	n.headers = [][]byte{nil}
	if b.Len() == 0 {
		return nil
	}
	extra, err := v(1) // header count minus one
	if err != nil {
		return err
	}
	for range extra {
		l, err := v(1)
		if err != nil {
			return err
		}
		h := make([]byte, l)
		if _, err := io.ReadFull(b, h); err != nil {
			return err
		}
		n.headers = append(n.headers, h)
	}
	return nil
}

// readFrame reads the frame whose frame code byte has been consumed.
func (n *nutReader) readFrame(code byte) ([]byte, error) {
	fc := n.codes[code]
	flags := fc.flags
	if flags&nutFlagInvalid != 0 {
		return nil, fmt.Errorf("nut: invalid frame code %d", code)
	}
	if flags&nutFlagCoded != 0 {
		x, err := n.v()
		if err != nil {
			return nil, err
		}
		flags ^= x
	}
	read := func(set uint64) (uint64, error) {
		if flags&set == 0 {
			return 0, nil
		}
		return n.v()
	}
	if _, err := read(nutFlagStreamID); err != nil {
		return nil, err
	}
	if _, err := read(nutFlagCodedPTS); err != nil {
		return nil, err
	}
	size := fc.sizeLSB
	msb, err := read(nutFlagSizeMSB)
	if err != nil {
		return nil, err
	}
	size += fc.sizeMul * msb
	if _, err := read(nutFlagMatchTime); err != nil {
		return nil, err
	}
	headerIdx := fc.headerIdx
	if flags&nutFlagHeaderIdx != 0 {
		if headerIdx, err = n.v(); err != nil {
			return nil, err
		}
	}
	reserved := fc.reservedCount
	if flags&nutFlagReserved != 0 {
		if reserved, err = n.v(); err != nil {
			return nil, err
		}
	}
	for range reserved {
		if _, err := n.v(); err != nil {
			return nil, err
		}
	}
	if flags&nutFlagChecksum != 0 {
		if err := n.skip(4); err != nil {
			return nil, err
		}
	}
	if flags&nutFlagSMData != 0 {
		return nil, errors.New("nut: side data in frames is not supported")
	}
	if headerIdx >= uint64(len(n.headers)) {
		return nil, fmt.Errorf("nut: elision header %d of %d", headerIdx, len(n.headers))
	}
	if size > 4096 {
		headerIdx = 0
	}
	head := n.headers[headerIdx]
	if size < uint64(len(head)) || size > nutMaxSize {
		return nil, fmt.Errorf("nut: frame of %d bytes", size)
	}
	frame := make([]byte, size)
	copy(frame, head)
	if _, err := io.ReadFull(n.r, frame[len(head):]); err != nil {
		return nil, err
	}
	return frame, nil
}

// splitAnnexB splits one access unit in Annex-B form into its NAL units.
func splitAnnexB(b []byte) [][]byte {
	var nals [][]byte
	for {
		sc, l := findStartCode(b)
		if sc < 0 {
			return nals
		}
		b = b[sc+l:]
		next, _ := findStartCode(b)
		if next < 0 {
			return append(nals, b)
		}
		nals = append(nals, b[:next])
		b = b[next:]
	}
}
