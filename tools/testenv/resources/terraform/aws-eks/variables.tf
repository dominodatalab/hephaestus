variable "region" {
  type        = string
  description = "The region in which the cluster is hosted."
}

variable "kubernetes_version" {
  type        = string
  description = "The Kubernetes version."
  default     = "1.22"
}


variable "kubeconfig_path" {
  type        = string
  description = "Path to the kubeconfig file."
  default     = "./kubeconfig"
}
