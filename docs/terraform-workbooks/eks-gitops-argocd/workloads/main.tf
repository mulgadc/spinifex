# GitOps workload for eks-gitops-argocd — separate root module / state.
#
# Instead of applying the app directly, this module hands delivery to Argo CD:
# it registers the (private) git repo as an Argo CD repository credential and
# creates an Argo CD Application that syncs the demo app's manifests from it. The
# app's Deployment, Service, and PersistentVolumeClaim live in the git repo (see
# ../../../../eks-demo-app); the HTTPS Ingress (LBC + ACM) stays here and points
# at the Service that Argo CD creates.
#
# The Argo CD Application is a kubernetes_manifest, so the argoproj.io CRDs must
# already exist — apply the parent module (which installs the argocd addon) and
# let it reach ACTIVE before applying this module.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-gitops-argocd
#   tofu init && tofu apply                       # parent: cluster + addons + ACM
#   cd workloads && tofu init
#   tofu apply -var git_repo_url=https://github.com/mulgadc/eks-demo-app.git \
#              -var git_token=<a-read-only-PAT>
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

variable "git_repo_url" {
  type        = string
  default     = "https://github.com/mulgadc/eks-demo-app.git"
  description = "Git repo Argo CD syncs the demo app from"
}

variable "git_revision" {
  type        = string
  default     = "main"
  description = "Git branch, tag, or commit Argo CD tracks"
}

variable "git_path" {
  type        = string
  default     = "manifests"
  description = "Path within the repo holding the app manifests"
}

variable "git_username" {
  type        = string
  default     = "git"
  description = "Username for the git credential (any non-empty value for a PAT)"
}

variable "git_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Read-only personal access token for the private repo. Leave empty for a public repo."
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

data "terraform_remote_state" "infra" {
  backend = "local"

  config = {
    path = "../terraform.tfstate"
  }
}

