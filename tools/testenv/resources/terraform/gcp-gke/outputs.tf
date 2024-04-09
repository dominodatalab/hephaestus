output "kubeconfig" {
  description = "A kubeconfig file configured to access the GKE cluster."
  value       = module.gke_auth.kubeconfig_raw
  sensitive   = true
}

output "repository" {
  description = "The name of a GAR repository created for testing."
  value       = google_artifact_registry_repository.repo.name
}

output "service_account" {
  description = "The email of a service account that has RW permissions to the test GAR repository."
  value       = google_service_account.external_agent.email
}
