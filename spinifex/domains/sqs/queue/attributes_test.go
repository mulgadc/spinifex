package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The range cases are the scen_p create and set boundaries; each limit is
// accepted and one past it rejected (ADR-0012 rows A5, A6, A8, A9, A10).
func TestValidateIntegerBoundaries(t *testing.T) {
	cases := []struct {
		name, value string
		ok          bool
	}{
		{AttrVisibilityTimeout, "0", true},
		{AttrVisibilityTimeout, "43200", true},
		{AttrVisibilityTimeout, "43201", false},
		{AttrMessageRetentionPeriod, "59", false},
		{AttrMessageRetentionPeriod, "60", true},
		{AttrMessageRetentionPeriod, "1209600", true},
		{AttrMessageRetentionPeriod, "1209601", false},
		{AttrMaximumMessageSize, "1023", false},
		{AttrMaximumMessageSize, "1024", true},
		{AttrMaximumMessageSize, "1048576", true},
		{AttrMaximumMessageSize, "1048577", false},
		{AttrDelaySeconds, "900", true},
		{AttrDelaySeconds, "901", false},
		{AttrReceiveMessageWaitTimeSeconds, "20", true},
		{AttrReceiveMessageWaitTimeSeconds, "21", false},
	}
	for _, tc := range cases {
		for _, validate := range []func(map[string]string) (Attributes, error){ValidateCreate, ValidateSet} {
			got, err := validate(map[string]string{tc.name: tc.value})
			if tc.ok {
				require.NoError(t, err, "%s=%s", tc.name, tc.value)
				assert.Equal(t, tc.value, got[tc.name])
				continue
			}
			assert.ErrorIs(t, err, ErrInvalidAttributeValue, "%s=%s", tc.name, tc.value)
		}
	}
}

func TestValidateUnknownNameIsInvalidAttributeName(t *testing.T) {
	for _, name := range []string{"Bogus", "visibilitytimeout", "QueueArn", "ApproximateNumberOfMessages", "All"} {
		_, err := ValidateSet(map[string]string{name: "1"})
		assert.ErrorIs(t, err, ErrInvalidAttributeName, name)
	}
}

// Attributes AWS accepts but this profile does not serve are refused, never
// silently dropped, so a client cannot believe it has a FIFO, encrypted or
// redriven queue.
func TestValidateRejectsUnsupportedAttributes(t *testing.T) {
	cases := map[string]string{
		AttrFifoQueue:                  "true",
		AttrSqsManagedSseEnabled:       "true",
		"ContentBasedDeduplication":    "false",
		"DeduplicationScope":           "queue",
		"FifoThroughputLimit":          "perQueue",
		"KmsMasterKeyId":               "alias/aws/sqs",
		"KmsDataKeyReusePeriodSeconds": "300",
		"Policy":                       "{}",
		"RedrivePolicy":                "{}",
		"RedriveAllowPolicy":           "{}",
	}
	for name, value := range cases {
		_, err := ValidateCreate(map[string]string{name: value})
		assert.ErrorIs(t, err, ErrUnsupportedAttribute, "%s=%s", name, value)
	}
}

func TestFifoQueueFalseAcceptedOnlyAtCreate(t *testing.T) {
	got, err := ValidateCreate(map[string]string{AttrFifoQueue: "false"})
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = ValidateSet(map[string]string{AttrFifoQueue: "false"})
	assert.ErrorIs(t, err, ErrInvalidAttributeName)

	_, err = ValidateCreate(map[string]string{AttrFifoQueue: "yes"})
	assert.ErrorIs(t, err, ErrInvalidAttributeValue)
}

func TestDefaults(t *testing.T) {
	assert.Equal(t, Attributes{
		AttrVisibilityTimeout:             "30",
		AttrMessageRetentionPeriod:        "345600",
		AttrMaximumMessageSize:            "1048576",
		AttrDelaySeconds:                  "0",
		AttrReceiveMessageWaitTimeSeconds: "0",
		AttrSqsManagedSseEnabled:          "false",
	}, Defaults())
}
