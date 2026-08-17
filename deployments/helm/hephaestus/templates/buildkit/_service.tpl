{{/*
Renders a full buildkit Service manifest, shared by service.yaml (legacy path) and pool-service.yaml
(platformPools path). Rendered as a full top-level document so callers never need to nindent this
template's own output.

Expects a dict with: root, name, namespace ("" to omit), standardLabels (raw), matchLabels (raw).
*/}}
{{- define "hephaestus.buildkit.service.render" -}}
{{- $ctx := . -}}
{{- $root := $ctx.root -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ $ctx.name }}
  {{- if $ctx.namespace }}
  namespace: {{ $ctx.namespace }}
  {{- end }}
  labels:
    {{- $ctx.standardLabels | nindent 4 }}
spec:
  clusterIP: None
  {{- with $root.Values.buildkit }}
  type: {{ .service.type }}
  ports:
    - name: {{ .service.portName }}
      port: {{ .service.port }}
      targetPort: {{ .service.portName }}
      protocol: TCP
  {{- end }}
  selector:
    {{- $ctx.matchLabels | nindent 4 }}
{{- end }}
