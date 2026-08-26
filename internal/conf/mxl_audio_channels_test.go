package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAudioChannels(t *testing.T) {
	for _, ca := range []struct {
		name string
		in   string
		want []int
	}{
		// Empty is the common case: the caller does not care which pair, so
		// the source takes the flow's first.
		{"empty", "", nil},
		{"a pair", "1,2", []int{1, 2}},
		{"a later pair", "11,12", []int{11, 12}},
		{"a single channel", "5", []int{5}},
		{"spaces around the separator", " 3 , 4 ", []int{3, 4}},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got, err := ParseAudioChannels(ca.in)
			require.NoError(t, err)
			require.Equal(t, ca.want, got)
		})
	}
}

func TestParseAudioChannelsRejects(t *testing.T) {
	for _, ca := range []struct {
		name string
		in   string
	}{
		// RTP Opus carries two channels, so a third has nowhere to go and
		// accepting it would silently drop one.
		{"more than a pair", "1,2,3"},
		{"not a number", "left,right"},
		// The numbering is 1-based everywhere an operator meets it, so a 0
		// is a caller that thinks otherwise rather than a valid channel.
		{"zero", "0"},
		{"negative", "-1"},
		{"empty field", "1,"},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, err := ParseAudioChannels(ca.in)
			require.Error(t, err)
		})
	}
}
