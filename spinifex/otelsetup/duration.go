package otelsetup

import "time"

// DurationSuffix is the key suffix every logged duration carries. slog logs a
// bare time.Duration as an int64 count of nanoseconds and names the unit
// nowhere, so a reader cannot tell one duration field from another without the
// call site. The unit lives in the key instead.
const DurationSuffix = "_ms"

// Millis converts d for logging under a key ending in DurationSuffix, e.g.
// slog.Info("done", "elapsed_ms", otelsetup.Millis(time.Since(start))). Never
// round the argument first: the value is already whole milliseconds, and
// rounding to a second logs 0 for everything faster than that.
func Millis(d time.Duration) int64 { return d.Milliseconds() }
