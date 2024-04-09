output "kubeconfig" {
  description = "A kubeconfig file configured to access the EKS cluster."
  sensitive   = true
  value       = data.local_file.kubeconfig.content
}

output "repository" {
  description = "Repository url for testing artifacts"
  value       = module.ecr.repository_url
}
