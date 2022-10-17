locals {
  name        = "testenv-gke-${random_string.name_suffix.result}"
  subnet_name = "subnet-gke-${random_string.name_suffix.result}"

  pod_range_name     = "pod-range"
  service_range_name = "service-range"
}

resource "random_string" "name_suffix" {
  length  = 5
  lower   = true
  upper   = false
  numeric = true
  special = false
}

resource "google_compute_network" "main" {
  name        = local.name
  project     = var.project_id
  description = "Created by Domino Data Lab testenv tooling"

  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "kubernetes" {
  name        = local.subnet_name
  project     = var.project_id
  region      = var.region
  network     = google_compute_network.main.name
  description = "The subnet containing GKE cluster and node pools"

  ip_cidr_range            = "10.10.10.0/24"
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = local.service_range_name
    ip_cidr_range = "192.168.1.0/24"
  }

  secondary_ip_range {
    range_name    = local.pod_range_name
    ip_cidr_range = "192.168.64.0/20"
  }
}

module "gke" {
  source  = "terraform-google-modules/kubernetes-engine/google"
  version = "~> 23.2"

  name               = local.name
  region             = var.region
  project_id         = var.project_id
  kubernetes_version = var.kubernetes_version

  network    = google_compute_network.main.name
  subnetwork = google_compute_subnetwork.kubernetes.name

  ip_range_pods     = local.pod_range_name
  ip_range_services = local.service_range_name
}

module "gke_auth" {
  source  = "terraform-google-modules/kubernetes-engine/google//modules/auth"
  version = "~> 23.2"

  location     = module.gke.location
  project_id   = var.project_id
  cluster_name = local.name
}

resource "local_file" "kubeconfig" {
  content         = module.gke_auth.kubeconfig_raw
  filename        = "${path.module}/kubeconfig"
  file_permission = "0600"
}

resource "google_artifact_registry_repository" "repo" {
  project       = var.project_id
  location      = var.region
  format        = "DOCKER"
  repository_id = local.name
  description   = "Created by Domino Data Lab testenv tooling"
}

resource "google_service_account" "external_agent" {
  project      = var.project_id
  account_id   = local.name
  display_name = local.name
  description  = "Created by Domino Data Lab testenv tooling"
}

resource "google_service_account_iam_binding" "external_agent_k8s_sa_binding" {
  role               = "roles/iam.workloadIdentityUser"
  service_account_id = google_service_account.external_agent.name

  members = [
    "serviceAccount:${var.project_id}.svc.id.goog[${var.kubernetes_service_account}]"
  ]
}

resource "google_project_iam_member" "external_agent_gar_rw" {
  project = var.project_id
  member  = "serviceAccount:${google_service_account.external_agent.email}"
  role    = "roles/artifactregistry.writer"
}
