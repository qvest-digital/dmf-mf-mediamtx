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
	var comps []uint32
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
	// 1280 is not a multiple of the 6-pixel v210 group: its last group
	// carries two pixels and padding, and every 720p flow depends on it.
	for _, width := range []int{1280, 1920} {
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
