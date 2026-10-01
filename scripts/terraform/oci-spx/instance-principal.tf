# Lets each node authenticate to the OCI API as itself, so the external-address
# allocator needs no API key on disk. Without one of these two credential routes a
# node forms and passes every health check, then refuses every launch that wants a
# public address with InsufficientAddressCapacity and no stated cause.
#
# Off by default, and deliberately: a dynamic group and a policy are tenancy-root
# resources, so this needs rights the compartment-scoped user the rest of this
# configuration runs as does not have and should not be given. Turn it on with a
# principal that holds them, or create both by hand once -- the OCIDs are stable
# and nothing here is per-node.

resource "oci_identity_dynamic_group" "nodes" {
  provider       = oci.home
  count          = var.enable_instance_principal ? 1 : 0
  compartment_id = var.tenancy_ocid
  name           = "${var.deployment_name}-nodes"
  description    = "Spinifex nodes in the ${var.deployment_name} deployment, authenticating as instance principals."

  # Scoped to the compartment, not to instance OCIDs: nodes are replaced, and a
  # rule listing them by OCID goes stale the first time one is.
  matching_rule = "ALL {instance.compartment.id = '${var.compartment_ocid}'}"
}

# Narrower than it looks. The allocator creates, moves and deletes secondary
# private IPs on a node's own VNIC and attaches public IPs to them, which is what
# these three verbs cover; nothing here grants compute, storage or identity.
resource "oci_identity_policy" "nodes" {
  provider       = oci.home
  count          = var.enable_instance_principal ? 1 : 0
  compartment_id = var.compartment_ocid
  name           = "${var.deployment_name}-nodes-network"
  description    = "Allows Spinifex nodes to manage the private and public IPs backing external addresses."

  statements = [
    "Allow dynamic-group ${oci_identity_dynamic_group.nodes[0].name} to use vnics in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.nodes[0].name} to manage private-ips in compartment id ${var.compartment_ocid}",
    "Allow dynamic-group ${oci_identity_dynamic_group.nodes[0].name} to manage public-ips in compartment id ${var.compartment_ocid}",
  ]
}
