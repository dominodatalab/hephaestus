{{/*
Renders a full buildkit NetworkPolicy manifest, called once per pool (implicit or explicit) from
networkpolicy.yaml. A full top-level document, so callers never nindent it.

controllerNamespace is "" for the implicit pool (always co-located with the controller, so a bare
podSelector is enough) and the release namespace for every explicit pool: a bare podSelector only
matches peers in the *same* namespace as the policy, so a differently-namespaced pool would
otherwise silently block controller ingress.

Expects a dict with: root, name, namespace ("" to omit), standardLabels (raw), matchLabels (raw),
controllerNamespace ("" to omit the namespaceSelector, otherwise the namespace to match).
*/}}
{{- define "hephaestus.buildkit.networkpolicy.render" -}}
{{- $ctx := . -}}
{{- $root := $ctx.root -}}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $ctx.name }}
  {{- if $ctx.namespace }}
  namespace: {{ $ctx.namespace }}
  {{- end }}
  labels:
    {{- $ctx.standardLabels | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- $ctx.matchLabels | nindent 6 }}
  policyTypes:
    - Ingress
  ingress:
    - ports:
        - port: {{ $root.Values.buildkit.service.port }}
          protocol: TCP
        {{- if $root.Values.istio.ambient }}
        - port: 15008 # Allow Istio Ambient mode's HBONE traffic
          protocol: TCP
        {{- end }}
      from:
        {{- if $ctx.controllerNamespace }}
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: {{ $ctx.controllerNamespace }}
          podSelector:
            matchLabels:
              {{- include "hephaestus.controller.labels.matchLabels" $root | nindent 14 }}
        {{- else }}
        - podSelector:
            matchLabels:
              {{- include "hephaestus.controller.labels.matchLabels" $root | nindent 14 }}
        {{- end }}
{{- end }}
