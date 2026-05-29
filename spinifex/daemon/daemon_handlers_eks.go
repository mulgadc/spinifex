package daemon

import "github.com/nats-io/nats.go"

// --- Cluster ---

func (d *Daemon) handleEKSCreateCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.CreateCluster)
}

func (d *Daemon) handleEKSDescribeCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeCluster)
}

func (d *Daemon) handleEKSListClusters(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListClusters)
}

func (d *Daemon) handleEKSUpdateClusterConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateClusterConfig)
}

func (d *Daemon) handleEKSUpdateClusterVersion(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateClusterVersion)
}

func (d *Daemon) handleEKSDeleteCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DeleteCluster)
}

// --- Nodegroup ---

func (d *Daemon) handleEKSCreateNodegroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.CreateNodegroup)
}

func (d *Daemon) handleEKSDescribeNodegroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeNodegroup)
}

func (d *Daemon) handleEKSListNodegroups(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListNodegroups)
}

func (d *Daemon) handleEKSUpdateNodegroupConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateNodegroupConfig)
}

func (d *Daemon) handleEKSUpdateNodegroupVersion(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateNodegroupVersion)
}

func (d *Daemon) handleEKSDeleteNodegroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DeleteNodegroup)
}

// --- AccessEntry + AccessPolicy ---

func (d *Daemon) handleEKSCreateAccessEntry(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.CreateAccessEntry)
}

func (d *Daemon) handleEKSDescribeAccessEntry(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeAccessEntry)
}

func (d *Daemon) handleEKSListAccessEntries(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListAccessEntries)
}

func (d *Daemon) handleEKSUpdateAccessEntry(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateAccessEntry)
}

func (d *Daemon) handleEKSDeleteAccessEntry(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DeleteAccessEntry)
}

func (d *Daemon) handleEKSAssociateAccessPolicy(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.AssociateAccessPolicy)
}

func (d *Daemon) handleEKSDisassociateAccessPolicy(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DisassociateAccessPolicy)
}

func (d *Daemon) handleEKSListAssociatedAccessPolicies(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListAssociatedAccessPolicies)
}

func (d *Daemon) handleEKSListAccessPolicies(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListAccessPolicies)
}

// --- Addons ---

func (d *Daemon) handleEKSListAddons(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListAddons)
}

func (d *Daemon) handleEKSDescribeAddonVersions(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeAddonVersions)
}

func (d *Daemon) handleEKSCreateAddon(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.CreateAddon)
}

func (d *Daemon) handleEKSDeleteAddon(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DeleteAddon)
}

func (d *Daemon) handleEKSDescribeAddon(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeAddon)
}

func (d *Daemon) handleEKSUpdateAddon(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UpdateAddon)
}

// --- OIDC identity-provider configs ---

func (d *Daemon) handleEKSAssociateIdentityProviderConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.AssociateIdentityProviderConfig)
}

func (d *Daemon) handleEKSDescribeIdentityProviderConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DescribeIdentityProviderConfig)
}

func (d *Daemon) handleEKSListIdentityProviderConfigs(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListIdentityProviderConfigs)
}

func (d *Daemon) handleEKSDisassociateIdentityProviderConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.DisassociateIdentityProviderConfig)
}

// --- Tags ---

func (d *Daemon) handleEKSTagResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.TagResource)
}

func (d *Daemon) handleEKSUntagResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.UntagResource)
}

func (d *Daemon) handleEKSListTagsForResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.eksService.ListTagsForResource)
}
