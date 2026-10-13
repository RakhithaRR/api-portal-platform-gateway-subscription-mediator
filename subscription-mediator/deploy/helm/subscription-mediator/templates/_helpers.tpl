{{/* Chart name, overridable with nameOverride. */}}
{{- define "subscription-mediator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name. Truncated to 52 characters, leaving room for the
"-headless" suffix and the StatefulSet's controller-revision hash label inside
Kubernetes' 63-character limit.
*/}}
{{- define "subscription-mediator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 52 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "subscription-mediator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "subscription-mediator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "subscription-mediator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "subscription-mediator.labels" -}}
helm.sh/chart: {{ include "subscription-mediator.chart" . }}
{{ include "subscription-mediator.selectorLabels" . }}
app.kubernetes.io/version: {{ include "subscription-mediator.imageVersion" . | quote }}
app.kubernetes.io/component: webhook-bridge
app.kubernetes.io/part-of: wso2-api-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "subscription-mediator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "subscription-mediator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The image reference: repository@digest when a digest is set, else repository:tag. */}}
{{- define "subscription-mediator.image" -}}
{{- $repo := required "image.repository is required, e.g. myregistry.azurecr.io/subscription-mediator" .Values.image.repository }}
{{- if .Values.image.digest }}
{{- printf "%s@%s" $repo .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" $repo (required "set image.tag (the sha-<commit> tag CI pushed) or image.digest" .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* A label-safe version: the tag, else the first 12 hex digits of the digest. */}}
{{- define "subscription-mediator.imageVersion" -}}
{{- if .Values.image.tag }}
{{- .Values.image.tag | trunc 63 | trimSuffix "-" }}
{{- else if .Values.image.digest }}
{{- .Values.image.digest | trimPrefix "sha256:" | trunc 12 }}
{{- end }}
{{- end }}

{{- define "subscription-mediator.secretName" -}}
{{- if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else if .Values.secret.create }}
{{- include "subscription-mediator.fullname" . }}
{{- else }}
{{- fail "credentials are required: set secret.existingSecret to a Secret holding the webhook secret and gateway credentials (recommended), or secret.create=true with the values" }}
{{- end }}
{{- end }}

{{/* config.toml. Strings are emitted as JSON string literals, which are valid TOML basic strings. */}}
{{- define "subscription-mediator.configToml" -}}
# Rendered by the subscription-mediator Helm chart. Credentials are injected
# as SM_* environment variables from a Secret and are never written here.

[gateway]
base_url = {{ required "config.gateway.baseUrl is required: the Gateway Controller management REST API URL" .Values.config.gateway.baseUrl | toJson }}

[logging]
level = {{ .Values.config.logging.level | toJson }}

[api_handles]
{{- range $key, $handle := .Values.config.apiHandles }}
{{ $key | toJson }} = {{ $handle | toJson }}
{{- end }}
{{- end }}

{{/*
Labels for the `helm test` pod. Deliberately NOT the selector labels, so the
Service never routes to it and the main NetworkPolicy does not select it.
*/}}
{{- define "subscription-mediator.testSelectorLabels" -}}
app.kubernetes.io/name: {{ include "subscription-mediator.name" . }}-test
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
