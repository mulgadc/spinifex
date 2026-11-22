# Plans the module against a mocked provider: no credentials and no network are used.
# Developer example: tofu init -backend=false && tofu test

mock_provider "latitudesh" {
  mock_resource "latitudesh_user_data" {
    defaults = { id = "ud_mock" }
  }
}

variables {
  deployment_name     = "t1"
  node_count          = 1
  ssh_public_key_path = "testdata/key.pub"
}

run "default_buys_one_address" {
  command = plan

  assert {
    condition     = length(latitudesh_elastic_ip.pool) == 1
    error_message = "the default must buy one Elastic IP"
  }
}

run "zero_with_an_existing_project_buys_none" {
  command = plan

  variables {
    elastic_ip_count = 0
    project          = "existing"
    project_id       = "proj_existing"
  }

  assert {
    condition     = length(latitudesh_elastic_ip.pool) == 0
    error_message = "a count of 0 must plan no Elastic IP"
  }

  assert {
    condition     = length(latitudesh_project.deploy) == 0
    error_message = "an existing project must not be created"
  }

  assert {
    condition     = output.elastic_ip == null && length(output.elastic_ips) == 0
    error_message = "the outputs must be empty when no address is bought"
  }
}

run "zero_without_a_project_is_refused" {
  command = plan

  variables {
    elastic_ip_count = 0
  }

  expect_failures = [latitudesh_ssh_key.deploy]
}

run "zero_with_a_project_but_no_id_is_refused" {
  command = plan

  variables {
    elastic_ip_count = 0
    project          = "existing"
  }

  expect_failures = [latitudesh_ssh_key.deploy]
}

run "zero_with_a_null_project_id_is_refused" {
  command = plan

  variables {
    elastic_ip_count = 0
    project          = "existing"
    project_id       = null
  }

  expect_failures = [latitudesh_ssh_key.deploy]
}

run "zero_with_a_null_project_is_refused" {
  command = plan

  variables {
    elastic_ip_count = 0
    project          = null
    project_id       = "proj_existing"
  }

  expect_failures = [latitudesh_ssh_key.deploy]
}

run "six_is_refused" {
  command = plan

  variables {
    elastic_ip_count = 6
  }

  expect_failures = [var.elastic_ip_count]
}
