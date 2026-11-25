# What ebs_provider decides, planned against a mocked oci provider:
# terraform init && terraform test, from scripts/terraform/oci-spx.
# It grants the ocibsd volume rights and nothing else; a node's own data
# volume attaches the same way whatever the provider or shape.

mock_provider "oci" {}

mock_provider "oci" {
  alias = "home"
}

override_data {
  target = data.oci_identity_availability_domains.available
  values = { availability_domains = [{ name = "AD-1" }] }
}

override_data {
  target = data.oci_core_images.ubuntu_2604
  values = { images = [{ id = "ocid1.image.oc1..test" }] }
}

override_data {
  target = data.oci_identity_fault_domains.available
  values = { fault_domains = [{ name = "FAULT-DOMAIN-1" }] }
}

override_data {
  target = data.oci_core_services.oracle_services_network
  values = { services = [{ id = "ocid1.service.oc1..test", name = "All Services In Oracle Services Network", cidr_block = "all-services" }] }
}

variables {
  tenancy_ocid        = "ocid1.tenancy.oc1..test"
  user_ocid           = "ocid1.user.oc1..test"
  fingerprint         = "00:00"
  compartment_ocid    = "ocid1.compartment.oc1..test"
  ssh_public_key_path = "tests/test-key.pub"
  private_key         = "not a key: the provider is mocked"
}

run "ebs_provider_defaults_to_ocibsd_grants" {
  command = plan

  assert {
    condition     = output.ebs_provider == "ocibsd"
    error_message = "ebs_provider must default to ocibsd, which spx admin init detects on any OCI instance"
  }
  assert {
    condition = alltrue([
      for grant in ["manage volume-family", "use instances"] :
      anytrue([for s in output.api_key_policy_statements : strcontains(s, " to ${grant} in ")])
    ])
    error_message = "the default policy must carry the ocibsd volume grants"
  }
}

run "viperblockd_drops_only_the_volume_grants" {
  command = plan

  variables {
    ebs_provider = "viperblockd"
  }

  assert {
    condition     = !anytrue([for s in output.api_key_policy_statements : strcontains(s, "volume-family")])
    error_message = "a viperblockd deployment is granted no OCI volume"
  }
}

run "data_volume_attaches_over_iscsi_with_agent_login" {
  command = plan

  variables {
    ebs_provider  = "ocibsd"
    compute_shape = "BM.Standard.E2.64"
  }

  assert {
    condition     = oci_core_volume_attachment.mulgadc_data[0].attachment_type == "iscsi" && oci_core_volume_attachment.mulgadc_data[0].is_agent_auto_iscsi_login_enabled
    error_message = "bare metal attaches only over iSCSI, with the agent logging the data volume in"
  }
  assert {
    condition     = one([for p in oci_core_instance.mulgadc[0].agent_config[0].plugins_config : p.desired_state if p.name == "Block Volume Management"]) == "ENABLED"
    error_message = "the Block Volume Management plugin must run to log the data volume in"
  }
}

run "viperblockd_vm_attaches_its_data_volume_identically" {
  command = plan

  variables {
    ebs_provider = "viperblockd"
  }

  assert {
    condition     = oci_core_volume_attachment.mulgadc_data[0].attachment_type == "iscsi" && oci_core_volume_attachment.mulgadc_data[0].is_agent_auto_iscsi_login_enabled
    error_message = "ebs_provider must not change how the data volume attaches"
  }
  assert {
    condition     = one([for p in oci_core_instance.mulgadc[0].agent_config[0].plugins_config : p.desired_state if p.name == "Block Volume Management"]) == "ENABLED"
    error_message = "ebs_provider must not change the Block Volume Management plugin"
  }
}

run "ocibsd_vm_attaches_its_data_volume_identically" {
  command = plan

  variables {
    ebs_provider = "ocibsd"
  }

  assert {
    condition     = oci_core_volume_attachment.mulgadc_data[0].attachment_type == "iscsi" && oci_core_volume_attachment.mulgadc_data[0].is_agent_auto_iscsi_login_enabled
    error_message = "an ocibsd VM must attach its data volume as a viperblockd one does"
  }
  assert {
    condition     = one([for p in oci_core_instance.mulgadc[0].agent_config[0].plugins_config : p.desired_state if p.name == "Block Volume Management"]) == "ENABLED"
    error_message = "an ocibsd VM must run the plugin as a viperblockd one does"
  }
}
