package readiness

// SetBeforeWriteHook runs f with the operation name just before each record
// write is sent, so a test can pause a writer between its read and its write.
func SetBeforeWriteHook(c *Control, f func(op string)) { c.beforeWrite = f }

// SetInitStepHook runs f at each named point between initialization steps, so
// a test can pause or disturb an initializer there.
func SetInitStepHook(in *Initializer, f func(step string)) { in.hook = f }
