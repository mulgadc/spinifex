package handlers_elbv2

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/lbagent"
)

// LBAgentHeartbeatInput is sent by the LB agent on each heartbeat tick.
// The agent includes its health report (HAProxy backend server statuses) so
// the daemon can update target health without polling.
type LBAgentHeartbeatInput struct {
	LBID    *string                `locationName:"LBID" type:"string"`
	Servers []*LBAgentServerStatus `locationName:"Servers" type:"list"`
}

// LBAgentServerStatus represents a single backend server's health status
// as reported by the LB agent. Maps to lbagent.ServerStatus for processing.
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

// GetLBConfigOutput returns the pre-computed data-plane config and its hash,
// plus any TLS certificate files the config references. Engine selects the
// data plane the agent runs: "haproxy" (ALB) or "nginx" (NLB). HealthTargets
// is populated for NLBs only: nginx `stream` has no active upstream probing,
// so the agent probes these and reports the results via heartbeat.
type GetLBConfigOutput struct {
	ConfigText    *string         `type:"string"`
	ConfigHash    *string         `type:"string"`
	CertFiles     []*CertFile     `locationName:"CertFiles" type:"list"`
	Engine        *string         `type:"string"`
	HealthTargets []*HealthTarget `locationName:"HealthTargets" type:"list"`
}

// HealthTarget is one backend the nginx agent must actively health-check.
// ServerName is daemon-computed (sanitizeName("srv", target.Id)) so it matches
// the name the health checker keys on — the agent echoes it back verbatim and
// needs no ELBv2 naming logic. Address is the ip:port to probe; Protocol is
// the target group's health-check protocol (TCP/HTTP/HTTPS); Path is the
// HTTP(S) health-check path.
type HealthTarget struct {
	ServerName *string `locationName:"ServerName" type:"string"`
	Address    *string `locationName:"Address" type:"string"`
	Protocol   *string `locationName:"Protocol" type:"string"`
	Path       *string `locationName:"Path" type:"string"`
}

// CertFile is one TLS certificate PEM delivered to the LB agent. Path is the
// absolute destination (under lbagent.CertDir) the agent writes 0600 before
// reload; PEM is the combined cert+chain+key material.
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
