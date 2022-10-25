output "kubeconfig" {
  description = "Kubeconfig for eks cluster"
  sensitive = true
  value       = local.kubeconfig
}