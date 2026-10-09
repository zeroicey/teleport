package store

import (
	"math"
	"time"
)

// HoursFromNow converts an hours offset into an absolute epoch-millisecond
// timestamp, mirroring the previous implementation's `hoursFromNow` exactly:
//
//	hours > 0  -> now + round(hours * 3600000)
//	hours <= 0 -> 0, meaning "never expires"
//
// Rounding to whole milliseconds matters: the original used Math.round on the
// product, so a fractional hour count produced an integral timestamp.
func HoursFromNow(hours float64, defaultHours int) int64 {
	_ = defaultHours
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 {
		return 0
	}
	return time.Now().UnixMilli() + int64(math.Round(hours*3_600_000))
}
