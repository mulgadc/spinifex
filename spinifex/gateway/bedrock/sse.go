package gateway_bedrock

import (
	"bufio"
	"io"
	"strings"
)

// sseMaxLineBuf is the maximum single-line size the scanner accepts. Data
// lines carry one JSON chunk each; 1MiB comfortably covers any single
// streamed delta.
const sseMaxLineBuf = 1024 * 1024

// sseEvent is one parsed text/event-stream record: the optional "event:"
// type (empty when absent, per the SSE spec's implicit "message" default)
// and its "data:" lines joined with "\n".
type sseEvent struct {
	Event string
	Data  string
}

// sseScanner reads text/event-stream framing shared by vLLM/OpenAI (bare
// "data:" lines) and Anthropic ("event:"+"data:" pairs): blank-line
// terminated records, skipping comment lines (leading ':') and blank
// keepalive lines between records.
type sseScanner struct {
	sc *bufio.Scanner
}

// newSSEScanner wraps r for SSE line scanning.
func newSSEScanner(r io.Reader) *sseScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxLineBuf)
	return &sseScanner{sc: sc}
}

// Next returns the next SSE record, or (zero, false, err) at EOF/read error.
// err is io.EOF at a clean stream end.
func (s *sseScanner) Next() (sseEvent, bool, error) {
	var event string
	var dataLines []string
	sawAny := false

	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if sawAny {
				return sseEvent{Event: event, Data: strings.Join(dataLines, "\n")}, true, nil
			}
			continue // keepalive blank line between records
		}
		switch {
		case strings.HasPrefix(line, ":"):
			// comment/keepalive, ignore — does not start a record
		case strings.HasPrefix(line, "event:"):
			sawAny = true
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			sawAny = true
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}

	if err := s.sc.Err(); err != nil {
		return sseEvent{}, false, err
	}
	if sawAny {
		return sseEvent{Event: event, Data: strings.Join(dataLines, "\n")}, true, nil
	}
	return sseEvent{}, false, io.EOF
}
