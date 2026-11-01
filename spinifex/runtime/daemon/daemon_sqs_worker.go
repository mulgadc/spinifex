package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// sqsWorkerImageDir holds the worker's kernel and initramfs, beside the SQS
// store image.
func sqsWorkerImageDir() string {
	return filepath.Join(microvmDir, "sqs-worker")
}

// sqsWorkerLaunch is an SQS worker launch that has passed every check that
// can run before capacity, an ENI or a VM record exists.
type sqsWorkerLaunch struct {
	volumeID   string
	region     string
	gatewayURL string
	caCert     string
}

var _ sqsGuest = (*sqsWorkerLaunch)(nil)

func (w *sqsWorkerLaunch) imageDir() string   { return sqsWorkerImageDir() }
func (w *sqsWorkerLaunch) dataVolume() string { return w.volumeID }
func (w *sqsWorkerLaunch) needsMgmtNIC() bool { return true }
func (w *sqsWorkerLaunch) label() string      { return "SQS worker" }

func (w *sqsWorkerLaunch) writeFwCfg(instanceID string) ([]vm.FwCfgEntry, error) {
	return writeSQSWorkerFwCfg(instanceID, w)
}

func (w *sqsWorkerLaunch) record(inst *vm.VM, nodeID, netcfg string) {
	inst.SQSWorkerGuest = &vm.SQSWorkerGuest{
		NodeID:       nodeID,
		DataVolumeID: w.volumeID,
		Region:       w.region,
		GatewayURL:   w.gatewayURL,
		CACert:       w.caCert,
		Netcfg:       netcfg,
	}
}

// LaunchSQSWorkerInstance launches the shared SQS worker on this node with its
// existing encrypted system-account data volume attached at boot. Every
// refusal here happens before anything is allocated, mounted or recorded.
func (d *Daemon) LaunchSQSWorkerInstance(ctx context.Context, input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	w, err := d.prepareSQSWorkerLaunch(ctx, input)
	if err != nil {
		return nil, err
	}
	return d.launchDirectSystemInstance(input, w)
}

// prepareSQSWorkerLaunch checks the request, the worker image, this node's
// mgmt bridge and trust store, and the data volume's own metadata.
func (d *Daemon) prepareSQSWorkerLaunch(ctx context.Context, input *elbv2domain.SystemInstanceInput) (*sqsWorkerLaunch, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: no input", systeminstance.ErrInvalidSQSWorkerInput)
	}
	if err := input.ValidateSQSWorker(); err != nil {
		return nil, err
	}
	if _, ok := d.resourceMgr.instanceTypes[input.InstanceType]; !ok {
		return nil, fmt.Errorf("unknown instance type: %s", input.InstanceType)
	}
	ca, err := d.sqsWorkerHostReady()
	if err != nil {
		return nil, err
	}
	if err := d.checkSQSWorkerVolume(ctx, input.SQSWorker.DataVolumeID); err != nil {
		return nil, err
	}
	return &sqsWorkerLaunch{
		volumeID:   input.SQSWorker.DataVolumeID,
		region:     input.SQSWorker.Region,
		gatewayURL: d.resolveSystemGatewayBaseURL(),
		caCert:     ca,
	}, nil
}

// sqsWorkerHostReady checks what this node needs to boot a worker: a complete
// image, the mgmt bridge the gateway URL names, and the cluster CA. It
// returns the CA. The launcher calls it before allocating anything.
func (d *Daemon) sqsWorkerHostReady() (string, error) {
	if d.mgmtIPAllocator == nil || d.mgmtBridgeIP == "" {
		return "", errors.New("sqs worker: this node has no mgmt bridge")
	}
	for _, name := range []string{"vmlinuz", "initramfs.cpio.gz"} {
		info, err := os.Stat(filepath.Join(sqsWorkerImageDir(), name))
		if err != nil {
			return "", fmt.Errorf("sqs worker image is not installed (%s): %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return "", fmt.Errorf("sqs worker image is incomplete: %s is empty or not a file", name)
		}
	}
	return d.sqsGuestGatewayCA("sqs worker")
}

// sqsGuestGatewayCA checks that an SQS guest can reach this node's gateway:
// a mgmt bridge and a readable cluster CA, which it returns.
func (d *Daemon) sqsGuestGatewayCA(label string) (string, error) {
	if d.mgmtIPAllocator == nil || d.mgmtBridgeIP == "" {
		return "", fmt.Errorf("%s: this node has no mgmt bridge", label)
	}
	if d.config == nil || d.config.NATS.CACert == "" {
		return "", fmt.Errorf("%s: no cluster CA is configured", label)
	}
	ca, err := os.ReadFile(d.config.NATS.CACert)
	if err != nil || len(strings.TrimSpace(string(ca))) == 0 {
		return "", fmt.Errorf("%s: cluster CA %s is unreadable or empty", label, d.config.NATS.CACert)
	}
	return string(ca), nil
}

// sqsWorkerHost is the launcher's view of sqsWorkerHostReady.
type sqsWorkerHost struct{ check func() (string, error) }

func (h sqsWorkerHost) Ready(context.Context) error {
	_, err := h.check()
	return err
}

// checkSQSWorkerVolume requires the volume's own record, in the system
// account, to say it is available and encrypted; no caller flag is trusted.
func (d *Daemon) checkSQSWorkerVolume(ctx context.Context, volumeID string) error {
	if d.volumeService == nil {
		return errors.New("sqs worker: volume service unavailable")
	}
	out, err := d.volumeService.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeID)},
	}, awsidentifiers.GlobalAccountID)
	if err != nil {
		return fmt.Errorf("sqs worker: describe data volume %s: %w", volumeID, err)
	}
	if out == nil || len(out.Volumes) != 1 || aws.StringValue(out.Volumes[0].VolumeId) != volumeID {
		return fmt.Errorf("sqs worker: data volume %s not found in the system account", volumeID)
	}
	vol := out.Volumes[0]
	if state := aws.StringValue(vol.State); state != ec2.VolumeStateAvailable {
		return fmt.Errorf("sqs worker: data volume %s is %s, not available", volumeID, state)
	}
	if !aws.BoolValue(vol.Encrypted) {
		return fmt.Errorf("sqs worker: data volume %s is not encrypted; the cluster storage key is not configured", volumeID)
	}
	return nil
}

