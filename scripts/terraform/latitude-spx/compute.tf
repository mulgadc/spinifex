locals {
  hostname_prefix = "spx-${var.deployment_name}-node"
  project         = var.project != "" ? var.project : latitudesh_project.deploy[0].slug
  project_id      = var.project != "" ? var.project_id : latitudesh_project.deploy[0].id
}

# Both lookups fail the plan when a slug does not exist, which is the only check of the OS slug.
data "latitudesh_operating_system" "os" {
  slug = var.operating_system
}

data "latitudesh_plan" "plan" {
  slug = var.plan
}

resource "latitudesh_project" "deploy" {
  count             = var.project == "" ? 1 : 0
  name              = "spinifex-${var.deployment_name}"
  description       = "Spinifex deployment ${var.deployment_name}"
  environment       = "Development"
  provisioning_type = "on_demand"
}

# The key is team scoped. The server login user is ubuntu, not root.
resource "latitudesh_ssh_key" "deploy" {
  name       = "spinifex-${var.deployment_name}"
  public_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
}

# Host preparation only. Latitude substitutes double-brace variables inside user data, so the file
# must hold none. Spinifex is installed over SSH by deploy-latitude.sh.
resource "latitudesh_user_data" "host" {
  description = "spinifex-${var.deployment_name}"
  content     = filebase64("${path.module}/cloud-init/user-data.yaml")
}

# billing must be hourly: the default is monthly, which bills upfront with no refund.
resource "latitudesh_server" "node" {
  count            = var.node_count
  project          = local.project
  hostname         = "${local.hostname_prefix}${count.index + 1}"
  site             = var.site
  plan             = data.latitudesh_plan.plan.slug
  operating_system = data.latitudesh_operating_system.os.slug
  billing          = "hourly"
  ssh_keys         = [latitudesh_ssh_key.deploy.id]
  user_data        = latitudesh_user_data.host.id

  timeouts = {
    create = "60m"
  }
}

# Elastic IPs are bought here and never created or deleted by Spinifex. Each starts on a server, spread
# over the nodes, and server_id is ignored afterwards so a move by Spinifex is not undone by a later apply.
resource "latitudesh_elastic_ip" "pool" {
  count = var.elastic_ip_count
  # The provider documents ID or slug but posts the value as project_id unresolved: a slug gave 404.
  project   = local.project_id
  server_id = latitudesh_server.node[count.index % var.node_count].id

  lifecycle {
    ignore_changes = [server_id]
  }
}

moved {
  from = latitudesh_elastic_ip.node1
  to   = latitudesh_elastic_ip.pool[0]
}
