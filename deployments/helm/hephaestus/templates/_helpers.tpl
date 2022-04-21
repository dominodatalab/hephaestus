{{/*
Return the controller image name.
*/}}
{{- define "hephaestus.controller.image" -}}
{{- $imageRoot := .Values.controller.image }}
{{- $_ := set $imageRoot "tag" (.Values.controller.image.tag | default .Chart.AppVersion) }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" .Values.global) }}
{{- end }}

{{/*
Return the controller standard labels.
*/}}
{{- define "hephaestus.controller.labels.standard" -}}
{{- include "common.labels.standard" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Return the controller selector lablels.
*/}}
{{- define "hephaestus.controller.labels.matchLabels" -}}
{{- include "common.labels.matchLabels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Return the controller config secret name.
*/}}
{{- define "hephaestus.configSecretName" -}}
{{ include "common.names.fullname" . }}-config
{{- end }}

{{/*
Return the controller service account name.
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
{{- include "common.images.renderPullSecrets" (dict "images" (list .Values.controller.image .Values.logProcessor.image .Values.buildkit.image) "global" .Values.global) }}
{{- end }}

{{/*
Return whether or not pod security policies are enabled and supported.
*/}}
{{- define "hephaestus.pspRequired" -}}
{{- if and .Values.enablePodSecurityPolicies (semverCompare "<1.25-0" .Capabilities.KubeVersion.Version) }}
{{- true }}
{{- end }}
{{- end }}

{{/*
Return if istio is enabled without the use of the CNI plugin.
*/}}
{{- define "hephaestus.istioWithoutCNI" -}}
{{- if and .Values.istio.enabled (not .Values.istio.cni) }}
{{- true }}
{{- end }}
{{- end }}

{{/*
Return a name suitable for all manager RBAC objects.
*/}}
{{- define "hephaestus.rbac.managerName" -}}
dominodatalab:controller:{{ include "common.names.fullname" . }}
{{- end }}

{{/*
Return the log processor image name.
*/}}
{{- define "hephaestus.logprocessor.image" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.logProcessor.image "global" .Values.global) }}
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

{{/*
Return the buildkit fully-qualified app name.
*/}}
{{- define "hephaestus.buildkit.fullname" -}}
{{- printf "%s-buildkit" (include "common.names.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Return the buildkit standard labels.
*/}}
{{- define "hephaestus.buildkit.labels.standard" -}}
{{- include "common.labels.standard" . }}
app.kubernetes.io/component: buildkit
{{- end }}

{{/*
Return the buildkit selector lablels.
*/}}
{{- define "hephaestus.buildkit.labels.matchLabels" -}}
{{- include "common.labels.matchLabels" . }}
app.kubernetes.io/component: buildkit
{{- end }}

{{/*
Return the buildkit mtls client secret name.
*/}}
{{- define "hephaestus.buildkit.clientSecret" -}}
{{ include "hephaestus.buildkit.fullname" . }}-client-tls
{{- end }}

{{/*
Return the buildkit mtls server secret name.
*/}}
{{- define "hephaestus.buildkit.serverSecret" -}}
{{ include "hephaestus.buildkit.fullname" . }}-server-tls
{{- end }}

{{/*
Return the buildkit image name.
*/}}
{{- define "hephaestus.buildkit.image" -}}
{{- $imageRoot := .Values.buildkit.image }}
{{- $tag := .Values.buildkit.image.tag | default .Chart.AppVersion }}
{{- if not .Values.buildkit.rootless }}
{{- $tag = trimSuffix "-rootless" $tag }}
{{- end }}
{{- $_ := set $imageRoot "tag" $tag }}
{{- include "common.images.image" (dict "imageRoot" $imageRoot "global" $) }}
{{- end }}

{{/*
Return the buildkit service account name.
*/}}
{{- define "hephaestus.buildkit.serviceAccountName" -}}
{{- if .Values.buildkit.serviceAccount.create }}
{{- default (include "hephaestus.buildkit.fullname" .) .Values.buildkit.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.buildkit.serviceAccount.name }}
{{- end }}
{{- end }}
