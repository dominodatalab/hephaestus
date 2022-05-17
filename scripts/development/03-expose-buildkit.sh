#!/usr/bin/env bash
#
# Creates services that target individual buildkit pods and maps their ingress
# hostnames so that they mimic the addresses generated from endpoints.

set -e

cat <<EOF | kubectl apply -f -
apiVersion: configuration.konghq.com/v1
kind: KongIngress
metadata:
  name: buildkit-timeout-conf
  annotations:
    kubernetes.io/ingress.class: "kong"
proxy:
  read_timeout: 3600000
  write_timeout: 3600000

---
apiVersion: v1
kind: Service
metadata:
  name: hephaestus-buildkit-0
  namespace: default
  annotations:
    konghq.com/protocol: "grpc"
    konghq.com/override: buildkit-timeout-conf
spec:
  type: ClusterIP
  ports:
    - name: daemon
      port: 1234
      targetPort: 1234
      protocol: TCP
  selector:
    statefulset.kubernetes.io/pod-name: hephaestus-buildkit-0

---
apiVersion: v1
kind: Service
metadata:
  name: hephaestus-buildkit-1
  namespace: default
  annotations:
    konghq.com/protocol: "grpc"
    konghq.com/override: buildkit-timeout-conf
spec:
  type: ClusterIP
  ports:
    - name: daemon
      port: 1234
      targetPort: 1234
      protocol: TCP
  selector:
    statefulset.kubernetes.io/pod-name: hephaestus-buildkit-1

---
apiVersion: v1
kind: Service
metadata:
  name: hephaestus-buildkit-2
  namespace: default
  annotations:
    konghq.com/protocol: "grpc"
    konghq.com/override: buildkit-timeout-conf
spec:
  type: ClusterIP
  ports:
    - name: daemon
      port: 1234
      targetPort: 1234
      protocol: TCP
  selector:
    statefulset.kubernetes.io/pod-name: hephaestus-buildkit-2

---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: buildkitd-ingress
  annotations:
    konghq.com/protocols: "grpc"
spec:
  ingressClassName: kong
  rules:
    - host: hephaestus-buildkit-0.hephaestus-buildkit.default
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: hephaestus-buildkit-0
                port:
                  number: 1234
    - host: hephaestus-buildkit-1.hephaestus-buildkit.default
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: hephaestus-buildkit-1
                port:
                  number: 1234
    - host: hephaestus-buildkit-2.hephaestus-buildkit.default
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: hephaestus-buildkit-2
                port:
                  number: 1234
EOF
