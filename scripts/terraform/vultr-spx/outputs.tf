output "deployment_name" {
  value = var.deployment_name
}

output "region" {
  value = var.region
}

output "vpc_id" {
  value = vultr_vpc.lan.id
}

# provider_host_id is what a reserved IP attaches to. The VPC address is read
# on the host, since the provider does not report it.
output "nodes" {
  value = [for n in concat(vultr_instance.node, vultr_bare_metal_server.node) : {
    name             = n.hostname
    provider_host_id = n.id
    public_ip        = n.main_ip
  }]
}

# The addresses the Vultr API key's access control must allow once the
# deployment is up, besides the machine that runs deploy-vultr.sh.
output "api_key_allowlist" {
  value = [for n in concat(vultr_instance.node, vultr_bare_metal_server.node) : "${n.main_ip}/32"]
}