locals {
  cluster_name     = data.terraform_remote_state.infra.outputs.cluster_name
  region           = data.terraform_remote_state.infra.outputs.region
  cert_arn         = data.terraform_remote_state.infra.outputs.certificate_arn
  argocd_node_port = data.terraform_remote_state.infra.outputs.argocd_node_port
  cert_cn          = data.terraform_remote_state.infra.outputs.cert_common_name
  private_repo     = var.git_token != ""

  # The demo app and the Argo CD UI share one ALB via an LBC IngressGroup; LBC
  # gives each backend its own target group and host-routes between them.
  alb_group   = local.cluster_name
  app_host    = "app.${local.cert_cn}"
  argocd_host = "argocd.${local.cert_cn}"
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

# Repository credential for the private repo. Argo CD picks up Secrets in its
# namespace labelled argocd.argoproj.io/secret-type=repository. Created only when
# a token is supplied (a public repo needs no credential).
resource "kubernetes_secret_v1" "repo" {
  count = local.private_repo ? 1 : 0

  metadata {
    name      = "eks-demo-app-repo"
    namespace = "argocd"
    labels = {
      "argocd.argoproj.io/secret-type" = "repository"
    }
  }

  data = {
    type     = "git"
    url      = var.git_repo_url
    username = var.git_username
    password = var.git_token
  }
}

# Argo CD Application: syncs the demo app from the git repo into the default
# namespace, self-healing and pruning so the cluster tracks the repo.
resource "kubernetes_manifest" "demo_app" {
  manifest = {
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "Application"

    metadata = {
      name      = "spinifex-demo"
      namespace = "argocd"
    }

    spec = {
      project = "default"

      source = {
        repoURL        = var.git_repo_url
        targetRevision = var.git_revision
        path           = var.git_path
      }

      destination = {
        server    = "https://kubernetes.default.svc"
        namespace = "default"
      }

      syncPolicy = {
        automated = {
          prune    = true
          selfHeal = true
        }
        syncOptions = ["CreateNamespace=true"]
      }
    }
  }

  depends_on = [kubernetes_secret_v1.repo]
}

# HTTPS Ingress (LBC + ACM), carried over from eks-https-ingress. It points at
# the spinifex-demo Service that Argo CD creates from the git manifests.
resource "kubernetes_ingress_v1" "demo" {
  metadata {
    name      = "spinifex-demo"
    namespace = "default"

    annotations = {
      "alb.ingress.kubernetes.io/scheme"           = "internet-facing"
      "alb.ingress.kubernetes.io/target-type"      = "instance"
      "alb.ingress.kubernetes.io/group.name"       = local.alb_group
      "alb.ingress.kubernetes.io/listen-ports"     = "[{\"HTTP\":80},{\"HTTPS\":443}]"
      "alb.ingress.kubernetes.io/certificate-arn"  = local.cert_arn
      "alb.ingress.kubernetes.io/ssl-redirect"     = "443"
      "alb.ingress.kubernetes.io/healthcheck-path" = "/healthz"
    }
  }

  spec {
    ingress_class_name = "alb"

    rule {
      host = local.app_host

      http {
        path {
          path      = "/"
          path_type = "Prefix"

          backend {
            service {
              name = "spinifex-demo"
              port {
                number = 80
              }
            }
          }
        }
      }
    }
  }

  depends_on = [kubernetes_manifest.demo_app]
}

# ---------------------------------------------------------------------------
# Argo CD UI exposure — managing the cluster through the GitOps console is the
# point of this demo, so the UI gets the same HTTPS-Ingress treatment as the app
# rather than a port-forward. The argocd addon ships argocd-server as a ClusterIP
# only; expose it via a NodePort the ALB targets. argocd-server serves TLS on
# 8080 (no --insecure), so the ALB speaks HTTPS to the backend.
#
# Same group.name as the demo Ingress, so LBC folds both onto ONE ALB with two
# target groups, host-routing app.<cn> to the app and argocd.<cn> to the UI.
# ---------------------------------------------------------------------------

resource "kubernetes_service_v1" "argocd_server_nodeport" {
  metadata {
    name      = "argocd-server-nodeport"
    namespace = "argocd"
  }

  spec {
    type     = "NodePort"
    selector = { "app.kubernetes.io/name" = "argocd-server" }

    port {
      port        = 443
      target_port = 8080
      node_port   = local.argocd_node_port
      protocol    = "TCP"
    }
  }
}

resource "kubernetes_ingress_v1" "argocd" {
  metadata {
    name      = "argocd-server"
    namespace = "argocd"

    annotations = {
      "alb.ingress.kubernetes.io/scheme"               = "internet-facing"
      "alb.ingress.kubernetes.io/target-type"          = "instance"
      "alb.ingress.kubernetes.io/group.name"           = local.alb_group
      "alb.ingress.kubernetes.io/listen-ports"         = "[{\"HTTPS\":443}]"
      "alb.ingress.kubernetes.io/certificate-arn"      = local.cert_arn
      "alb.ingress.kubernetes.io/backend-protocol"     = "HTTPS"
      "alb.ingress.kubernetes.io/healthcheck-protocol" = "HTTPS"
      "alb.ingress.kubernetes.io/healthcheck-path"     = "/healthz"
    }
  }

  spec {
    ingress_class_name = "alb"

    rule {
      host = local.argocd_host

      http {
        path {
          path      = "/"
          path_type = "Prefix"

          backend {
            service {
              name = kubernetes_service_v1.argocd_server_nodeport.metadata[0].name
              port {
                number = 443
              }
            }
          }
        }
      }
    }
  }
}

output "application_name" {
  value = kubernetes_manifest.demo_app.manifest.metadata.name
}

output "alb_address_hint" {
  value = "Both share one ALB: kubectl get ingress spinifex-demo -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{\"\\n\"}'. App host: ${local.app_host}. Argo CD host: ${local.argocd_host}."
}

output "argocd_url_hint" {
  value = "Open https://${local.argocd_host} (admin password: kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d). Resolve the host to the ALB address (northstar/CNAME), or curl -k --resolve ${local.argocd_host}:443:<alb-ip>."
}
