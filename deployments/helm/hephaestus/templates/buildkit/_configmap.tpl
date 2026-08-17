{{/*
Renders a full buildkit ConfigMap manifest, shared by configmap.yaml (legacy path) and
pool-configmap.yaml (platformPools path) - the buildkitd.toml/subuid/subgid data never varies per
pool. Rendered as a full top-level document so callers never need to nindent this template's own
output.

The data block is deliberately inlined here rather than nindent-included from yet another shared
template: both callers need it at the same fixed indentation (2 spaces under `data:`), which this
template's own literal source already carries, verbatim. Passing it through `nindent` at a call site
was tried previously and reverted (see docs/multi-arch-image-builds-design.md, "the nindent/checksum
trap") - nindent unconditionally reindents every line of its input including blank ones, turning the
intentional blank lines in this TOML output into trailing-whitespace-only lines and silently changing
the checksum/config annotation. Matching indentation at the source instead of the call site sidesteps
that trap rather than working around it.

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
