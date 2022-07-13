#!/usr/bin/env bash
#
# Install required kubernetes applications and CRDs.
#
# Patch hephaestus webhook resources to route requests to the host process.

set -e

helmfile --file scripts/development/helmfile.yaml sync

make apply

kubectl patch service hephaestus-webhook-server --type merge --patch '{
  "spec": {
    "clusterIP": null,
    "externalName": "host.minikube.internal",
    "type": "ExternalName"
  }
}'

kubectl patch mutatingwebhookconfigurations hephaestus --patch '{
  "webhooks": [
    {
      "name": "mutate-imagebuild.hephaestus.dominodatalab.com",
      "clientConfig": {
        "service": {
          "port": 9443
        }
      }
    }
  ]
}'

kubectl patch validatingwebhookconfigurations hephaestus --patch '{
  "webhooks": [
    {
      "name": "validate-imagebuild.hephaestus.dominodatalab.com",
      "clientConfig": {
        "service": {
          "port": 9443
        }
      }
    }
  ]
}'
