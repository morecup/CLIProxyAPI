package features

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// RefreshCadence mirrors the native SDK's feature-controlled periodic refresh:
// null is an unjittered six hours; a present value enables bounded jitter and
// the failed-fetch extra attempt, even when that value is invalid.
func RefreshCadence(raw json.RawMessage, random float64) (time.Duration, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 6 * time.Hour, false
	}
	minutes, valid := number(raw)
	var value string
	if json.Unmarshal(raw, &value) == nil {
		value = strings.Trim(value, "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
		if strings.Contains(value, "_") {
			valid = false
		} else {
			base := 0
			if len(value) > 2 {
				switch value[:2] {
				case "0x", "0X":
					base = 16
				case "0b", "0B":
					base = 2
				case "0o", "0O":
					base = 8
				}
			}
			if base != 0 {
				n, err := strconv.ParseUint(value[2:], base, 64)
				minutes, valid = float64(n), err == nil
			} else {
				var err error
				minutes, err = strconv.ParseFloat(value, 64)
				valid = err == nil
			}
		}
	}
	if !valid || math.IsNaN(minutes) || math.IsInf(minutes, 0) || minutes <= 0 {
		minutes = 360
	} else {
		minutes = math.Min(math.Max(minutes, 5), 360)
	}
	ms := math.Min(minutes*(0.9+random*0.2), 360) * 60 * 1000
	rounded := math.Floor(ms)
	if ms-rounded >= 0.5 {
		rounded++
	}
	return time.Duration(rounded) * time.Millisecond, true
}
