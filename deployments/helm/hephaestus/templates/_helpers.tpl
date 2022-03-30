{{/*
Return the proper controller image name.
*/}}
{{- define "hephaestus.controller.image" -}}
{{- $imageRoot := .Values.controller.image }}
{{- $_ := set $imageRoot "tag" (.Values.controller.image.tag | default .Chart.AppVersion) }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" .Values.global) }}
{{- end }}

{{/*
Return the proper log processor image name.
*/}}
{{- define "hephaestus.logprocessor.image" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.logProcessor.image "global" .Values.global) }}
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
{{- if .Values.controller.serviceAccount.create }}
{{- default (include "common.names.fullname" .) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Returns a unified list of image pull secrets.
*/}}
{{- define "hephaestus.imagePullSecrets" -}}
{{- include "common.images.pullSecrets" (dict "images" (list .Values.controller.image .Values.logProcessor.image) "global" .Values.global) }}
{{- end }}

{{/*
Returns the logfile directory.
*/}}
{{- define "hephaestus.logfileDir" -}}
/var/log/hephaestus
{{- end }}

{{/*
Returns the logfile pathname.
*/}}
{{- define "hephaestus.logfilePath" -}}
{{ include "hephaestus.logfileDir" . }}/output.json
{{- end }}

{{/*
Return whether or not securityContext.seccompProfile field is supported.
*/}}
{{- define "hephaestus.supportsSeccompGA" -}}
{{- semverCompare ">1.19-0" .Capabilities.KubeVersion.Version }}
{{- end }}

{{/*
Return a name suitable for all manager RBAC objects.
*/}}
{{- define "hephaestus.rbac.managerName" -}}
dominodatalab:operator:{{ include "common.names.fullname" . }}
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
