// Package gateway_ec2_idem holds the client-token plumbing shared by the EC2
// gateway: the KV bucket every EC2 action's token records live in, and the
// extraction of a token and parameter hash from an SDK input struct.
package gateway_ec2_idem

import (
	"reflect"
	"time"

	"github.com/mulgadc/spinifex/spinifex/idempotency"
)

const (
	// KVBucket is the JetStream KV bucket for EC2 ClientToken records. One
	// bucket for the whole EC2 surface; per-action stores namespace their keys.
	KVBucket = "spinifex-ec2-clienttokens"

	// TTL must outlast SDK retry windows; short enough that a crashed in-flight
	// record ages out and frees the token for a fresh attempt.
	TTL = 15 * time.Minute

	// tokenField is the ClientToken field on every EC2 input struct that
	// supports idempotency.
	tokenField = "ClientToken"
)

// TokenAndParams reads the ClientToken off an SDK input struct and hashes the
// request with the token cleared, so identical requests always hash alike. It
// reports false when the input carries no usable token, which means the caller
// asked for no idempotency and the request runs unwrapped.
func TokenAndParams(input any) (token, paramHash string, ok bool) {
	v := reflect.ValueOf(input)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", "", false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", "", false
	}
	field := v.FieldByName(tokenField)
	if !field.IsValid() || field.Kind() != reflect.Pointer || field.Type().Elem().Kind() != reflect.String {
		return "", "", false
	}
	if field.IsNil() || field.Elem().String() == "" {
		return "", "", false
	}

	// Hash a copy with the token cleared rather than the original: the caller's
	// input is dispatched afterwards and must reach the handler intact.
	clone := reflect.New(v.Type()).Elem()
	clone.Set(v)
	clone.FieldByName(tokenField).Set(reflect.Zero(field.Type()))
	return field.Elem().String(), idempotency.ParamHash(clone.Addr().Interface()), true
}
