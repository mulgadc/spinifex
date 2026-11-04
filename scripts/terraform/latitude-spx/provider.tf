# The token comes from LATITUDESH_AUTH_TOKEN in the environment, which the provider reads
# itself (its auth_token argument documents that variable). It is never a variable here, so it
# is not written to the state or to a tfvars file. The default project is set per resource.
provider "latitudesh" {}
