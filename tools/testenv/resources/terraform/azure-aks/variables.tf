variable "location" {
  type        = string
  description = "The location in which the resources will be created."
  default     = "westus2"
}

variable "kubernetes_version" {
  type        = string
  description = "The Kubernetes version of the cluster."
  default     = null
}
