// Package queue owns SQS queue identity, attributes and records
// (ADR-0012:S1, S3).
package queue

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
)

// Attribute names a queue record can carry in this profile.
const (
	AttrVisibilityTimeout             = "VisibilityTimeout"
	AttrMessageRetentionPeriod        = "MessageRetentionPeriod"
	AttrMaximumMessageSize            = "MaximumMessageSize"
	AttrDelaySeconds                  = "DelaySeconds"
	AttrReceiveMessageWaitTimeSeconds = "ReceiveMessageWaitTimeSeconds"
	AttrSqsManagedSseEnabled          = "SqsManagedSseEnabled"
	AttrFifoQueue                     = "FifoQueue"
)

// Errors a request can fail validation with. The API adapter maps each to
// its AWS error code; the codes for ErrUnsupportedAttribute and
// ErrInvalidQueueName are provisional until characterised against AWS.
var (
	ErrInvalidAttributeName  = errors.New("invalid attribute name")
	ErrInvalidAttributeValue = errors.New("invalid attribute value")
	ErrUnsupportedAttribute  = errors.New("attribute not supported")
	ErrInvalidQueueName      = errors.New("invalid queue name")
)

type intRange struct {
	min, max, def int
}

// intAttributes are the settable integer attributes with AWS ranges and
// defaults (ADR-0012 rows A5, A6, A8, A9, A10).
var intAttributes = map[string]intRange{
	AttrVisibilityTimeout:             {0, 43200, 30},
	AttrMessageRetentionPeriod:        {60, 1209600, 345600},
	AttrMaximumMessageSize:            {1024, 1048576, 1048576},
	AttrDelaySeconds:                  {0, 900, 0},
	AttrReceiveMessageWaitTimeSeconds: {0, 20, 0},
}

// unsupported are attributes AWS accepts that this profile rejects
// explicitly rather than ignoring.
var unsupported = map[string]bool{
	"ContentBasedDeduplication":    true,
	"DeduplicationScope":           true,
	"FifoThroughputLimit":          true,
	"KmsMasterKeyId":               true,
	"KmsDataKeyReusePeriodSeconds": true,
	"Policy":                       true,
	"RedrivePolicy":                true,
	"RedriveAllowPolicy":           true,
}

// Attributes is a queue's attribute set keyed by AWS attribute name, with
// canonical values. Names this release does not know are kept as written.
type Attributes map[string]string

// Defaults is the effective attribute set of a queue created with none.
func Defaults() Attributes {
	a := Attributes{AttrSqsManagedSseEnabled: "false"}
	for name, r := range intAttributes {
		a[name] = strconv.Itoa(r.def)
	}
	return a
}

// ValidateCreate checks CreateQueue attributes and returns them in canonical
// form. FifoQueue=false is accepted and dropped: every queue is standard.
func ValidateCreate(in map[string]string) (Attributes, error) {
	return validate(in, true)
}

// ValidateSet checks SetQueueAttributes attributes and returns them in
// canonical form; FifoQueue is fixed at creation and rejected here.
func ValidateSet(in map[string]string) (Attributes, error) {
	return validate(in, false)
}

func validate(in map[string]string, create bool) (Attributes, error) {
	out := make(Attributes, len(in))
	for _, name := range sortedKeys(in) {
		value := in[name]
		if r, ok := intAttributes[name]; ok {
			n, err := parseInt(value)
			if err != nil || n < r.min || n > r.max {
				return nil, fmt.Errorf("%w: %s must be an integer from %d to %d", ErrInvalidAttributeValue, name, r.min, r.max)
			}
			out[name] = strconv.Itoa(n)
			continue
		}
		switch {
		case name == AttrSqsManagedSseEnabled:
			b, err := parseBool(name, value)
			if err != nil {
				return nil, err
			}
			if b {
				return nil, fmt.Errorf("%w: %s=true; server-side encryption is not available", ErrUnsupportedAttribute, name)
			}
			out[name] = "false"
		case name == AttrFifoQueue && create:
			b, err := parseBool(name, value)
			if err != nil {
				return nil, err
			}
			if b {
				return nil, fmt.Errorf("%w: FIFO queues are not available", ErrUnsupportedAttribute)
			}
		case name == AttrFifoQueue:
			return nil, fmt.Errorf("%w: %s can only be set when the queue is created", ErrInvalidAttributeName, name)
		case unsupported[name]:
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedAttribute, name)
		default:
			return nil, fmt.Errorf("%w: %s", ErrInvalidAttributeName, name)
		}
	}
	return out, nil
}

// WithDefaults returns a copy of a with every unset attribute at its default.
func (a Attributes) WithDefaults() Attributes {
	out := Defaults()
	maps.Copy(out, a)
	return out
}

// Matches reports whether every supplied attribute equals a's value for it.
// Both must be canonical. Provisional: which attributes a repeat must match
// is not yet characterised against AWS.
func (a Attributes) Matches(supplied Attributes) bool {
	for name, v := range supplied {
		if a[name] != v {
			return false
		}
	}
	return true
}

// parseInt accepts decimal digits only, so "+30", "-0" and " 30" fail.
func parseInt(s string) (int, error) {
	if s == "" || len(s) > 10 {
		return 0, strconv.ErrSyntax
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.Atoi(s)
}

// parseBool is case-insensitive; provisional until AWS's casing rule is
// characterised.
func parseBool(name, s string) (bool, error) {
	switch {
	case strings.EqualFold(s, "true"):
		return true, nil
	case strings.EqualFold(s, "false"):
		return false, nil
	}
	return false, fmt.Errorf("%w: %s must be true or false", ErrInvalidAttributeValue, name)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
