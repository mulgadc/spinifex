package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// handleAccountCreated creates a default VPC for a newly created account.
func (d *Daemon) handleAccountCreated(msg *nats.Msg) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	var evt struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.ErrorContext(ctx, "Failed to unmarshal account creation event", "error", err)
		utils.MarkSpanError(span, err)
		return
	}
	if evt.AccountID == "" {
		slog.ErrorContext(ctx, "Account creation event has empty account ID")
		return
	}
	if _, err := d.vpcService.EnsureDefaultVPC(evt.AccountID); err != nil {
		slog.ErrorContext(ctx, "Failed to create default VPC for new account",
			"accountID", evt.AccountID, "error", err)
		utils.MarkSpanError(span, err)
		// Skip IGW setup — the VPC is missing or half-built. The next daemon
		// startup or handleAccountCreated event will retry.
		return
	}
	d.ensureDefaultVPCInfrastructureFor(ctx, evt.AccountID)
}

// ensureDefaultVPCInfrastructure attaches an IGW and a default 0.0.0.0/0 route
// to each well-known account's default VPC. The default SG is provisioned by
// EnsureDefaultVPC / CreateVpc itself, so this routine is IGW-only. Accounts
// in skipAccounts are not touched — used to avoid attaching infrastructure to
// a half-built VPC when EnsureDefaultVPC failed earlier in startup.
func (d *Daemon) ensureDefaultVPCInfrastructure(skipAccounts map[string]struct{}) {
	for _, accountID := range []string{utils.GlobalAccountID, admin.DefaultAccountID()} {
		if _, skip := skipAccounts[accountID]; skip {
			continue
		}
		d.ensureDefaultVPCInfrastructureFor(context.Background(), accountID)
	}
}

// ensureDefaultVPCInfrastructureFor attaches an IGW and 0.0.0.0/0 route to the
// given account's default VPC if not already present.
func (d *Daemon) ensureDefaultVPCInfrastructureFor(ctx context.Context, accountID string) {
	if d.igwService == nil || d.vpcService == nil {
		return
	}

	// Find the default VPC for this account
	descOut, err := d.vpcService.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "DescribeVpcs failed for default VPC infrastructure, retrying", "accountID", accountID, "err", err)
		time.Sleep(500 * time.Millisecond)
		descOut, err = d.vpcService.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "DescribeVpcs failed for default VPC infrastructure after retry",
				"accountID", accountID, "err", err)
			return
		}
	}
	var defaultVpcId string
	for _, vpc := range descOut.Vpcs {
		if vpc.IsDefault != nil && *vpc.IsDefault {
			defaultVpcId = *vpc.VpcId
			break
		}
	}
	if defaultVpcId == "" {
		return
	}

	// Check if IGW already attached
	igwOut, err := d.igwService.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInternetGateways failed for default VPC infrastructure, retrying",
			"accountID", accountID, "err", err)
		time.Sleep(500 * time.Millisecond)
		igwOut, err = d.igwService.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{}, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "DescribeInternetGateways failed for default VPC infrastructure after retry",
				"accountID", accountID, "err", err)
			return
		}
	}
	hasIGW := false
	for _, igw := range igwOut.InternetGateways {
		for _, att := range igw.Attachments {
			if att.VpcId != nil && *att.VpcId == defaultVpcId {
				hasIGW = true
				break
			}
		}
	}
	if !hasIGW {
		// Create and attach an IGW — use bootstrap ID if available
		var createOut *ec2.CreateInternetGatewayOutput
		var err error
		if accountID == admin.DefaultAccountID() && d.clusterConfig != nil && d.clusterConfig.Bootstrap.IgwId != "" {
			createOut, err = d.igwService.CreateInternetGatewayWithID(&ec2.CreateInternetGatewayInput{}, accountID, d.clusterConfig.Bootstrap.IgwId)
		} else {
			createOut, err = d.igwService.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{}, accountID)
		}
		if err != nil {
			slog.ErrorContext(ctx, "Failed to create default IGW", "accountID", accountID, "err", err)
			return
		}
		igwId := *createOut.InternetGateway.InternetGatewayId
		_, err = d.igwService.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
			InternetGatewayId: &igwId,
			VpcId:             &defaultVpcId,
		}, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to attach default IGW", "igwId", igwId, "vpcId", defaultVpcId, "err", err)
		} else {
			slog.InfoContext(ctx, "Attached default IGW to default VPC", "igwId", igwId, "vpcId", defaultVpcId, "accountID", accountID)
		}
	}

	// Add 0.0.0.0/0 → IGW route to the main route table (if not already present)
	if d.routeTableService != nil {
		d.ensureDefaultIGWRoute(ctx, accountID, defaultVpcId)
	}
}

// ensureDefaultIGWRoute adds 0.0.0.0/0 → igw-xxx to the main route table if not present.
func (d *Daemon) ensureDefaultIGWRoute(ctx context.Context, accountID, vpcID string) {
	// Find the main route table for this VPC
	vpcFilter := "vpc-id"
	mainFilter := "association.main"
	trueVal := "true"
	descOut, err := d.routeTableService.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: &vpcFilter, Values: []*string{&vpcID}},
			{Name: &mainFilter, Values: []*string{&trueVal}},
		},
	}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to query main route table for default IGW route", "vpcId", vpcID, "err", err)
		return
	}
	if len(descOut.RouteTables) == 0 {
		slog.DebugContext(ctx, "No main route table found for default IGW route", "vpcId", vpcID, "accountID", accountID)
		return
	}

	mainRtb := descOut.RouteTables[0]

	// Check if 0.0.0.0/0 route already exists
	for _, r := range mainRtb.Routes {
		if r.DestinationCidrBlock != nil && *r.DestinationCidrBlock == "0.0.0.0/0" {
			return // Already has a default route
		}
	}

	// Find the attached IGW for this VPC
	igwOut, err := d.igwService.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to query IGWs for default route", "vpcId", vpcID, "err", err)
		return
	}
	var igwID string
	for _, igw := range igwOut.InternetGateways {
		for _, att := range igw.Attachments {
			if att.VpcId != nil && *att.VpcId == vpcID {
				igwID = *igw.InternetGatewayId
				break
			}
		}
	}
	if igwID == "" {
		return // No IGW attached
	}

	// Add the default route
	dest := "0.0.0.0/0"
	_, err = d.routeTableService.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         mainRtb.RouteTableId,
		DestinationCidrBlock: &dest,
		GatewayId:            &igwID,
	}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to add default IGW route to main route table", "err", err)
	} else {
		slog.InfoContext(ctx, "Added default IGW route to main route table",
			"routeTableId", *mainRtb.RouteTableId, "igwId", igwID, "vpcId", vpcID)
	}
}
