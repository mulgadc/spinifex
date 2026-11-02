package awsapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnonymousArgs_Classification covers the body-peek helper for anonymous action detection.
func TestAnonymousArgs_Classification(t *testing.T) {
	t.Run("anonymous action recognised, body restored", func(t *testing.T) {
		const body = "Action=AssumeRoleWithWebIdentity&WebIdentityToken=tok"
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		args, ok := anonymousArgs(req)
		require.True(t, ok)
		assert.Equal(t, "AssumeRoleWithWebIdentity", args["Action"])

		restored, _ := io.ReadAll(req.Body)
		assert.Equal(t, body, string(restored), "body must be restored for downstream read")
	})

	t.Run("non-anonymous action rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
		_, ok := anonymousArgs(req)
		assert.False(t, ok)
	})
}
