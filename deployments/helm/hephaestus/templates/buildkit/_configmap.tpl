{{/*
Renders a full buildkit ConfigMap manifest, called once per pool (implicit or explicit) from
configmap.yaml - the buildkitd.toml/subuid/subgid data never varies per pool. A full top-level
document, so callers never nindent it.

The data block is inlined here rather than nindent-included from another template: nindent reindents
every line of its input, including blank ones, which would turn this TOML output's intentional blank
lines into trailing-whitespace-only ones and silently change the checksum/config annotation. Keeping
the literal source at callers' indentation (2 spaces under `data:`) avoids that.

Expects a dict with: root, name, namespace ("" to omit), standardLabels (raw).
*/}}
{{- define "hephaestus.buildkit.configmap.render" -}}
{{- $ctx := . -}}
{{- $root := $ctx.root -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $ctx.name }}
  {{- if $ctx.namespace }}
  namespace: {{ $ctx.namespace }}
  {{- end }}
  labels:
    {{- $ctx.standardLabels | nindent 4 }}
data:
  {{- with $root.Values.buildkit }}
  buildkitd.toml: |
    [grpc]
      address = [ "tcp://0.0.0.0:{{ .service.port }}", "{{ .rootless | ternary (printf "unix:///run/user/%v/buildkit/buildkitd.sock" .rootlessUser) "unix:///run/buildkit/buildkitd.sock" }}" ]

      {{- if .mtls.enabled }}
      [grpc.tls]
        cert = "/etc/buildkit/x509/tls.crt"
        key = "/etc/buildkit/x509/tls.key"
        ca = "/etc/buildkit/x509/ca.crt"
      {{- end }}

    [worker.oci]
      {{- if .rootless }}
      noProcessSandbox = true
      {{- end }}
      {{- if .persistence.enabled }}
      [[worker.oci.gcpolicy]]
        all = true
        keepBytes = {{ .gcKeepStorage | int64 }}
      {{- end }}

    {{- range $domain, $opts := $root.Values.registries }}
    [registry."{{ $domain }}"]
      {{- with $opts.http }}
      http = {{ . }}
      {{- end }}

      {{- with $opts.insecure }}
      insecure = {{ . }}
      {{- end }}
    {{- end }}
  {{- if .rootless }}
  subgid: |
    user:100000:65536
  subuid: |
    user:100000:65536
  {{- end -}}
  {{- end }}
{{- end }}
