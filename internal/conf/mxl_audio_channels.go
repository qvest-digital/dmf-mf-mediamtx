package conf

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseAudioChannels reads the mxlAudioChannels path option: one or two
// 1-based channel numbers, comma separated.
//
// Empty selects the flow's first pair, which is what a caller that does not
// care should get. Two channels are the most that can be selected because RTP
// Opus carries no more, so a wider flow is heard a pair at a time and the
// option is how the rest are reached.
func ParseAudioChannels(v string) ([]int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}

	fields := strings.Split(v, ",")
	if len(fields) > 2 {
		return nil, fmt.Errorf("at most two channels, got %d", len(fields))
	}

	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("%q is not a channel number", f)
		}
		// 1-based, matching how every other tool in this chain numbers
		// audio channels and how an operator reads them off a router.
		if n < 1 {
			return nil, fmt.Errorf("channels are 1-based, got %d", n)
		}
		out = append(out, n)
	}
	return out, nil
}
