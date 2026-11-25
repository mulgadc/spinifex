package awsgw

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseBedrockEndpoints(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			assert.Equal(t, tc.want, parseBedrockEndpoints(tc.raw))
		})
	}
}

// A typo in an optional tuning knob must not stop the gateway from serving,
// so an unusable value falls back to the fail-fast default.
func TestParseColdStartWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset keeps the fail-fast default", "", 0},
		{"a duration is honoured", "45s", 45 * time.Second},
		{"whitespace is trimmed", "  2m  ", 2 * time.Minute},
		{"a malformed value is ignored", "45 seconds", 0},
		{"a negative value is ignored", "-10s", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, parseColdStartWait(tt.raw))
		})
	}
}

func TestParseBedrockEndpoints_MalformedEntryLogOmitsEntry(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	got := parseBedrockEndpoints("http://gw-user-4471:gw-pass-9035@vllm.internal:8000/gw-path-2264,m=http://vllm2:8000")

	assert.Equal(t, map[string]string{"m": "http://vllm2:8000"}, got)
	assert.Contains(t, logs.String(), "position=1")
	for _, marker := range []string{"gw-user-4471", "gw-pass-9035", "gw-path-2264", "vllm.internal"} {
		assert.NotContains(t, logs.String(), marker)
	}
}
