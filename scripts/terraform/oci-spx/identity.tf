# Upstream created the compartment, an IAM group and a root policy. We do not:
# those are tenancy-root resources, and the API user Spinifex runs as is scoped to
# a compartment on purpose. Deploying into an existing compartment is the whole
# point -- creating one needs rights no node should ever hold.
data "oci_identity_compartment" "mulgadc" {
  id = var.compartment_ocid
}