var sqsWorkerFwCfgNames = []string{"data-serial", "region", "gateway-url"}

func sqsWorkerFwCfgEntry(instanceID, name string) vm.FwCfgEntry {
	return vm.FwCfgEntry{
		Name: "opt/spinifex/sqs-worker/" + name,
		File: filepath.Join(hostprocess.RuntimeDir(), fmt.Sprintf("fwcfg-%s-sqsw-%s.tmp", instanceID, name)),
	}
}

// sqsWorkerCAEntry is the CA bundle the guest's /init installs.
func sqsWorkerCAEntry(instanceID string) vm.FwCfgEntry {
	return vm.FwCfgEntry{
		Name: "opt/spinifex/ca-cert",
		File: filepath.Join(hostprocess.RuntimeDir(), fmt.Sprintf("fwcfg-%s-sqsw-cacert.tmp", instanceID)),
	}
}

// writeSQSWorkerFwCfg writes the worker's values and CA as fw_cfg files.
// Nothing written is secret: the CA is a public certificate.
func writeSQSWorkerFwCfg(instanceID string, w *sqsWorkerLaunch) ([]vm.FwCfgEntry, error) {
	values := map[string]string{
		"data-serial": vm.VolumeSerial(w.volumeID),
		"region":      w.region,
		"gateway-url": w.gatewayURL,
	}
	entries := make([]vm.FwCfgEntry, 0, len(sqsWorkerFwCfgNames)+1)
	write := func(e vm.FwCfgEntry, value string) error {
		if err := os.WriteFile(e.File, []byte(value), 0600); err != nil {
			for _, e := range entries {
				_ = os.Remove(e.File)
			}
			return fmt.Errorf("write %s: %w", e.Name, err)
		}
		entries = append(entries, e)
		return nil
	}
	for _, name := range sqsWorkerFwCfgNames {
		if err := write(sqsWorkerFwCfgEntry(instanceID, name), values[name]); err != nil {
			return nil, err
		}
	}
	if err := write(sqsWorkerCAEntry(instanceID), w.caCert); err != nil {
		return nil, err
	}
	return entries, nil
}

// refreshSQSWorkerState rewrites an SQS worker guest's fw_cfg files from its
// record for a relaunch on the node that launched it. Every other relaunch
// is refused, so the worker never moves to another node.
func (d *Daemon) refreshSQSWorkerState(inst *vm.VM) error {
	g := inst.SQSWorkerGuest
	if g == nil {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched: its record has no guest config", inst.ID)
	}
	if g.NodeID == "" || g.NodeID != d.node {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched on node %s: it was launched on node %q and is only restarted there", inst.ID, d.node, g.NodeID)
	}
	w := &systeminstance.SQSWorkerInput{DataVolumeID: g.DataVolumeID, Region: g.Region}
	if err := w.Validate(); err != nil {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched: %w", inst.ID, err)
	}
	if !strings.HasPrefix(g.GatewayURL, "https://") || strings.TrimSpace(g.CACert) == "" || g.Netcfg == "" {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched: its record lacks the gateway URL, CA or network config", inst.ID)
	}
	if !sqsStoreKeepsDataVolume(inst, g.DataVolumeID) {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched: data volume %s is not among its kept volumes", inst.ID, g.DataVolumeID)
	}
	want := []vm.FwCfgEntry{{Name: "opt/spinifex/netcfg", File: netcfgFwCfgPath(inst.ID)}}
	for _, name := range sqsWorkerFwCfgNames {
		want = append(want, sqsWorkerFwCfgEntry(inst.ID, name))
	}
	want = append(want, sqsWorkerCAEntry(inst.ID))
	if !slices.Equal(inst.Config.FwCfg, want) {
		return fmt.Errorf("sqs worker instance %s cannot be relaunched: its recorded fw_cfg files are not the worker's", inst.ID)
	}
	if err := os.WriteFile(want[0].File, []byte(g.Netcfg), 0600); err != nil {
		return fmt.Errorf("sqs worker instance %s: write netcfg: %w", inst.ID, err)
	}
	if _, err := writeSQSWorkerFwCfg(inst.ID, &sqsWorkerLaunch{
		volumeID: g.DataVolumeID, region: g.Region, gatewayURL: g.GatewayURL, caCert: g.CACert,
	}); err != nil {
		_ = os.Remove(want[0].File)
		return fmt.Errorf("sqs worker instance %s: %w", inst.ID, err)
	}
	slog.Info("Refreshed SQS worker guest config", "instanceId", inst.ID)
	return nil
}
