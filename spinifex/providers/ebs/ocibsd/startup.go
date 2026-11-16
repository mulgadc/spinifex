package ocibsd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// RequiredPolicy is what the tenancy must grant the node's credential in the
// compartment ocibsd keeps its volumes in.
const RequiredPolicy = "manage volume-family and use instances"

// CheckCredential makes the read ocibsd makes at start: it proves client
// authenticates and can read volumes in the compartment, not that every grant
// in RequiredPolicy is present, so an inspect-only credential passes.
func CheckCredential(ctx context.Context, client oci.BlockClient, compartmentID, availabilityDomain, clusterID string) error {
	if _, err := client.ListVolumes(ctx, oci.VolumeFilter{
		CompartmentID: compartmentID, AvailabilityDomain: availabilityDomain,
		Tags: map[string]string{TagCluster: clusterID},
	}); err != nil {
		return fmt.Errorf("ocibsd: read volumes in compartment %s: %w; the tenancy needs a policy granting this node's credential %s in that compartment, as \"Allow dynamic-group <group> to manage volume-family in compartment id %[1]s\" and \"Allow dynamic-group <group> to use instances in compartment id %[1]s\" (or group <group> for an API key)",
			compartmentID, err, RequiredPolicy)
	}
	return nil
}

// Start readies the provider on node start, before it serves anything. It
// checks the credential as CheckCredential does, then logs out stale
// sessions; PublishVolume is refused until it ends.
func (p *Provider) Start(ctx context.Context) error {
	if err := CheckCredential(ctx, p.cfg.Client, p.cfg.CompartmentID, p.cfg.AvailabilityDomain, p.cfg.ClusterID); err != nil {
		return err
	}
	if err := p.logoutStaleSessions(ctx); err != nil {
		return err
	}
	p.started.Store(true)
	return nil
}

func (p *Provider) checkStarted() error {
	if !p.started.Load() {
		return fmt.Errorf("%w: ocibsd on node %s has not finished starting", ebsprovider.ErrUnavailable, p.cfg.NodeID)
	}
	return nil
}

// logoutStaleSessions logs out a session only when its target is an OCI
// attachment of a volume tagged with this cluster that OCI no longer attaches
// to this node. A session it cannot tie to such a volume is left alone.
func (p *Provider) logoutStaleSessions(ctx context.Context) error {
	sessions, err := p.cfg.Initiator.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("ocibsd: list iSCSI sessions: %w", err)
	}
	var candidates []Target
	for _, s := range sessions {
		if strings.HasPrefix(s.IQN, oci.IQNPrefix) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	attachments, err := p.cfg.Client.ListVolumeAttachments(ctx, oci.AttachmentFilter{CompartmentID: p.cfg.CompartmentID})
	if err != nil {
		return fmt.Errorf("ocibsd: list volume attachments: %w", err)
	}
	for _, s := range candidates {
		stale, reason := p.isStale(ctx, s, attachments)
		if !stale {
			slog.InfoContext(ctx, "ocibsd: keeping iSCSI session", "iqn", s.IQN, "portal", portal(s), "reason", reason)
			continue
		}
		if err := p.cfg.Initiator.Logout(ctx, s); err != nil {
			return fmt.Errorf("ocibsd: log out of stale session %s: %w", s.IQN, err)
		}
		slog.InfoContext(ctx, "ocibsd: logged out stale iSCSI session", "iqn", s.IQN, "portal", portal(s))
	}
	return nil
}

// isStale decides one session, and says why a session it keeps is kept.
func (p *Provider) isStale(ctx context.Context, s Target, attachments []oci.VolumeAttachment) (bool, string) {
	volumes := map[string]bool{}
	for _, a := range attachments {
		if a.IQN == s.IQN {
			volumes[a.VolumeID] = true
		}
	}
	if len(volumes) == 0 {
		return false, "no OCI attachment has this IQN"
	}
	for volumeID := range volumes {
		v, err := p.cfg.Client.GetVolume(ctx, volumeID)
		if err != nil {
			return false, "volume " + volumeID + " cannot be read: " + err.Error()
		}
		if v.FreeformTags[TagCluster] != p.cfg.ClusterID {
			return false, "volume " + volumeID + " is not this cluster's"
		}
		for _, a := range attachments {
			if a.VolumeID == volumeID && a.InstanceID == p.cfg.InstanceID && a.State != oci.AttachmentDetached {
				return false, "volume " + volumeID + " is attached to this node"
			}
		}
	}
	return true, ""
}
