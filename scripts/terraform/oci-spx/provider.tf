# A runner gets the key as a value, a workstation as a path. Passing the value
# means CI never writes key material to disk, so there is nothing to leave behind
# on a shared runner and nothing to clean up after a cancelled job.
provider "oci" {
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key      = var.private_key != "" ? var.private_key : null
  private_key_path = var.private_key == "" ? var.private_key_path : null
  region           = var.region
}

# Region subscriptions are readable from the deployment-region provider. OCI
# marks exactly one subscription as the tenancy home region; its IAM endpoint is
# where dynamic groups and policies must be written.
data "oci_identity_region_subscriptions" "tenancy" {
  count      = var.instance_principal == "create" ? 1 : 0
  tenancy_id = var.tenancy_ocid
}
locals {
  tenancy_home_region = var.instance_principal == "create" ? one([
    for subscription in data.oci_identity_region_subscriptions.tenancy[0].region_subscriptions : subscription.region_name
    if subscription.is_home_region
  ]) : var.region
}
# OCI serves IAM writes from the tenancy's home region, so the dynamic group and
# policy go through this one. Defaults to var.region, which makes it identical to
# the provider above until OCI_HOME_REGION says otherwise -- so a tenancy whose
# home region is where we build is unaffected, and ours is not.
provider "oci" {
  alias            = "home"
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key      = var.private_key != "" ? var.private_key : null
  private_key_path = var.private_key == "" ? var.private_key_path : null
  region           = local.tenancy_home_region
}
