package utils

import "math"

// SafeInt64ToUint64 converts int64 to uint64, returning 0 if negative.
func SafeInt64ToUint64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SafeIntToUint8 converts int to uint8, clamping to [0, 255].
func SafeIntToUint8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(v)
}

// SafeIntToUint64 converts int to uint64, returning 0 if negative.
func SafeIntToUint64(v int) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SafeUint64ToInt64 converts uint64 to int64, capping at math.MaxInt64.
func SafeUint64ToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
