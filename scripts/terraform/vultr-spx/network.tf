# One VPC per deployment, single node included, so the encap address and the
# east-west path are the same shape at one node as at three.
resource "vultr_vpc" "lan" {
  region         = var.region
  description    = "spinifex-${var.deployment_name}"
  v4_subnet      = var.vpc_subnet
  v4_subnet_mask = var.vpc_subnet_mask
}
