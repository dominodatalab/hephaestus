resource "random_string" "name_suffix" {
  length  = 5
  lower   = true
  upper   = false
  numeric = true
  special = false
}

locals {
  name = "testenv-gke-${random_string.name_suffix.result}"
}

module "vpc" {
  source  = "terraform-google-modules/network/google"
  version = "~> 5.2"

  network_name = local.name
  project_id   = var.project_id
  description  = "Created by Domino Data Lab testenv tooling"

  subnets = [
    {
      description = "The subnet containing GKE cluster and node pools"

      subnet_name           = "subnet-gke"
      subnet_ip             = "10.10.10.0/24"
      subnet_region         = var.region
      subnet_private_access = "true"
    }
  ]

  secondary_ranges = {
    subnet-gke = [
      {
        range_name    = "service-range"
        ip_cidr_range = "192.168.1.0/24"
      },
      {
        range_name    = "pod-range"
        ip_cidr_range = "192.168.64.0/20"
      }
    ]
  }
}

module "gke" {
  source  = "terraform-google-modules/kubernetes-engine/google"
  version = "~> 23.1"

  name               = local.name
  region             = var.region
  project_id         = var.project_id
  kubernetes_version = var.kubernetes_version

  network    = module.vpc.network_name
  subnetwork = module.vpc.subnets_names[0]

  ip_range_pods     = module.vpc.subnets_secondary_ranges[0][1].range_name
  ip_range_services = module.vpc.subnets_secondary_ranges[0][0].range_name
}

module "gke_auth" {
  source  = "terraform-google-modules/kubernetes-engine/google//modules/auth"
  version = "~> 23.1"

  location     = module.gke.location
  project_id   = var.project_id
  cluster_name = local.name
}

resource "local_file" "kubeconfig" {
  content  = module.gke_auth.kubeconfig_raw
  filename = "${path.module}/kubeconfig"
}
