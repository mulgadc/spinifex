# Demo workload for eks-https-ingress — separate root module / state.
#
# Kept apart from the cluster root module on purpose: the kubernetes provider
# below reads the cluster endpoint from a *live* data source, so it is only ever
# configured while the cluster exists. Destroy this module before the parent and
# the provider never falls back to localhost.
#
# Creates the Spinifex-themed demo Deployment, a NodePort Service, and an Ingress
# (ingressClassName: alb). The AWS Load Balancer Controller reconciles the
# Ingress into an internet-facing ALB that terminates TLS with the imported ACM
# certificate and forwards to the workers' NodePort.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-https-ingress
#   tofu init && tofu apply                    # parent: cluster + LBC + ACM
#   cd workloads && tofu init && tofu apply    # this module: demo app + Ingress
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
  type    = string
  default = "https://127.0.0.1:9999"
}

variable "replicas" {
  type    = number
  default = 2
}

variable "demo_image" {
  type        = string
  default     = ""
  description = "Demo image ref. Defaults to the parent's ECR repository URL at :latest."
}

variable "inbound_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the ALB HTTPS listener"
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
  cert_arn     = data.terraform_remote_state.infra.outputs.certificate_arn
  demo_image   = var.demo_image != "" ? var.demo_image : "${data.terraform_remote_state.infra.outputs.ecr_repository_url}:latest"
}

data "aws_eks_cluster" "this" {
  name = local.cluster_name
}

provider "kubernetes" {
  host                   = data.aws_eks_cluster.this.endpoint
  cluster_ca_certificate = base64decode(data.aws_eks_cluster.this.certificate_authority[0].data)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", local.cluster_name, "--region", local.region]
  }
}

# The Spinifex-themed demo app reports the pod, node, cluster, and region that
# served the request. The pod and node names come from the downward API.
resource "kubernetes_deployment_v1" "demo" {
  metadata {
    name      = "spinifex-demo"
    namespace = "default"
    labels    = { app = "spinifex-demo" }
  }

  spec {
    replicas = var.replicas

    selector {
      match_labels = { app = "spinifex-demo" }
    }

    template {
      metadata {
        labels = { app = "spinifex-demo" }
      }

      spec {
        container {
          name  = "spinifex-demo"
          image = local.demo_image

          port {
            container_port = 8080
          }

          env {
            name = "POD_NAME"
            value_from {
              field_ref {
                field_path = "metadata.name"
              }
            }
          }

          env {
            name = "NODE_NAME"
            value_from {
              field_ref {
                field_path = "spec.nodeName"
              }
            }
          }

          env {
            name = "POD_NAMESPACE"
            value_from {
              field_ref {
                field_path = "metadata.namespace"
              }
            }
          }

          env {
            name  = "CLUSTER_NAME"
            value = local.cluster_name
          }

          env {
            name  = "AWS_REGION"
            value = local.region
          }

          env {
            name  = "APP_TITLE"
            value = "Spinifex EKS — HTTPS Ingress"
          }

          readiness_probe {
            http_get {
              path = "/healthz"
              port = 8080
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }
        }
      }
    }
  }
}

# NodePort Service: the ALB target group registers the workers on this port.
resource "kubernetes_service_v1" "demo" {
  metadata {
    name      = "spinifex-demo"
    namespace = "default"
  }

  spec {
    selector = { app = "spinifex-demo" }
    type     = "NodePort"

    port {
      port        = 80
      target_port = 8080
      node_port   = local.node_port
    }
  }

  depends_on = [kubernetes_deployment_v1.demo]
}

# Ingress reconciled by the AWS Load Balancer Controller into an internet-facing
# ALB. target-type instance registers the workers' NodePort; the ACM cert ARN
# attaches to the HTTPS:443 listener; HTTP is redirected to HTTPS. ELB subnets
# are injected by Spinifex into the alb IngressClassParams, so none are set here.
resource "kubernetes_ingress_v1" "demo" {
  metadata {
    name      = "spinifex-demo"
    namespace = "default"

    annotations = {
      "alb.ingress.kubernetes.io/scheme"           = "internet-facing"
      "alb.ingress.kubernetes.io/target-type"      = "instance"
      "alb.ingress.kubernetes.io/listen-ports"     = "[{\"HTTP\":80},{\"HTTPS\":443}]"
      "alb.ingress.kubernetes.io/certificate-arn"  = local.cert_arn
      "alb.ingress.kubernetes.io/ssl-redirect"     = "443"
      "alb.ingress.kubernetes.io/healthcheck-path" = "/healthz"
      "alb.ingress.kubernetes.io/inbound-cidrs"    = var.inbound_cidr
    }
  }

  spec {
    ingress_class_name = "alb"

    rule {
      http {
        path {
          path      = "/"
          path_type = "Prefix"

          backend {
            service {
              name = kubernetes_service_v1.demo.metadata[0].name
              port {
                number = 80
              }
            }
          }
        }
      }
    }
  }
}

output "ingress_name" {
  value = kubernetes_ingress_v1.demo.metadata[0].name
}
