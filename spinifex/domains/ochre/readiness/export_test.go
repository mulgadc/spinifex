package readiness

// SetBeforeWriteHook runs f with the operation name just before each record
// write is sent, so a test can pause a writer between its read and its write.
func SetBeforeWriteHook(c *Control, f func(op string)) { c.beforeWrite = f }
