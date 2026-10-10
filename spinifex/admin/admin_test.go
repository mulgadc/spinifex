package admin

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- Key generation ---

func TestGenerateAWSAccessKey_Format(t *testing.T) {
	key, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	assert.Len(t, key, 20)
	assert.True(t, strings.HasPrefix(key, "AKIA"))
	for _, c := range key[4:] {
		assert.True(t, (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'),
			"unexpected character %c in access key suffix", c)
	}
}

func TestGenerateAWSAccessKey_Uniqueness(t *testing.T) {
	k1, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	k2, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	assert.NotEqual(t, k1, k2)
}

func TestGenerateAWSSecretKey_Format(t *testing.T) {
	key, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.Len(t, key, 40)
	_, err = base64.StdEncoding.DecodeString(key)
	assert.NoError(t, err, "secret key should be valid base64")
}

func TestGenerateAWSSecretKey_Uniqueness(t *testing.T) {
	k1, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	k2, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.NotEqual(t, k1, k2)
}

func TestDefaultAccountID(t *testing.T) {
	id := DefaultAccountID()
	assert.Equal(t, "000000000001", id)
	assert.Len(t, id, 12)
}
