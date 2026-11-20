package ochre

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redirectPair returns a source server that redirects every request to a
// separate destination server, and counters for the requests each received.
func redirectPair(t *testing.T, status int) (sourceURL string, sourceCalls, destinationCalls *atomic.Int32) {
	t.Helper()
	sourceCalls, destinationCalls = new(atomic.Int32), new(atomic.Int32)
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls.Add(1)
	}))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		http.Redirect(w, r, destination.URL+r.URL.Path, status)
	}))
	t.Cleanup(source.Close)
	return source.URL, sourceCalls, destinationCalls
}

func TestKBEmbedder_DoesNotFollowRedirects(t *testing.T) {
	const modelID = "nomic-embed-text-v1.5"
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, sourceCalls, destinationCalls := redirectPair(t, status)
			embedder := NewEmbedder(NewStaticEndpointResolver(map[string]string{modelID: sourceURL}))

			_, err := embedder.Embed(context.Background(), modelID, []string{"search_document: tenant text"})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			assert.Zero(t, destinationCalls.Load(), "the redirect target must receive nothing")
			assert.Equal(t, int32(1), sourceCalls.Load(), "a redirect must not be retried")
		})
	}
}

func TestKBReranker_DoesNotFollowRedirects(t *testing.T) {
	const modelID = "bge-reranker-base"
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, sourceCalls, destinationCalls := redirectPair(t, status)
			reranker := NewReranker(NewStaticEndpointResolver(map[string]string{modelID: sourceURL}), modelID)

			_, err := reranker.Rerank(context.Background(), "tenant query", []string{"tenant doc a", "tenant doc b"})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			assert.Zero(t, destinationCalls.Load(), "the redirect target must receive nothing")
			assert.Equal(t, int32(1), sourceCalls.Load(), "a redirect must not be retried")
		})
	}
}
