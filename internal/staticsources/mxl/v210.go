package mxl

import (
	"encoding/binary"
	"fmt"
)

// V210Unpacker converts SMPTE 372M / Apple v210 (10-bit packed 4:2:2) into
// planar YUV 4:2:0 8-bit suitable for an H.264 encoder. Bit-depth is reduced
// from 10 → 8 by dropping the two LSBs of each component; vertical chroma is
// downsampled by averaging adjacent line pairs.
//
// Width must be a multiple of 6 (v210's packing unit). Height must be even.
// All HD broadcast resolutions in use today satisfy both.
type V210Unpacker struct {
	width, height int
	// Reusable per-line chroma buffers (top and bottom of each 2-line group).
	cbTop, crTop []byte
	cbBot, crBot []byte
}

// NewV210Unpacker returns an unpacker for frames of the given geometry. The
// unpacker is not safe for concurrent use; create one per goroutine.
func NewV210Unpacker(width, height int) (*V210Unpacker, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("v210: width and height must be positive")
	}
	if width%6 != 0 {
		return nil, fmt.Errorf("v210: width must be divisible by 6, got %d", width)
	}
	if height%2 != 0 {
		return nil, fmt.Errorf("v210: height must be even, got %d", height)
	}
	return &V210Unpacker{
		width:  width,
		height: height,
		cbTop:  make([]byte, width/2),
		crTop:  make([]byte, width/2),
		cbBot:  make([]byte, width/2),
		crBot:  make([]byte, width/2),
	}, nil
}

// Unpack writes the input v210 frame into the supplied YUV 4:2:0 planes.
// srcStride is the v210 line stride in bytes (typically rounded up to 128).
// The y plane must be width*height bytes; cb and cr must each be
// (width/2)*(height/2) bytes.
func (u *V210Unpacker) Unpack(src []byte, srcStride int, y, cb, cr []byte) error {
	if srcStride < (u.width/6)*16 {
		return fmt.Errorf("v210: srcStride %d too small for width %d", srcStride, u.width)
	}
	if len(src) < srcStride*u.height {
		return fmt.Errorf("v210: src %d bytes too small for %d×%d at stride %d",
			len(src), u.width, u.height, srcStride)
	}
	if len(y) < u.width*u.height {
		return fmt.Errorf("v210: y plane %d bytes too small for %d×%d", len(y), u.width, u.height)
	}
	cwh := (u.width / 2) * (u.height / 2)
	if len(cb) < cwh || len(cr) < cwh {
		return fmt.Errorf("v210: chroma planes too small (want %d each)", cwh)
	}

	for row := 0; row < u.height; row += 2 {
		topSrc := src[row*srcStride:]
		botSrc := src[(row+1)*srcStride:]
		topY := y[row*u.width : (row+1)*u.width]
		botY := y[(row+1)*u.width : (row+2)*u.width]

		unpackV210Line(topSrc, topY, u.cbTop, u.crTop)
		unpackV210Line(botSrc, botY, u.cbBot, u.crBot)

		halfW := u.width / 2
		cbOut := cb[(row/2)*halfW : (row/2+1)*halfW]
		crOut := cr[(row/2)*halfW : (row/2+1)*halfW]
		for c := 0; c < halfW; c++ {
			cbOut[c] = byte((uint16(u.cbTop[c]) + uint16(u.cbBot[c])) >> 1)
			crOut[c] = byte((uint16(u.crTop[c]) + uint16(u.crBot[c])) >> 1)
		}
	}
	return nil
}

// unpackV210Line decodes one row of v210 into Y, Cb, Cr lines (8-bit). Caller
// ensures len(y) is a multiple of 6 and matches the width; cb/cr are half.
func unpackV210Line(src, y, cb, cr []byte) {
	width := len(y)
	groups := width / 6
	for g := 0; g < groups; g++ {
		off := g * 16
		w0 := binary.LittleEndian.Uint32(src[off:])
		w1 := binary.LittleEndian.Uint32(src[off+4:])
		w2 := binary.LittleEndian.Uint32(src[off+8:])
		w3 := binary.LittleEndian.Uint32(src[off+12:])

		// Each 32-bit word: bits [0:9]=A, [10:19]=B, [20:29]=C, [30:31]=unused.
		// Component order per SMPTE 372M / QuickTime TN2162:
		//   w0: Cb0  Y0   Cr0
		//   w1: Y1   Cb1  Y2
		//   w2: Cr1  Y3   Cb2
		//   w3: Y4   Cr2  Y5
		// Convert 10-bit → 8-bit by dropping the two LSBs.
		yo := g * 6
		co := g * 3
		y[yo+0] = byte((w0 >> 12) & 0xFF) // Y0 high 8 of 10
		y[yo+1] = byte((w1 >> 2) & 0xFF)  // Y1
		y[yo+2] = byte((w1 >> 22) & 0xFF) // Y2
		y[yo+3] = byte((w2 >> 12) & 0xFF) // Y3
		y[yo+4] = byte((w3 >> 2) & 0xFF)  // Y4
		y[yo+5] = byte((w3 >> 22) & 0xFF) // Y5

		cb[co+0] = byte((w0 >> 2) & 0xFF)  // Cb0
		cb[co+1] = byte((w1 >> 12) & 0xFF) // Cb1
		cb[co+2] = byte((w2 >> 22) & 0xFF) // Cb2

		cr[co+0] = byte((w0 >> 22) & 0xFF) // Cr0
		cr[co+1] = byte((w2 >> 2) & 0xFF)  // Cr1
		cr[co+2] = byte((w3 >> 12) & 0xFF) // Cr2
	}
}
