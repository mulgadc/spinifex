# The key comes from VULTR_API_KEY in the environment, never a variable, so it
# is not written to the state or to a tfvars file.
provider "vultr" {
  rate_limit  = 700
  retry_limit = 3
}
