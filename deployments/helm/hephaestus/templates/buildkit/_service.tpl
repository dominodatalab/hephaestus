{{/*
Renders a full buildkit Service manifest, called once per pool (implicit or explicit) from
service.yaml. A full top-level document, so callers never nindent it.

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
