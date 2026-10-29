package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go"
)

// sqsStoreDataDevice is the device name the store's data volume is recorded
// under; the guest finds the disk by serial, not by this name.
const sqsStoreDataDevice = "/dev/sdf"

// sqsStoreLaunch is an SQS store launch that has passed every check that can
// run before capacity, an ENI or a VM record exists.
type sqsStoreLaunch struct {
	volumeID   string
	listenAddr string
	clientNKey string
}

// sqsStoreImageDir holds the store's kernel and initramfs, beside the load
// balancer image whose firmware it shares.
func sqsStoreImageDir() string {
	return filepath.Join(microvmDir, "sqs-store")
}

// LaunchSQSStoreInstance launches the SQS store guest on this node with its
// existing system-account data volume attached at boot. Every refusal here
// happens before anything is allocated, mounted or recorded.
func (d *Daemon) LaunchSQSStoreInstance(input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	store, err := d.prepareSQSStoreLaunch(context.Background(), input)
	if err != nil {
		return nil, err
	}
	return d.launchDirectSystemInstance(input, store)
}

// LaunchSQSStoreInstanceOnNode launches the SQS store on nodeID, in-process
// when that is this node. A node without the SQS store subject has no
// responder, so the launch fails rather than reaching an older launch path.
func (d *Daemon) LaunchSQSStoreInstanceOnNode(nodeID string, input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: no input", systeminstance.ErrInvalidSQSStoreInput)
	}
	if nodeID == "" || nodeID == d.node {
		return d.LaunchSQSStoreInstance(input)
	}
	if err := input.ValidateSQSStore(); err != nil {
		return nil, err
	}
	if d.natsConn == nil {
		return nil, fmt.Errorf("sqs store: cannot target node %s without a NATS connection", nodeID)
	}
	out, err := d.requestSystemLaunch(systeminstance.SQSStoreLaunchSubject(input.InstanceType, nodeID), input)
	if errors.Is(err, nats.ErrNoResponders) {
		return nil, fmt.Errorf("sqs store: node %s does not serve SQS store launches for %s: %w", nodeID, input.InstanceType, err)
	}
	return out, err
}

// prepareSQSStoreLaunch checks the request, the store image and the data
// volume, and derives the guest's listen address from its default NIC.
func (d *Daemon) prepareSQSStoreLaunch(ctx context.Context, input *elbv2domain.SystemInstanceInput) (*sqsStoreLaunch, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: no input", systeminstance.ErrInvalidSQSStoreInput)
	}
	if err := input.ValidateSQSStore(); err != nil {
		return nil, err
	}
	if _, ok := d.resourceMgr.instanceTypes[input.InstanceType]; !ok {
		return nil, fmt.Errorf("unknown instance type: %s", input.InstanceType)
	}
	listenAddr, err := sqsStoreListenAddr(input.NICs)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"vmlinuz", "initramfs.cpio.gz"} {
		if _, err := os.Stat(filepath.Join(sqsStoreImageDir(), name)); err != nil {
			return nil, fmt.Errorf("sqs store image is not installed (%s): %w", name, err)
		}
	}
	if err := d.checkSQSStoreVolume(ctx, input.SQSStore.DataVolumeID); err != nil {
		return nil, err
	}
	return &sqsStoreLaunch{
		volumeID:   input.SQSStore.DataVolumeID,
		listenAddr: listenAddr,
		clientNKey: input.SQSStore.ClientNKey,
	}, nil
}

// sqsStoreListenAddr is the address of the one default NIC, which the guest
// configures and then requires its listener to be on.
func sqsStoreListenAddr(nics []elbv2domain.NICConfig) (string, error) {
	var addr string
	for _, nic := range nics {
		if !nic.IsDefault {
			continue
		}
		if addr != "" {
			return "", fmt.Errorf("%w: more than one default NIC", systeminstance.ErrInvalidSQSStoreInput)
		}
		ip, _, err := net.ParseCIDR(nic.CIDR)
		if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return "", fmt.Errorf("%w: default NIC address %q is not a usable IPv4 address", systeminstance.ErrInvalidSQSStoreInput, nic.CIDR)
		}
		addr = ip.String()
	}
	if addr == "" {
		return "", fmt.Errorf("%w: no default NIC", systeminstance.ErrInvalidSQSStoreInput)
	}
	return addr, nil
}

// checkSQSStoreVolume requires the volume to exist in the system account and
// be available. That is the normal EBS attach rule, not an ownership check.
func (d *Daemon) checkSQSStoreVolume(ctx context.Context, volumeID string) error {
	if d.volumeService == nil {
		return errors.New("sqs store: volume service unavailable")
	}
	out, err := d.volumeService.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeID)},
	}, awsidentifiers.GlobalAccountID)
	if err != nil {
		return fmt.Errorf("sqs store: describe data volume %s: %w", volumeID, err)
	}
	if out == nil || len(out.Volumes) != 1 || aws.StringValue(out.Volumes[0].VolumeId) != volumeID {
		return fmt.Errorf("sqs store: data volume %s not found in the system account", volumeID)
	}
	if state := aws.StringValue(out.Volumes[0].State); state != ec2.VolumeStateAvailable {
		return fmt.Errorf("sqs store: data volume %s is %s, not available", volumeID, state)
	}
	return nil
}

// sqsStoreNetcfgOnly keeps the network config and drops the load balancer's
// agent environment and CA bundle, which the store guest must not receive.
func sqsStoreNetcfgOnly(entries []vm.FwCfgEntry) []vm.FwCfgEntry {
	kept := make([]vm.FwCfgEntry, 0, 1)
	for _, e := range entries {
		if e.Name == "opt/spinifex/netcfg" {
			kept = append(kept, e)
			continue
		}
		_ = os.Remove(e.File)
	}
	return kept
}

// writeSQSStoreFwCfg writes the guest's three store values as fw_cfg files.
// The key is a public key; nothing secret is written or logged.
func writeSQSStoreFwCfg(instanceID string, store *sqsStoreLaunch) ([]vm.FwCfgEntry, error) {
	values := []struct{ name, value string }{
		{"data-serial", vm.VolumeSerial(store.volumeID)},
		{"listen-addr", store.listenAddr},
		{"client-nkey", store.clientNKey},
	}
	entries := make([]vm.FwCfgEntry, 0, len(values))
	for _, v := range values {
		path := filepath.Join(hostprocess.RuntimeDir(), fmt.Sprintf("fwcfg-%s-sqs-%s.tmp", instanceID, v.name))
		if err := os.WriteFile(path, []byte(v.value), 0600); err != nil {
			for _, e := range entries {
				_ = os.Remove(e.File)
			}
			return nil, fmt.Errorf("write %s: %w", v.name, err)
		}
		entries = append(entries, vm.FwCfgEntry{Name: "opt/spinifex/sqs-store/" + v.name, File: path})
	}
	return entries, nil
}
