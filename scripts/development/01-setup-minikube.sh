#!/usr/bin/env bash
#
# Create minikube cluster with adequate resources and extended port range and
# enable required/useful addons.
#
# Patch kong ingress controller to expose buildkit port on host.

set -e

minikube start \
  --extra-config=apiserver.service-node-port-range=1-65535 \
  --cpus=4 \
  --memory=16g \
  --disk-size=100g \
  --driver=hyperkit \
  --wait=true \
  --kubernetes-version=1.21.12

minikube addons enable kong
minikube addons enable ingress-dns
minikube addons enable metrics-server

cat <<EOF | sudo tee /etc/resolver/minikube-default
domain default
nameserver $(minikube ip)
search_order 1
timeout 5
EOF

kubectl patch deploy -n kong ingress-kong --patch '{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "proxy",
            "env": [
              {
                "name": "KONG_PROXY_LISTEN",
                "value": "0.0.0.0:8000, 0.0.0.0:8443 ssl http2, 0.0.0.0:1234 http2"
              }
            ],
            "ports": [
              {
                "containerPort": 1234,
                "name": "buildkit",
                "protocol": "TCP"
              }
            ]
          }
        ]
      }
    }
  }
}'

kubectl patch service -n kong kong-proxy --patch '{
  "spec": {
    "ports": [
      {
        "name": "buildkit1234",
        "nodePort": 1234,
        "port": 1234,
        "protocol": "TCP",
        "targetPort": 1234
      }
    ]
  }
}'
