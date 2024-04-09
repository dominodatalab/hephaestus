output "tenant_id" {
  description = "The tenant ID of the AAD service principal created for testing."
  value       = azuread_service_principal.app.application_tenant_id
}

output "client_id" {
  description = "The client ID of the AAD service principal created for testing."
  value       = azuread_service_principal.app.application_id
}

output "client_secret" {
  description = "The client secret of the AAD service principal created for testing."
  value       = azuread_service_principal_password.app.value
  sensitive   = true
}

output "kubeconfig" {
  description = "A kubeconfig file configured to access the AKS cluster."
  value       = module.aks.kube_config_raw
  sensitive   = true
}

output "repository" {
  description = "The name of the ACR registry created for testing."
  value       = azurerm_container_registry.main.login_server
}
