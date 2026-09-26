output "compartment_name" { value = data.oci_identity_compartment.mulgadc.name }
output "compartment_ocid" { value = var.compartment_ocid }
output "vcn_ocid" { value = oci_core_vcn.mulgadc.id }
output "subnet_ocids" { value = { public = oci_core_subnet.public.id, private = oci_core_subnet.private.id } }
output "gateway_ocids" { value = { internet_gateway = oci_core_internet_gateway.mulgadc.id, nat_gateway = oci_core_nat_gateway.mulgadc.id, service_gateway = oci_core_service_gateway.mulgadc.id } }
output "route_table_ocids" { value = { public = oci_core_route_table.public.id, private = oci_core_route_table.private.id } }
output "security_list_ocids" { value = { public = oci_core_security_list.public.id, private = oci_core_security_list.private.id } }
output "compute_ocids" { value = [for instance in oci_core_instance.mulgadc : instance.id] }
output "data_volume_ocids" { value = [for volume in oci_core_volume.mulgadc_data : volume.id] }

# What the Spinifex deploy path consumes. The primary VNIC is the host plane, so
# these are the addresses update-nodes.sh and install-node.sh connect to.
output "nodes" {
  value = [for instance in oci_core_instance.mulgadc : {
    name         = instance.display_name
    public_ip    = instance.public_ip
    private_ip   = instance.private_ip
    fault_domain = instance.fault_domain
  }]
}

# Ready for `update-nodes.sh --hosts-file`, so the handoff between Terraform and
# the deploy scripts is a file rather than a copy-paste.
output "hosts_file" {
  value = join("\n", [for instance in oci_core_instance.mulgadc : instance.public_ip])
}
