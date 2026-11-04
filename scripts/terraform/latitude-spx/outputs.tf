output "deployment_name" {
  value = var.deployment_name
}

output "site" {
  value = var.site
}

output "project" {
  value = local.project
}

# provider_host_id is the server ID. public_ip is the server's own /31 address, which SSH and the
# Spinifex API use; the Elastic IP is separate.
output "nodes" {
  value = [for n in latitudesh_server.node : {
    name             = trimprefix(n.hostname, "spx-${var.deployment_name}-")
    provider_host_id = n.id
    public_ip        = n.primary_ipv4
  }]
}

# The first address, for a single node's static pool.
output "elastic_ip" {
  value = latitudesh_elastic_ip.pool[0].address
}

# IDs as well as addresses: destroy deletes only these IDs, and the floating pool lists them.
output "elastic_ips" {
  value = [for e in latitudesh_elastic_ip.pool : { id = e.id, address = e.address }]
}

# The VLAN ID of the cluster network; null for one node, which has no private network.
output "vid" {
  value = length(latitudesh_virtual_network.lan) == 0 ? null : latitudesh_virtual_network.lan[0].vid
}

output "ssh_key_id" {
  value = latitudesh_ssh_key.deploy.id
}

output "user_data_id" {
  value = latitudesh_user_data.host.id
}

output "project_id" {
  value = local.project_id
}

output "project_created" {
  value = length(latitudesh_project.deploy) > 0
}

output "virtual_network_id" {
  value = length(latitudesh_virtual_network.lan) == 0 ? null : latitudesh_virtual_network.lan[0].id
}
