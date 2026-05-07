package daemon

import (
	"encoding/json"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

func (d *Daemon) handleEC2CreateVpc(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.CreateVpc)
}

func (d *Daemon) handleEC2DeleteVpc(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DeleteVpc)
}

func (d *Daemon) handleEC2DescribeVpcs(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeVpcs)
}

func (d *Daemon) handleEC2CreateSubnet(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.CreateSubnet)
}

func (d *Daemon) handleEC2DeleteSubnet(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DeleteSubnet)
}

func (d *Daemon) handleEC2DescribeSubnets(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeSubnets)
}

func (d *Daemon) handleEC2ModifySubnetAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.ModifySubnetAttribute)
}

func (d *Daemon) handleEC2ModifyVpcAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.ModifyVpcAttribute)
}

func (d *Daemon) handleEC2DescribeVpcAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeVpcAttribute)
}

func (d *Daemon) handleEC2CreateNetworkInterface(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.CreateNetworkInterface)
}

func (d *Daemon) handleEC2DeleteNetworkInterface(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DeleteNetworkInterface)
}

func (d *Daemon) handleEC2ModifyNetworkInterfaceAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.ModifyNetworkInterfaceAttribute)
}

func (d *Daemon) handleEC2DescribeNetworkInterfaces(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeNetworkInterfaces)
}

// handleAccountCreated creates a default VPC for a newly created account.
func (d *Daemon) handleAccountCreated(msg *nats.Msg) {
	var evt struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("Failed to unmarshal account creation event", "error", err)
		return
	}
	if evt.AccountID == "" {
		slog.Error("Account creation event has empty account ID")
		return
	}
	if _, err := d.vpcService.EnsureDefaultVPC(evt.AccountID); err != nil {
		slog.Error("Failed to create default VPC for new account",
			"accountID", evt.AccountID, "error", err)
	}
	d.ensureDefaultVPCInfrastructure()
}

// ensureDefaultVPCInfrastructure attaches an IGW and a default 0.0.0.0/0 route
// to each account's default VPC. The default SG is provisioned by
// EnsureDefaultVPC / CreateVpc itself, so this routine is IGW-only.
func (d *Daemon) ensureDefaultVPCInfrastructure() {
	if d.igwService == nil || d.vpcService == nil {
		return
	}

	for _, accountID := range []string{utils.GlobalAccountID, admin.DefaultAccountID()} {
		// Find the default VPC for this account
		descOut, err := d.vpcService.DescribeVpcs(&ec2.DescribeVpcsInput{}, accountID)
		if err != nil {
			continue
		}
		var defaultVpcId string
		for _, vpc := range descOut.Vpcs {
			if vpc.IsDefault != nil && *vpc.IsDefault {
				defaultVpcId = *vpc.VpcId
				break
			}
		}
		if defaultVpcId == "" {
			continue
		}

		// Check if IGW already attached
		igwOut, err := d.igwService.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{}, accountID)
		if err != nil {
			continue
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
				createOut, err = d.igwService.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}, accountID)
			}
			if err != nil {
				slog.Error("Failed to create default IGW", "accountID", accountID, "err", err)
				continue
			}
			igwId := *createOut.InternetGateway.InternetGatewayId
			_, err = d.igwService.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
				InternetGatewayId: &igwId,
				VpcId:             &defaultVpcId,
			}, accountID)
			if err != nil {
				slog.Error("Failed to attach default IGW", "igwId", igwId, "vpcId", defaultVpcId, "err", err)
			} else {
				slog.Info("Attached default IGW to default VPC", "igwId", igwId, "vpcId", defaultVpcId, "accountID", accountID)
			}
		}

		// Add 0.0.0.0/0 → IGW route to the main route table (if not already present)
		if d.routeTableService != nil {
			d.ensureDefaultIGWRoute(accountID, defaultVpcId)
		}
	}
}

// ensureDefaultIGWRoute adds 0.0.0.0/0 → igw-xxx to the main route table if not present
func (d *Daemon) ensureDefaultIGWRoute(accountID, vpcID string) {
	// Find the main route table for this VPC
	vpcFilter := "vpc-id"
	mainFilter := "association.main"
	trueVal := "true"
	descOut, err := d.routeTableService.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: &vpcFilter, Values: []*string{&vpcID}},
			{Name: &mainFilter, Values: []*string{&trueVal}},
		},
	}, accountID)
	if err != nil {
		slog.Warn("Failed to query main route table for default IGW route", "vpcId", vpcID, "err", err)
		return
	}
	if len(descOut.RouteTables) == 0 {
		slog.Debug("No main route table found for default IGW route", "vpcId", vpcID, "accountID", accountID)
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
	igwOut, err := d.igwService.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{}, accountID)
	if err != nil {
		slog.Warn("Failed to query IGWs for default route", "vpcId", vpcID, "err", err)
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
	_, err = d.routeTableService.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         mainRtb.RouteTableId,
		DestinationCidrBlock: &dest,
		GatewayId:            &igwID,
	}, accountID)
	if err != nil {
		slog.Warn("Failed to add default IGW route to main route table", "err", err)
	} else {
		slog.Info("Added default IGW route to main route table",
			"routeTableId", *mainRtb.RouteTableId, "igwId", igwID, "vpcId", vpcID)
	}
}
