//go:build lambdatestlink

package hostlink

// Protocol is the only ALPN protocol a test-link host and guest offer. It
// differs from the release protocol because the first frame carries the
// execution credential and function environment, which an older guest would ignore.
const Protocol = "spinifex-lambda-testlink/3"
