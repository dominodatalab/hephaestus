output "kubeconfig" {
  description = "A kubeconfig file configured to access the GKE cluster."
  value       = module.gke_auth.kubeconfig_raw
  sensitive   = true
}
