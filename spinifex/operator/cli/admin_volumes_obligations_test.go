package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	ebsobligation "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/obligation"
	"github.com/stretchr/testify/assert"
)

func TestPrintUnpublishObligations_OldestFirstWithFailures(t *testing.T) {
	now := time.Unix(1_700_010_000, 0)
	var out bytes.Buffer
	printUnpublishObligations(&out, []ebsobligation.UnpublishObligation{
		{VolumeID: "vol-newer", NodeID: "node-b", InstanceID: "i-2", RecordedAt: now.Add(-90 * time.Second)},
		{VolumeID: "vol-older", NodeID: "node-a", InstanceID: "i-1", RecordedAt: now.Add(-2 * time.Hour),
			Attempts: 3, Error: "seal refused"},
	}, now)

	text := out.String()
	assert.Contains(t, text, "2 open unpublish obligation(s)")
	assert.Less(t, strings.Index(text, "vol-older"), strings.Index(text, "vol-newer"), "the longest-open obligation is listed first")
	assert.Contains(t, text, "node:     node-a")
	assert.Contains(t, text, "instance: i-1")
	assert.Contains(t, text, "age:      2h0m0s")
	assert.Contains(t, text, "attempts: 3")
	assert.Contains(t, text, "error:    seal refused")
	assert.Equal(t, 1, strings.Count(text, "attempts:"), "an obligation that never failed shows no attempts")
}

func TestPrintUnpublishObligations_None(t *testing.T) {
	var out bytes.Buffer
	printUnpublishObligations(&out, nil, time.Now())
	assert.Contains(t, out.String(), "No open unpublish obligations")
}
