{{/*
Return the proper image name.
*/}}
{{- define "buildkitd.image" -}}
{{- $imageRoot := .Values.image }}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- if not .Values.rootless }}
{{- $tag = trimSuffix "-rootless" $tag }}
{{- end }}
{{- $_ := set $imageRoot "tag" $tag }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" $) }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "buildkitd.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
