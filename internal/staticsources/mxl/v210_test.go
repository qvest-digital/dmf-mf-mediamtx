package mxl

import (
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// packV210Line encodes one line as v210: the 4:2:2 component sequence
// Cb Y Cr Y per pixel pair, three 10-bit components per little-endian word,
// zero-padded to stride. Each 8-bit sample becomes the top 8 of 10 bits.
func packV210Line(y, cb, cr []byte, stride int) []byte {
	comps := make([]uint32, 0, 4*len(cb))
	for p := range cb {
		comps = append(comps, uint32(cb[p]), uint32(y[2*p]), uint32(cr[p]), uint32(y[2*p+1]))
	}
	out := make([]byte, stride)
	for w := 0; w*3 < len(comps); w++ {
		var word uint32
		for k := range 3 {
			if i := w*3 + k; i < len(comps) {
				word |= comps[i] << 2 << (10 * k)
			}
		}
		binary.LittleEndian.PutUint32(out[w*4:], word)
	}
	return out
}

func TestV210Unpack(t *testing.T) {
	// A width that is not a multiple of the 6-pixel v210 group ends in a
	// group carrying two (1280, every 720p flow) or four (1282) pixels and
	// padding; 2 and 4 are a tail with no complete group before it.
	for _, width := range []int{2, 4, 1280, 1282, 1920} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			const height = 2
			stride := (width + 47) / 48 * 128

			y := make([]byte, width)
			cb := make([]byte, width/2)
			cr := make([]byte, width/2)
			for i := range y {
				y[i] = byte(i * 7)
			}
			for i := range cb {
				cb[i] = byte(i*3 + 1)
				cr[i] = byte(i*5 + 2)
			}
			line := packV210Line(y, cb, cr, stride)
			src := append(append([]byte{}, line...), line...)

			u, err := NewV210Unpacker(width, height)
			require.NoError(t, err)

			yOut := make([]byte, width*height)
			cbOut := make([]byte, width/2)
			crOut := make([]byte, width/2)
			require.NoError(t, u.Unpack(src, stride, yOut, cbOut, crOut))

			require.Equal(t, append(append([]byte{}, y...), y...), yOut)
			require.Equal(t, cb, cbOut)
			require.Equal(t, cr, crOut)
		})
	}
}

func TestV210RejectsOddWidth(t *testing.T) {
	// 4:2:0 output has one chroma sample per two pixels.
	_, err := NewV210Unpacker(1279, 720)
	require.Error(t, err)
}
