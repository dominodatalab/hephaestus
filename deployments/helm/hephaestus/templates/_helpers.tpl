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
