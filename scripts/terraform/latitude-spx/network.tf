# A private network needs two or more servers, so a single node has none. The VLAN carries the cluster
# and Geneve traffic; its address is set over SSH, since the VID is only known after apply.
resource "latitudesh_virtual_network" "lan" {
  count       = var.node_count > 1 ? 1 : 0
  project     = local.project_id
  site        = var.site
  description = "spinifex-${var.deployment_name}"
}

resource "latitudesh_vlan_assignment" "node" {
  count              = var.node_count > 1 ? var.node_count : 0
  server_id          = latitudesh_server.node[count.index].id
  virtual_network_id = latitudesh_virtual_network.lan[0].id
}
