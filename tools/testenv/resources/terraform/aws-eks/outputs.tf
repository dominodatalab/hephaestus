output "kubeconfig_path" {
  description = "Kubeconfig path for the eks cluster"
  sensitive   = true
  value       = var.kubeconfig_path
}

output "repo_url" {
  description = "Repository url for testing artifacts"
  value       = module.ecr.repository_url
}
