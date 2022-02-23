{{/*
Return the proper image name.
*/}}
{{- define "hephaestus.image" -}}
{{- $imageRoot := .Values.image }}
{{- $_ := set $imageRoot "tag" (.Values.image.tag | default .Chart.AppVersion) }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" $) }}
{{- end }}

{{/*
Return config secret name.
*/}}
{{- define "hephaestus.configSecretName" -}}
{{ include "common.names.fullname" . }}-config
{{- end }}

{{/*
Return the service account name.
*/}}
{{- define "hephaestus.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Return a name suitable for all manager RBAC objects.
*/}}
{{- define "hephaestus.rbac.managerName" -}}
dominodatalab:operator:{{ include "common.names.fullname" . }}
{{- end }}

{{/*
Return whether or not securityContext.seccompProfile field is supported.
*/}}
{{- define "hephaestus.supportsSeccompGA" -}}
{{- semverCompare ">1.19-0" .Capabilities.KubeVersion.Version }}
{{- end }}

{{/*
Return the self-signed issuer name.
*/}}
{{- define "hephaestus.certmanager.selfSignedIssuer" -}}
{{ include "common.names.fullname" . }}-selfsigned-issuer
{{- end }}

{{/*
Return the self-signed CA certificate name.
*/}}
{{- define "hephaestus.certmanager.selfSignedCA" -}}
{{ include "common.names.fullname" . }}-selfsigned-ca
{{- end }}

{{/*
Return the root CA secret name.
*/}}
{{- define "hephaestus.certmanager.rootTLS" -}}
{{ include "common.names.fullname" . }}-root-tls
{{- end }}

{{/*
Return the CA issuer name.
*/}}
{{- define "hephaestus.certmanager.caIssuer" -}}
{{ include "common.names.fullname" . }}-ca-issuer
{{- end }}

{{/*
Return the buildkit mtls client secret name.
*/}}
{{- define "hephaestus.buildkit.clientSecret" -}}
{{ include "common.names.fullname" . }}-buildkit-client-tls
{{- end }}

{{/*
Return the buildkit mtls server secret name.

Leverage the buildkit subcharts template to
create a secret with the name it's expecting.
*/}}
{{- define "hephaestus.buildkit.serverSecret" -}}
{{ include "buildkit.mTLSSecret" .Subcharts.buildkit }}
{{- end }}

{{/*
Return the webhook certificate name.
*/}}
{{- define "hephaestus.webhook.certificate" -}}
{{ include "common.names.fullname" . }}-webhook
{{- end -}}

{{/*
Return the webhook certificate secret name.
*/}}
{{- define "hephaestus.webhook.secret" -}}
{{ include "common.names.fullname" . }}-webhook-tls
{{- end -}}

{{/*
Return the webhook service name.
*/}}
{{- define "hephaestus.webhook.service" -}}
{{ include "common.names.fullname" . }}-webhook-server
{{- end }}

{{/*
Return the webhook annotations.
*/}}
{{- define "hephaestus.webhook.annotations" -}}
cert-manager.io/inject-ca-from: {{ .Release.Namespace }}/{{ include "hephaestus.webhook.certificate" . }}
{{- end }}
