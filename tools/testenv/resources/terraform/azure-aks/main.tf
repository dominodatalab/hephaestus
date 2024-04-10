provider "azurerm" {
  features {}
}

provider "azuread" {
}

resource "random_id" "suffix" {
  byte_length = 8
}

locals {
  name = "testenv-aks-${random_id.suffix.hex}"
}

resource "azurerm_resource_group" "main" {
  location = var.location
  name     = "${local.name}-rg"
}

## AKS

resource "azurerm_virtual_network" "main" {
  address_space       = ["10.0.0.0/16"]
  location            = azurerm_resource_group.main.location
  name                = "${local.name}-vn"
  resource_group_name = azurerm_resource_group.main.name
}

resource "azurerm_subnet" "main" {
  address_prefixes     = ["10.0.0.0/24"]
  name                 = "${local.name}-sn"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
}

module "aks" {
  source  = "Azure/aks/azurerm"
  version = "~> 8.0"

  prefix                          = local.name
  resource_group_name             = azurerm_resource_group.main.name
  log_analytics_workspace_enabled = false
  net_profile_pod_cidr            = "10.52.0.0/16"
  kubernetes_version              = var.kubernetes_version
  rbac_aad                        = false
}

resource "local_file" "kubeconfig" {
  content         = module.aks.kube_config_raw
  filename        = "${path.module}/kubeconfig"
  file_permission = "0600"
}

## Container Registry

resource "azurerm_container_registry" "main" {
  name                = "testenv${random_id.suffix.hex}"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  sku                 = "Basic"
}

## Azure AD

data "azuread_client_config" "current" {}

resource "azuread_application" "app" {
  display_name = local.name
  owners       = [data.azuread_client_config.current.object_id]
}

resource "azuread_service_principal" "app" {
  client_id = azuread_application.app.client_id
  owners    = [data.azuread_client_config.current.object_id]
}

resource "azuread_service_principal_password" "app" {
  service_principal_id = azuread_service_principal.app.id
}

resource "azurerm_role_assignment" "acr" {
  scope                            = azurerm_container_registry.main.id
  role_definition_name             = "AcrPush"
  principal_id                     = azuread_service_principal.app.object_id
  skip_service_principal_aad_check = true
}
