output "kubeconfig" {
  description = "Kubeconfig for eks cluster"
  sensitive   = true
  value       = local.kubeconfig
}

output "repo_url" {
  description = "Repository url for testing artifacts"
  value       = module.ecr.repository_url
}