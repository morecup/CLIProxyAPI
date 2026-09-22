package telemetry

import (
	"testing"
	"time"
)

func TestSDKResultDurationMatchesSourceRounding(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    int64
	}{
		{-time.Millisecond, 0},
		{0, 0},
		{499 * time.Microsecond, 0},
		{500 * time.Microsecond, 1},
		{1499 * time.Microsecond, 1},
		{1500 * time.Microsecond, 2},
	} {
		if got := sdkRoundedDurationMS(tc.elapsed); got != tc.want {
			t.Errorf("duration %v: got %d want %d", tc.elapsed, got, tc.want)
		}
	}
}
