{{/*
Return the proper image name.
*/}}
{{- define "hephaestus.image" -}}
{{- $imageRoot := .Values.image }}
{{- $_ := set $imageRoot "tag" (.Values.image.tag | default .Chart.AppVersion) }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" $) }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "hephaestus.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Returns a name suitable for all manager RBAC objects.
*/}}
{{- define "hephaestus.rbac.managerName" -}}
dominodatalab:operator:{{ include "common.names.fullname" . }}
{{- end }}


{{/*
Return the webhook certificate CA name.
*/}}
{{- define "hephaestus.webhook.issuer" -}}
{{ include "common.names.fullname" . }}-selfsigned-issuer
{{- end }}

{{/*
Return the webhook certificate name.
*/}}
{{- define "hephaestus.webhook.certificate" -}}
{{ include "common.names.fullname" . }}-webhook
{{- end -}}

{{/*
Return the webhook annotations.
*/}}
{{- define "hephaestus.webhook.annotations" -}}
cert-manager.io/inject-ca-from: {{ .Release.Namespace }}/{{ include "hephaestus.webhook.certificate" . }}
{{- end }}

{{/*
Return the webhook service name.
*/}}
{{- define "hephaestus.webhook.service" -}}
{{ include "common.names.fullname" . }}-webhook-server
{{- end }}

{{/*
Return the webhook certificate secret name.
*/}}
{{- define "hephaestus.webhook.secret" -}}
{{ include "common.names.fullname" . }}-webhook-cert
{{- end -}}
