{{/*
Return the proper image name.
*/}}
{{- define "buildkit.image" -}}
{{- $imageRoot := .Values.image }}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- if not .Values.rootless }}
{{- $tag = trimSuffix "-rootless" $tag }}
{{- end }}
{{- $_ := set $imageRoot "tag" $tag }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" $) }}
{{- end }}

{{/*
Return the service account name.
*/}}
{{- define "buildkit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Return the mtls server secret name
*/}}
{{- define "buildkit.mTLSSecret" -}}
{{ printf "%s-server-tls" (include "common.names.fullname" .) }}
{{- end }}
