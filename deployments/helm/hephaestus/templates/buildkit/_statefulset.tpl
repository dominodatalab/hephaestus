{{/*
Renders a full buildkit StatefulSet manifest (metadata, spec, volumeClaimTemplates). Shared by the
legacy single-pool path (statefulset.yaml) and the platformPools path (pool-statefulset.yaml) - both
callers pre-resolve whichever fields differ between the two (name/namespace/labels/
serviceAccountName/checksum/replicaCount/resources/nodeSelector/tolerations/affinity/
priorityClassName/args/extra pod labels+annotations); everything else that never varies per pool
(image, rootless mode, mTLS, service port, persistence, etc.) is read directly off ctx.root.

Rendered as a full top-level document (not nested inside another block), so callers never need to
nindent this template's own output - only its own already-rendered sub-pieces (labels) get nindented,
here, at the fixed depths the two callers already share. Avoids the nindent/checksum trap documented
in docs/multi-arch-image-builds-design.md, which was specific to sharing content nested under a
`data:` key.

Expects a dict with:
  root                - $
  name                - resolved StatefulSet/Service/ServiceAccount/ConfigMap name
  namespace           - resolved namespace, or "" to omit metadata.namespace (legacy path)
  standardLabels      - raw (un-nindented) rendered standard labels
  matchLabels         - raw (un-nindented) rendered selector labels
  serviceAccountName  - resolved service account name
  checksumConfig      - pre-computed checksum/config annotation value
  extraPodAnnotations - additional (pool-specific) pod annotations map, or nil
  extraPodLabels      - additional (pool-specific) pod labels map, or nil
  replicaCount, resources, nodeSelector, tolerations, affinity, priorityClassName, args - resolved,
    already defaulted to the shared buildkit.* value when not overridden per pool
*/}}
{{- define "hephaestus.buildkit.statefulset.render" -}}
{{- $ctx := . -}}
{{- $root := $ctx.root -}}
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ $ctx.name }}
  {{- if $ctx.namespace }}
  namespace: {{ $ctx.namespace }}
  {{- end }}
  labels:
    {{- $ctx.standardLabels | nindent 4 }}
