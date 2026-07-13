{{/*
Expand the name of the chart.
*/}}
{{- define "container-image-exporter-cluster.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "container-image-exporter-cluster.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "container-image-exporter-cluster.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels (component label included so queries can still filter by
app.kubernetes.io/component=cluster-exporter).
*/}}
{{- define "container-image-exporter-cluster.selectorLabels" -}}
app.kubernetes.io/name: {{ include "container-image-exporter-cluster.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: cluster-exporter
{{- end }}

{{/*
Common labels
*/}}
{{- define "container-image-exporter-cluster.labels" -}}
helm.sh/chart: {{ include "container-image-exporter-cluster.chart" . }}
{{ include "container-image-exporter-cluster.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Labels for helm test Pods. Uses "component: test" instead of the Deployment's
component label so the ReplicaSet controller does not treat the test Pod as
one of its own replicas and try to reconcile the pod count.
*/}}
{{- define "container-image-exporter-cluster.testLabels" -}}
helm.sh/chart: {{ include "container-image-exporter-cluster.chart" . }}
app.kubernetes.io/name: {{ include "container-image-exporter-cluster.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: test
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "container-image-exporter-cluster.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "container-image-exporter-cluster.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
