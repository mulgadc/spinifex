package handlers_elbv2

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/lbagent"
)

// LBAgentHeartbeatInput is sent by the LB agent on each heartbeat tick,
// including its health report so the daemon can update target health.
type LBAgentHeartbeatInput struct {
	LBID    *string                `locationName:"LBID" type:"string"`
	Servers []*LBAgentServerStatus `locationName:"Servers" type:"list"`
}

// LBAgentServerStatus represents a single backend server's health status as reported by the LB agent.
type LBAgentServerStatus struct {
	Backend *string `locationName:"Backend" type:"string"`
	Server  *string `locationName:"Server" type:"string"`
	Status  *string `locationName:"Status" type:"string"`
}

// LBAgentHeartbeatOutput is returned to the agent after processing a heartbeat.
type LBAgentHeartbeatOutput struct {
	Status     *string `type:"string"`
	ConfigHash *string `type:"string"`
}

// GetLBConfigInput is sent by the agent when it detects a config hash change.
type GetLBConfigInput struct {
	LBID *string `locationName:"LBID" type:"string"`
}

// GetLBConfigOutput returns the pre-computed data-plane config, its hash, and
// any TLS certificate files. Engine is "haproxy" (ALB) or "nginx" (NLB).
// HealthTargets is NLB-only: the agent actively probes these targets.
type GetLBConfigOutput struct {
	ConfigText    *string         `type:"string"`
	ConfigHash    *string         `type:"string"`
	CertFiles     []*CertFile     `locationName:"CertFiles" type:"list"`
	Engine        *string         `type:"string"`
	HealthTargets []*HealthTarget `locationName:"HealthTargets" type:"list"`
}

// HealthTarget is one backend the nginx agent actively health-checks.
// ServerName matches sanitizeName("srv", target.Id); the agent echoes it back verbatim.
type HealthTarget struct {
	ServerName *string `locationName:"ServerName" type:"string"`
	Address    *string `locationName:"Address" type:"string"`
	Protocol   *string `locationName:"Protocol" type:"string"`
	Path       *string `locationName:"Path" type:"string"`
}

// CertFile is one TLS certificate PEM delivered to the LB agent.
// Path is the absolute destination the agent writes 0600 before reload.
type CertFile struct {
	Path *string `locationName:"Path" type:"string"`
	PEM  *string `locationName:"PEM" type:"string"`
}

// toHealthReport converts the heartbeat input's server list to the lbagent
// HealthReport format used by handleHealthReportDirect.
func (in *LBAgentHeartbeatInput) toHealthReport() lbagent.HealthReport {
	report := lbagent.HealthReport{
		LBID: aws.StringValue(in.LBID),
	}
	for _, s := range in.Servers {
		report.Servers = append(report.Servers, lbagent.ServerStatus{
			Backend: aws.StringValue(s.Backend),
			Server:  aws.StringValue(s.Server),
			Status:  aws.StringValue(s.Status),
		})
	}
	return report
}
