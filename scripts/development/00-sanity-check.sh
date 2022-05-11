#!/usr/bin/env bash
#
# Asserts that required executables are installed on host.

set -e

commands=("helmfile" "helm" "kubectl" "minikube" "ngrok")
for cmd in "${commands[@]}"; do
  if ! command -v "$cmd" &> /dev/null; then
    echo "'$cmd' command not found, please install"
    exit 1
  fi
done
