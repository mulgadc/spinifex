# Demo workload for eks-quickstart — separate root module / state.
#
# Kept apart from the cluster root module on purpose: the kubernetes provider
# below reads the cluster endpoint from a *live* data source, so it is only ever
# configured while the cluster exists. Destroy this module before the parent and
# the provider never falls back to localhost.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-quickstart
#   tofu init && tofu apply                    # parent: cluster + infra
#   cd workloads && tofu init && tofu apply    # this module: demo app
#   # teardown is the reverse — destroy here first, then the parent.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40, < 6.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.20"
    }
  }
}

variable "spinifex_endpoint" {
  type        = string
  default     = "https://127.0.0.1:9999"
  description = "Spinifex AWS gateway endpoint"
}

variable "replicas" {
  type        = number
  default     = 2
  description = "Demo app replicas; refresh the page to see requests land on different pods"
}

provider "aws" {
  region = data.terraform_remote_state.infra.outputs.region

  endpoints {
    ec2 = var.spinifex_endpoint
    iam = var.spinifex_endpoint
    sts = var.spinifex_endpoint
    eks = var.spinifex_endpoint
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
}

# Cluster identity comes from the parent module's state; the live endpoint/CA
# come from a data source so the provider is only configured while the cluster
# is up.
data "terraform_remote_state" "infra" {
  backend = "local"

  config = {
    path = "../terraform.tfstate"
  }
}

locals {
  cluster_name = data.terraform_remote_state.infra.outputs.cluster_name
  region       = data.terraform_remote_state.infra.outputs.region
  node_port    = data.terraform_remote_state.infra.outputs.node_port
}

data "aws_eks_cluster" "this" {
  name = local.cluster_name
}

# Authenticates with the same `aws eks get-token` exec flow the generated
# kubeconfig uses, so the Kubernetes provider can deploy the demo app.
provider "kubernetes" {
  host                   = data.aws_eks_cluster.this.endpoint
  cluster_ca_certificate = base64decode(data.aws_eks_cluster.this.certificate_authority[0].data)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", local.cluster_name, "--region", local.region]
  }
}

# nginxdemos/hello renders a page showing which pod served the request; with
# multiple replicas, refreshing the demo_url alternates between them.
resource "kubernetes_deployment_v1" "hello" {
  metadata {
    name      = "hello"
    namespace = "default"
    labels    = { app = "hello" }
  }

  spec {
    replicas = var.replicas

    selector {
      match_labels = { app = "hello" }
    }

    template {
      metadata {
        labels = { app = "hello" }
      }

      spec {
        container {
          name  = "hello"
          image = "nginxdemos/hello"

          port {
            container_port = 80
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "hello" {
  metadata {
    name      = "hello"
    namespace = "default"
  }

  spec {
    selector = { app = "hello" }
    type     = "NodePort"

    port {
      port        = 80
      target_port = 80
      node_port   = local.node_port
    }
  }

  depends_on = [kubernetes_deployment_v1.hello]
}
