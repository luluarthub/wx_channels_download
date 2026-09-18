package api

import (
	"strconv"
	"testing"
)

func TestSegmentSpeedLimit(t *testing.T) {
	maxInput := int(^uint(0) >> 1)
	maxExpected := int64(^uint64(0) >> 1)
	if strconv.IntSize == 32 {
		maxExpected = int64(maxInput) * 1024 * 1024
	}
	for _, tc := range []struct {
		name  string
		input int
		want  int64
	}{
		{"disabled", 0, 0},
		{"negative", -1, 0},
		{"existing local setting", 4, 4 * 1024 * 1024},
		{"maximum integer", maxInput, maxExpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := segment_speed_limit(tc.input); got != tc.want {
				t.Fatalf("got %d bytes/s, want %d", got, tc.want)
			}
		})
	}
}
