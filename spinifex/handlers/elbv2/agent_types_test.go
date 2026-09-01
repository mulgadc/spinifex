package handlers_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/lbagent"
	"github.com/stretchr/testify/assert"
)

// TestToHealthReport verifies the LBAgentHeartbeatInput.toHealthReport conversion,
// in particular that aws.StringValue handles nil pointers without panicking.
func TestToHealthReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      *LBAgentHeartbeatInput
		wantLB  string
		wantSrv []lbagent.ServerStatus
	}{
		{
			name: "all fields populated",
			in: &LBAgentHeartbeatInput{
				LBID: aws.String("lb-abc123"),
				Servers: []*LBAgentServerStatus{
					{Backend: aws.String("tg-1"), Server: aws.String("10.0.0.1:80"), Status: aws.String("UP")},
					{Backend: aws.String("tg-2"), Server: aws.String("10.0.0.2:80"), Status: aws.String("DOWN")},
				},
			},
			wantLB: "lb-abc123",
			wantSrv: []lbagent.ServerStatus{
				{Backend: "tg-1", Server: "10.0.0.1:80", Status: "UP"},
				{Backend: "tg-2", Server: "10.0.0.2:80", Status: "DOWN"},
			},
		},
		{
			name: "nil LBID",
			in: &LBAgentHeartbeatInput{
				LBID: nil,
				Servers: []*LBAgentServerStatus{
					{Backend: aws.String("tg-1"), Server: aws.String("10.0.0.1:80"), Status: aws.String("UP")},
				},
			},
			wantLB: "",
			wantSrv: []lbagent.ServerStatus{
				{Backend: "tg-1", Server: "10.0.0.1:80", Status: "UP"},
			},
		},
		{
			name:    "nil servers",
			in:      &LBAgentHeartbeatInput{LBID: aws.String("lb-abc123"), Servers: nil},
			wantLB:  "lb-abc123",
			wantSrv: nil,
		},
		{
			name:    "empty input",
			in:      &LBAgentHeartbeatInput{},
			wantLB:  "",
			wantSrv: nil,
		},
		{
			name: "nil server fields",
			in: &LBAgentHeartbeatInput{
				LBID: aws.String("lb-abc123"),
				Servers: []*LBAgentServerStatus{
					{Backend: nil, Server: nil, Status: nil},
					{Backend: aws.String("tg-1"), Server: nil, Status: aws.String("MAINT")},
					{Backend: nil, Server: aws.String("10.0.0.3:80"), Status: nil},
				},
			},
			wantLB: "lb-abc123",
			wantSrv: []lbagent.ServerStatus{
				{Backend: "", Server: "", Status: ""},
				{Backend: "tg-1", Server: "", Status: "MAINT"},
				{Backend: "", Server: "10.0.0.3:80", Status: ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := tt.in.toHealthReport()
			assert.Equal(t, tt.wantLB, report.LBID)
			if tt.wantSrv == nil {
				assert.Empty(t, report.Servers)
			} else {
				assert.Equal(t, tt.wantSrv, report.Servers)
			}
		})
	}
}
