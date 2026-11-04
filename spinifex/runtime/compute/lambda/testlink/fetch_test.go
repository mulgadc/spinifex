//go:build lambdatestlink

package testlink

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// A redirect whose Location cannot be parsed makes Go's client quote the
// Location into its error; a credential carried there must not survive.
func TestFetch_MalformedRedirectDropsCredential(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Location", "%zz?X-Amz-Credential=AKIASYNTHETICTESTKEY&X-Amz-Signature=deadbeef")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{HTTP: srv.Client()}
	_, err := d.fetch(t.Context(), Request{CodeURL: srv.URL + "/code", CodeSize: 1, CodeSHA256: "00"})
	require.Error(t, err)
	require.Equal(t, int32(1), served.Load(), "the redirect never reached the client")
	require.Equal(t, "code download: request failed", err.Error())
}
