data "oci_core_services" "oracle_services_network" {}

locals {
  all_oracle_services = one([for s in data.oci_core_services.oracle_services_network.services : s if strcontains(s.name, "Services In Oracle Services Network")])
}

resource "oci_core_vcn" "mulgadc" {
  compartment_id = var.compartment_ocid
  display_name   = "${var.deployment_name}-vcn"
  dns_label      = var.deployment_name
  cidr_blocks    = [var.vcn_cidr]
}
resource "oci_core_internet_gateway" "mulgadc" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-igw"
  enabled        = true
}
resource "oci_core_nat_gateway" "mulgadc" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-nat"
}
resource "oci_core_service_gateway" "mulgadc" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-sgw"
  services { service_id = local.all_oracle_services.id }
}
resource "oci_core_route_table" "public" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-public-rt"
  route_rules {
    network_entity_id = oci_core_internet_gateway.mulgadc.id
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
  }
}
resource "oci_core_route_table" "private" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-private-rt"
  route_rules {
    network_entity_id = oci_core_nat_gateway.mulgadc.id
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
  }
  route_rules {
    network_entity_id = oci_core_service_gateway.mulgadc.id
    destination       = local.all_oracle_services.cidr_block
    destination_type  = "SERVICE_CIDR_BLOCK"
  }
}

# Private-subnet ingress is limited to traffic originating inside this VCN.
# Egress remains open so private workloads can use the NAT and Service Gateway.
resource "oci_core_security_list" "private" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-private-sl"

  ingress_security_rules {
    protocol = "6" # TCP
    source   = var.vcn_cidr

    tcp_options {
      min = 22
      max = 22
    }
  }

  ingress_security_rules {
    protocol = "1" # ICMP
    source   = var.vcn_cidr

    icmp_options {
      type = 8 # Echo request (ping)
      code = 0
    }
  }

  egress_security_rules {
    protocol    = "all"
    destination = "0.0.0.0/0"
  }
}
# Deliberately permissive, and that is the design rather than a shortcut.
#
# A Spinifex guest's firewall is its AWS security group, enforced by OVN on the
# node. A customer who opens 443 on their own security group expects 443 to
# work, and they have no way to know an OCI security list exists, let alone to
# edit one. Enumerating ports here would mean every customer port needed a
# Terraform change, and the failure would present as "my security group does
# nothing" — which is the hardest kind of network fault to trace.
#
# So the layers are: OCI security list open, OVN security groups enforce the
# guest policy, and the host firewall (cloud-init/open-service-ports.sh) guards
# the node's own listeners. Narrow `node_client_cidr_allow_list` to restrict
# who can reach the deployment at all; the intra-VCN rule is separate so that
# narrowing it can never cut Geneve, OVN, NATS or predastore between nodes.
resource "oci_core_security_list" "public" {
  compartment_id = var.compartment_ocid
  vcn_id         = oci_core_vcn.mulgadc.id
  display_name   = "${var.deployment_name}-public-sl"

  ingress_security_rules {
    protocol = "all"
    source   = var.vcn_cidr
  }

  dynamic "ingress_security_rules" {
    for_each = var.node_client_cidr_allow_list
    content {
      protocol = "all"
      source   = ingress_security_rules.value
    }
  }

  egress_security_rules {
    protocol    = "all"
    destination = "0.0.0.0/0"
  }
}

resource "oci_core_subnet" "public" {
  compartment_id             = var.compartment_ocid
  vcn_id                     = oci_core_vcn.mulgadc.id
  display_name               = "${var.deployment_name}-public-subnet"
  dns_label                  = "public"
  cidr_block                 = var.public_subnet_cidr
  route_table_id             = oci_core_route_table.public.id
  prohibit_public_ip_on_vnic = false
  security_list_ids          = [oci_core_security_list.public.id]
}
resource "oci_core_subnet" "private" {
  compartment_id             = var.compartment_ocid
  vcn_id                     = oci_core_vcn.mulgadc.id
  display_name               = "${var.deployment_name}-private-subnet"
  dns_label                  = "private"
  cidr_block                 = var.private_subnet_cidr
  route_table_id             = oci_core_route_table.private.id
  prohibit_public_ip_on_vnic = true
  # Retain the VCN default list and add the private workload rules.
  security_list_ids = [oci_core_vcn.mulgadc.default_security_list_id, oci_core_security_list.private.id]
}

# Deliberately absent: DRG, remote peering connection, local peering gateway, and RZG attachment.
