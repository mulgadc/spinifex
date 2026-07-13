package awsgw

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBedrockEndpoints(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"empty input returns nil", "", nil},
		{"whitespace-only input returns nil", "   ", nil},
		{
			"single pair",
			"meta.llama3-70b-instruct-v1:0=http://vllm.internal:8000",
			map[string]string{"meta.llama3-70b-instruct-v1:0": "http://vllm.internal:8000"},
		},
		{
			"multiple pairs",
			"meta.llama3-70b-instruct-v1:0=http://vllm1:8000,mistral.mixtral-8x7b-v1:0=http://vllm2:8000",
			map[string]string{
				"meta.llama3-70b-instruct-v1:0": "http://vllm1:8000",
				"mistral.mixtral-8x7b-v1:0":     "http://vllm2:8000",
			},
		},
		{
			"whitespace around pairs and entries trimmed",
			"  meta.llama3-70b-instruct-v1:0 = http://vllm.internal:8000  ,  mistral.mixtral-8x7b-v1:0=http://vllm2:8000 ",
			map[string]string{
				"meta.llama3-70b-instruct-v1:0": "http://vllm.internal:8000",
				"mistral.mixtral-8x7b-v1:0":     "http://vllm2:8000",
			},
		},
		{
			"malformed entry with no equals sign is skipped",
			"no-equals,meta.llama3-70b-instruct-v1:0=http://vllm.internal:8000",
			map[string]string{"meta.llama3-70b-instruct-v1:0": "http://vllm.internal:8000"},
		},
		{
			"malformed entry with empty modelId is skipped",
			"=http://vllm.internal:8000,meta.llama3-70b-instruct-v1:0=http://vllm.internal:8000",
			map[string]string{"meta.llama3-70b-instruct-v1:0": "http://vllm.internal:8000"},
		},
		{
			"malformed entry with empty baseURL is skipped",
			"meta.llama3-70b-instruct-v1:0=,mistral.mixtral-8x7b-v1:0=http://vllm2:8000",
			map[string]string{"mistral.mixtral-8x7b-v1:0": "http://vllm2:8000"},
		},
		{"all entries malformed returns nil", "no-equals,=u,m=", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseBedrockEndpoints(tc.raw))
		})
	}
}