spec:
  serviceName: {{ $ctx.name }}
  podManagementPolicy: Parallel
  replicas: {{ $ctx.replicaCount }}
  {{- if semverCompare ">=1.32.0" $root.Capabilities.KubeVersion.Version }}
  persistentVolumeClaimRetentionPolicy:
    whenDeleted: {{ $root.Values.buildkit.persistence.pvcRetentionPolicy.whenDeleted }}
    whenScaled: {{ $root.Values.buildkit.persistence.pvcRetentionPolicy.whenScaled }}
  {{- end }}
  selector:
    matchLabels:
      {{- $ctx.matchLabels | nindent 6 }}
  template:
    metadata:
      annotations:
        checksum/config: {{ $ctx.checksumConfig }}
        cluster-autoscaler.kubernetes.io/safe-to-evict: "false"
        {{- if $root.Values.buildkit.rootless }}
        container.apparmor.security.beta.kubernetes.io/buildkitd: unconfined
        {{- end }}
        {{- with $root.Values.buildkit.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
        {{- with $ctx.extraPodAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
        {{- with $root.Values.podAnnotations }}
          {{- toYaml . | nindent 8 }}
        {{- end }}
      labels:
        {{- $ctx.matchLabels | nindent 8 }}
        {{- with $root.Values.buildkit.podLabels }}
        {{- toYaml . | trimSuffix "\n" | nindent 8 }}
        {{- end }}
        {{- with $ctx.extraPodLabels }}
        {{- toYaml . | trimSuffix "\n" | nindent 8 }}
        {{- end }}
        {{- with $root.Values.podLabels }}
          {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      {{- include "hephaestus.imagePullSecrets" $root | indent 6 }}
      serviceAccountName: {{ $ctx.serviceAccountName }}
      enableServiceLinks: {{ $root.Values.buildkit.enableServiceLinks }}
      securityContext:
        runAsNonRoot: {{ $root.Values.buildkit.rootless }}
        runAsUser: {{ ternary $root.Values.buildkit.rootlessUser 0 $root.Values.buildkit.rootless }}
        fsGroup: {{ ternary $root.Values.buildkit.rootlessUser 0 $root.Values.buildkit.rootless }}
        fsGroupChangePolicy: "OnRootMismatch"
        seLinuxOptions:
          type: spc_t
      {{- if $root.Values.buildkit.binfmt.enabled }}
      initContainers:
        # Registers QEMU user-mode emulation handlers on this pod's node so buildkitd can solve
        # non-native platforms. Writes to /proc/sys/fs/binfmt_misc, which is not mount-namespaced -
        # a privileged container's registration is visible to the host kernel without any special
        # volume mounts or host namespace sharing, so this only needs to run here, in the same pod
        # as buildkitd, rather than as a separate cluster-wide DaemonSet.
        - name: binfmt
          image: {{ include "hephaestus.buildkit.binfmt.image" $root }}
          imagePullPolicy: {{ $root.Values.buildkit.binfmt.image.pullPolicy }}
          args: ["--install", "all"]
          {{- with $root.Values.podEnv }}
          env:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            privileged: true
      {{- end }}
      containers:
        - name: buildkitd
          securityContext:
            {{- if $root.Values.buildkit.rootless }}
            # NOTE: To change UID/GID, you need to rebuild the image
            runAsUser: {{ $root.Values.buildkit.rootlessUser }}
            runAsGroup: {{ $root.Values.buildkit.rootlessUser }}
            {{- toYaml $root.Values.buildkit.rootlessContainerSecurityContext | nindent 12 }}
            {{- else }}
            {{- toYaml $root.Values.buildkit.containerSecurityContext | nindent 12 }}
            {{- end }}
          image: {{ include "hephaestus.buildkit.image" $root }}
          imagePullPolicy: {{ $root.Values.buildkit.image.pullPolicy }}
          args:
            - --config
            - /etc/buildkit/buildkitd.toml
            {{- if $root.Values.buildkit.rootless }}
            - --addr
            - tcp://0.0.0.0:{{- default 1234 $root.Values.buildkit.service.port }}
            - --addr
            - unix:///run/user/{{ $root.Values.buildkit.rootlessUser }}/buildkit/buildkitd.sock
            {{- end -}}
            {{- if $root.Values.buildkit.debug }}
            - --debug
            {{- end }}
            {{- with $ctx.args }}
              {{- toYaml . | nindent 12 }}
            {{- end }}
          {{- with $root.Values.podEnv }}
          env:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          ports:
            - name: {{ $root.Values.buildkit.service.portName }}
              protocol: TCP
              containerPort: {{ $root.Values.buildkit.service.port }}
          livenessProbe:
            exec: null
            tcpSocket:
              port: {{ default 1234 $root.Values.buildkit.service.port }}
            {{- with $root.Values.buildkit.livenessProbe }}
            initialDelaySeconds: {{ .initialDelaySeconds }}
            periodSeconds: {{ .periodSeconds }}
            timeoutSeconds: {{ .timeoutSeconds }}
            failureThreshold: {{ .failureThreshold }}
            successThreshold: {{ .successThreshold }}
            {{- end }}
          readinessProbe:
            exec: null
            tcpSocket:
              port: {{ default 1234 $root.Values.buildkit.service.port }}
            {{- with $root.Values.buildkit.readinessProbe }}
            initialDelaySeconds: {{ .initialDelaySeconds }}
            periodSeconds: {{ .periodSeconds }}
            timeoutSeconds: {{ .timeoutSeconds }}
            failureThreshold: {{ .failureThreshold }}
            successThreshold: {{ .successThreshold }}
            {{- end }}
          resources:
            {{- toYaml $ctx.resources | nindent 12 }}
          volumeMounts:
            {{- if $root.Values.buildkit.rootless }}
            - mountPath: /etc/subuid
              name: config-vol
              subPath: subuid
            - mountPath: /etc/subgid
              name: config-vol
              subPath: subgid
            {{- end }}
            - name: config-vol
              readOnly: true
              mountPath: /etc/buildkit/buildkitd.toml
              subPath: buildkitd.toml
            {{- if $root.Values.buildkit.mtls.enabled }}
            - name: mtls-vol
              readOnly: true
              mountPath: /etc/buildkit/x509
            {{- end }}
            {{- with $root.Values.buildkit.customCABundle }}
            - name: ca-bundle-vol
              readOnly: true
              mountPath: /etc/ssl/certs
            {{- end }}
            {{- if $root.Values.buildkit.persistence.enabled }}
            - name: cache
              {{- if $root.Values.buildkit.rootless }}
              mountPath: /home/user/.local/share/buildkit
              {{- else }}
              mountPath: /var/lib/buildkit
              {{- end }}
            {{- end }}
      volumes:
        - name: config-vol
          configMap:
            name: {{ $ctx.name }}
        {{- if $root.Values.buildkit.mtls.enabled }}
        - name: mtls-vol
          secret:
            secretName: {{ include "hephaestus.buildkit.serverSecret" $root }}
        {{- end }}
        {{- with $root.Values.buildkit.customCABundle }}
        - name: ca-bundle-vol
          configMap:
            name: {{ . }}
        {{- end }}
      {{- with $ctx.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $ctx.affinity }}
      affinity:
        {{- tpl . $root | nindent 8 }}
      {{- end }}
      {{- with $ctx.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $ctx.priorityClassName }}
      priorityClassName: {{ . | quote }}
      {{- end }}
  {{- if $root.Values.buildkit.persistence.enabled }}
  volumeClaimTemplates:
    - metadata:
        name: cache
        labels:
          {{- $ctx.matchLabels | nindent 10 }}
      spec:
        storageClassName: {{ $root.Values.buildkit.persistence.storageClass }}
        accessModes:
          {{- toYaml $root.Values.buildkit.persistence.accessModes | nindent 10 }}
        resources:
          requests:
            storage: {{ $root.Values.buildkit.persistence.size }}
  {{- end }}
{{- end }}
