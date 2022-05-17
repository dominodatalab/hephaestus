#!/usr/bin/env bash
#
# Export controller config and webhook TLS files onto the host.

set -e

dev_dir=local-development
mkdir -p $dev_dir
kubectl get secrets hephaestus-config -ojsonpath='{.data.config\.yaml}' | base64 -d > $dev_dir/hephaestus.yaml

cert_dir=$dev_dir/webhook-certs
mkdir -p $cert_dir
kubectl get secrets hephaestus-webhook-tls -ojsonpath='{.data.ca\.crt}' | base64 -d > $cert_dir/ca.crt
kubectl get secrets hephaestus-webhook-tls -ojsonpath='{.data.tls\.crt}' | base64 -d > $cert_dir/tls.crt
kubectl get secrets hephaestus-webhook-tls -ojsonpath='{.data.tls\.key}' | base64 -d > $cert_dir/tls.key
