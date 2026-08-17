{{/*
Renders a full buildkit ServiceAccount manifest, shared by serviceaccount.yaml (legacy path) and
pool-serviceaccount.yaml (platformPools path). Rendered as a full top-level document so callers never
need to nindent this template's own output.

Expects a dict with: root, name, namespace ("" to omit), standardLabels (raw).
*/}}
{{- define "hephaestus.buildkit.serviceaccount.render" -}}
{{- $ctx := . -}}
{{- $root := $ctx.root -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ $ctx.name }}
  {{- if $ctx.namespace }}
  namespace: {{ $ctx.namespace }}
  {{- end }}
  labels:
    {{- $ctx.standardLabels | nindent 4 }}
  {{- with $root.Values.buildkit.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: false
{{- end }}
