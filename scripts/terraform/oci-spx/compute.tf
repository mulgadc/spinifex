data "oci_identity_availability_domains" "available" {
  compartment_id = var.tenancy_ocid
}

# ap-sydney-1 has exactly one availability domain, so spreading nodes across ADs
# is not an option there and fault domains are the whole of the hardware
# separation available. Each one is a different set of racks, power and network
# inside the AD, which is what a Spinifex node needs from its neighbours.
data "oci_identity_fault_domains" "available" {
  compartment_id      = var.compartment_ocid
  availability_domain = local.compute_availability_domain
}

# Uses the newest Ubuntu 26.04 platform image that OCI advertises for this
# shape and region at plan time, rather than pinning a regional image OCID.
data "oci_core_images" "ubuntu_2604" {
  compartment_id           = var.tenancy_ocid
  operating_system         = "Canonical Ubuntu"
  operating_system_version = var.ubuntu_version
  shape                    = var.compute_shape
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

locals {
  compute_availability_domain = data.oci_identity_availability_domains.available.availability_domains[0].name
  ubuntu_image_id             = data.oci_core_images.ubuntu_2604.images[0].id

  # shape_config sizes a flex shape and is rejected outright on a fixed bare-metal
  # one, so it is emitted only when the shape name says Flex.
  shape_is_flex = strcontains(var.compute_shape, "Flex")

  # Round-robin, so three nodes land on three fault domains rather than wherever
  # OCI's placement happens to put them. Left to itself OCI spreads on a
  # best-effort basis and says nothing about the result, which is not something
  # a storage cluster should take on trust.
  fault_domains = data.oci_identity_fault_domains.available.fault_domains[*].name

  # Every node has exactly one data volume, so every node names it the same. Asking
  # for the path is what makes it stable: a volume attached without one is named
  # after the boot disk (mulga-poc has oraclevda1 pointing at sdb1).
  data_device = "/dev/oracleoci/oraclevdb"

  # Identical on every node, so a change to it replaces all of them together.
  user_data = templatefile("${path.module}/cloud-init/user-data.yaml.tftpl", {
    data_device          = local.data_device
    data_mountpoint      = var.data_mountpoint
    peer_cidr            = var.vcn_cidr
    service_ports        = join(", ", [for p in var.node_service_ports : "\"${p}\""])
    wan_bridge_name      = var.wan_bridge_name
    wan_bridge_mtu       = var.wan_bridge_mtu
    mount_script         = file("${path.module}/cloud-init/mount-data-volume.sh")
    wan_bridge_script    = file("${path.module}/cloud-init/setup-wan-bridge.sh")
    service_ports_script = file("${path.module}/cloud-init/open-service-ports.sh")
  })
}

resource "oci_core_instance" "mulgadc" {
  count               = var.node_count
  availability_domain = local.compute_availability_domain
  fault_domain        = local.fault_domains[count.index % length(local.fault_domains)]
  compartment_id      = var.compartment_ocid
  display_name        = format("%s-node-%02d", var.deployment_name, count.index + 1)
  shape               = var.compute_shape

  dynamic "shape_config" {
    for_each = local.shape_is_flex ? [1] : []
    content {
      ocpus         = var.compute_ocpus
      memory_in_gbs = var.compute_memory_in_gbs
    }
  }

  metadata = {
    ssh_authorized_keys = file(var.ssh_public_key_path)
    user_data           = base64encode(local.user_data)
  }

  # Required for automatic UHP iSCSI multipath setup and login.
  agent_config {
    plugins_config {
      name          = "Block Volume Management"
      desired_state = "ENABLED"
    }
  }

  # The host plane: SSH, the node's advertise address, and the Geneve overlay.
  # Public, because a node serves the UI, predastore and the AWS gateway itself.
  create_vnic_details {
    subnet_id        = oci_core_subnet.public.id
    assign_public_ip = true
    hostname_label   = format("%snode%02d", var.deployment_name, count.index + 1)
  }

  source_details {
    source_type = "image"
    source_id   = local.ubuntu_image_id
  }
}

# Every Spinifex external address lives here as a secondary private IP, written by
# the OCI allocator at runtime. Same subnet as the primary, which mulga-poc proves
# works. E6.Flex allows two VNICs total, so this is the whole remaining budget.
resource "oci_core_vnic_attachment" "mulgadc_external" {
  count       = var.node_count
  instance_id = oci_core_instance.mulgadc[count.index].id

  create_vnic_details {
    subnet_id        = oci_core_subnet.public.id
    display_name     = format("%s-node-%02d-external", var.deployment_name, count.index + 1)
    assign_public_ip = true

    # Deliberately left at the default. Routed NAT masquerades to this VNIC's own
    # address and an EIP's private half is a registered IP here, so nothing we
    # send is a foreign source and skipping the check would buy nothing.
    skip_source_dest_check = false
  }
}

# Backs /var/lib/spinifex, so viperblock, predastore and JetStream all land on it.
# OCI presents the device; partitioning, formatting and mounting remain an OS task.
resource "oci_core_volume" "mulgadc_data" {
  count               = var.node_count
  availability_domain = local.compute_availability_domain
  compartment_id      = var.compartment_ocid
  display_name        = format("%s-node-%02d-data", var.deployment_name, count.index + 1)
  size_in_gbs         = var.data_volume_size_in_gbs
  vpus_per_gb         = var.data_volume_vpus_per_gb
}

resource "oci_core_volume_attachment" "mulgadc_data" {
  count           = var.node_count
  attachment_type = "iscsi"
  # Required by OCI for Ultra High Performance iSCSI volume attachments.
  device                            = local.data_device
  is_agent_auto_iscsi_login_enabled = true
  instance_id                       = oci_core_instance.mulgadc[count.index].id
  volume_id                         = oci_core_volume.mulgadc_data[count.index].id
}
