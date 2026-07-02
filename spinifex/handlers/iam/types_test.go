package handlers_iam

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStringOrArr_UnmarshalSingleString(t *testing.T) {
	var s StringOrArr
	err := json.Unmarshal([]byte(`"s3:GetObject"`), &s)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{"s3:GetObject"}, s)
}

func TestStringOrArr_UnmarshalArray(t *testing.T) {
	var s StringOrArr
	err := json.Unmarshal([]byte(`["s3:Get*","s3:List*"]`), &s)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{"s3:Get*", "s3:List*"}, s)
}

func TestStringOrArr_UnmarshalNull(t *testing.T) {
	// A JSON null yields a nil slice (an inert field), not [""].
	var s StringOrArr
	err := json.Unmarshal([]byte(`null`), &s)
	require.NoError(t, err)
	assert.Nil(t, s)
}

func TestStringOrArr_UnmarshalEmptyArray(t *testing.T) {
	var s StringOrArr
	err := json.Unmarshal([]byte(`[]`), &s)
	require.NoError(t, err)
	assert.Equal(t, StringOrArr{}, s)
}

func TestStringOrArr_MarshalSingleElement(t *testing.T) {
	s := StringOrArr{"ec2:*"}
	data, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Equal(t, `"ec2:*"`, string(data))
}

func TestStringOrArr_MarshalMultipleElements(t *testing.T) {
	s := StringOrArr{"s3:Get*", "s3:Put*"}
	data, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Equal(t, `["s3:Get*","s3:Put*"]`, string(data))
}

func TestStringOrArr_RoundTrip_Single(t *testing.T) {
	original := StringOrArr{"iam:CreateUser"}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded StringOrArr
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}

func TestStringOrArr_RoundTrip_Array(t *testing.T) {
	original := StringOrArr{"ec2:Describe*", "ec2:Run*", "ec2:Stop*"}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded StringOrArr
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}
