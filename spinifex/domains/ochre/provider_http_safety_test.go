package ochre

//test:in-package: drives the unexported provider clients and adapters
// against local source and destination servers.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	safetyLlamaModel     = "meta.llama3-2-1b-instruct-v1:0"
	safetyAnthropicModel = "anthropic.claude-3-5-sonnet-20240620-v1:0"
)

func safetyConverseMessages() []*bedrockruntime.Message {
	return []*bedrockruntime.Message{{
		Role:    aws.String(bedrockruntime.ConversationRoleUser),
		Content: []*bedrockruntime.ContentBlock{{Text: aws.String(reproPromptMarker)}},
	}}
}

// providerCalls issues one request per production provider path against
// baseURL and returns its error.
func providerCalls() map[string]func(baseURL string) error {
	ctx := context.Background()
	static := func(u string) EndpointResolver {
		return NewStaticEndpointResolver(map[string]string{safetyLlamaModel: u})
	}
	anthropic := func(u string) *anthropicBoundProvider {
		p := newAnthropicProvider(reproAPIKey).(*anthropicBoundProvider)
		p.inner.baseURL = u
		return p
	}
	anthropicInvoke := func(u string) *boundAnthropicInvokeAdapter {
		a := newAnthropicInvokeAdapter(reproAPIKey).(*boundAnthropicInvokeAdapter)
		a.inner.baseURL = u
		return a
	}
	invokeBody := []byte(`{"prompt":"` + reproPromptMarker + `"}`)
	anthropicBody := []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`)
	return map[string]func(string) error{
		"vllm converse": func(u string) error {
			_, err := newVLLMProvider(static(u)).Converse(ctx, safetyLlamaModel, &bedrockruntime.ConverseInput{Messages: safetyConverseMessages()})
			return err
		},
		"vllm converse-stream": func(u string) error {
			_, err := newVLLMProvider(static(u)).ConverseStream(ctx, safetyLlamaModel, &bedrockruntime.ConverseStreamInput{Messages: safetyConverseMessages()})
			return err
		},
		"llama invoke": func(u string) error {
			_, _, err := newLlamaInvokeAdapter(static(u)).InvokeModel(ctx, safetyLlamaModel, invokeBody)
			return err
		},
		"llama invoke-stream": func(u string) error {
			_, err := newLlamaInvokeAdapter(static(u)).InvokeModelWithResponseStream(ctx, safetyLlamaModel, invokeBody)
			return err
		},
		"anthropic converse": func(u string) error {
			_, err := anthropic(u).Converse(ctx, safetyAnthropicModel, &bedrockruntime.ConverseInput{Messages: safetyConverseMessages()})
			return err
		},
		"anthropic converse-stream": func(u string) error {
			_, err := anthropic(u).ConverseStream(ctx, safetyAnthropicModel, &bedrockruntime.ConverseStreamInput{Messages: safetyConverseMessages()})
			return err
		},
		"anthropic invoke": func(u string) error {
			_, _, err := anthropicInvoke(u).InvokeModel(ctx, safetyAnthropicModel, anthropicBody)
			return err
		},
		"anthropic invoke-stream": func(u string) error {
			_, err := anthropicInvoke(u).InvokeModelWithResponseStream(ctx, safetyAnthropicModel, anthropicBody)
			return err
		},
	}
}

func TestProviderClients_RedirectIsModelErrorAndNotFollowed(t *testing.T) {
	statuses := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	for name, call := range providerCalls() {
		for _, status := range statuses {
			t.Run(name+"/"+strconv.Itoa(status), func(t *testing.T) {
				source, sourceHits, hits := redirectPair(t, status, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				})

				err := call(source.URL)

				require.EqualValues(t, 1, sourceHits.Load())
				assert.Zero(t, hits.Load(), "redirect target received a request")
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			})
		}
	}
}

func TestProviderClients_MalformedLocationIsServiceUnavailable(t *testing.T) {
	for name, call := range providerCalls() {
		t.Run(name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "http://[::1")
				w.WriteHeader(http.StatusFound)
			}))
			defer source.Close()

			err := call(source.URL)

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
		})
	}
}

// truncatedStatus answers status with a Content-Length the body never meets,
// so reading the body fails.
func truncatedStatus(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("cut"))
	}
}

func TestProviderClients_TruncatedErrorBodyMapsByStatus(t *testing.T) {
	for name, call := range providerCalls() {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(truncatedStatus(http.StatusTooManyRequests))
			defer upstream.Close()

			err := call(upstream.URL)

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
		})
	}
}

func TestTEIInvoke_StatusMapsBeforeBodyRead(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusRequestEntityTooLarge, awserrors.ErrorValidationException},
		{http.StatusUnprocessableEntity, awserrors.ErrorValidationException},
		{http.StatusTemporaryRedirect, awserrors.ErrorModelErrorException},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			upstream := httptest.NewServer(truncatedStatus(c.status))
			defer upstream.Close()
			a := newTEIEmbeddingInvokeAdapter(NewStaticEndpointResolver(map[string]string{nomicModel: upstream.URL}))

			_, _, err := a.InvokeModel(context.Background(), nomicModel, embeddingBody("a"))

			require.Error(t, err)
			assert.Equal(t, c.want, err.Error())
		})
	}
}

func TestTEIInvoke_RedirectIsNotFollowed(t *testing.T) {
	source, sourceHits, hits := redirectPair(t, http.StatusTemporaryRedirect, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	a := newTEIEmbeddingInvokeAdapter(NewStaticEndpointResolver(map[string]string{nomicModel: source.URL}))

	_, _, err := a.InvokeModel(context.Background(), nomicModel, embeddingBody("a"))

	require.EqualValues(t, 1, sourceHits.Load())
	assert.Zero(t, hits.Load())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}
