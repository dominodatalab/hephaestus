output "kubeconfig" {
  description = "A kubeconfig file configured to access the EKS cluster."
  sensitive   = true
  value       = local_file.kubeconfig
}

output "repository" {
  description = "Repository url for testing artifacts"
  value       = module.ecr.repository_url
}
