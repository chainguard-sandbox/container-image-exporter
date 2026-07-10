{{/*
Expand the name of the chart.
*/}}
{{- define "container-image-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "container-image-exporter.fullname" -}}
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
{{- define "container-image-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "container-image-exporter.labels" -}}
helm.sh/chart: {{ include "container-image-exporter.chart" . }}
{{ include "container-image-exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Base selector labels (shared by both components)
*/}}
{{- define "container-image-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "container-image-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Cluster exporter fully qualified name
*/}}
{{- define "container-image-exporter.clusterExporter.fullname" -}}
{{- printf "%s-cluster-exporter" (include "container-image-exporter.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Cluster exporter selector labels
*/}}
{{- define "container-image-exporter.clusterExporter.selectorLabels" -}}
{{ include "container-image-exporter.selectorLabels" . }}
app.kubernetes.io/component: cluster-exporter
{{- end }}

{{/*
Cluster exporter common labels (includes component label)
*/}}
{{- define "container-image-exporter.clusterExporter.labels" -}}
{{ include "container-image-exporter.labels" . }}
app.kubernetes.io/component: cluster-exporter
{{- end }}

{{/*
Create the name of the cluster exporter service account to use
*/}}
{{- define "container-image-exporter.clusterExporter.serviceAccountName" -}}
{{- if .Values.clusterExporter.serviceAccount.create }}
{{- default (include "container-image-exporter.clusterExporter.fullname" .) .Values.clusterExporter.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.clusterExporter.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Node exporter fully qualified name
*/}}
{{- define "container-image-exporter.nodeExporter.fullname" -}}
{{- printf "%s-node-exporter" (include "container-image-exporter.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Node exporter selector labels
*/}}
{{- define "container-image-exporter.nodeExporter.selectorLabels" -}}
{{ include "container-image-exporter.selectorLabels" . }}
app.kubernetes.io/component: node-exporter
{{- end }}

{{/*
Node exporter common labels (includes component label)
*/}}
{{- define "container-image-exporter.nodeExporter.labels" -}}
{{ include "container-image-exporter.labels" . }}
app.kubernetes.io/component: node-exporter
{{- end }}

{{/*
Create the name of the node exporter service account to use
*/}}
{{- define "container-image-exporter.nodeExporter.serviceAccountName" -}}
{{- if .Values.nodeExporter.serviceAccount.create }}
{{- default (include "container-image-exporter.nodeExporter.fullname" .) .Values.nodeExporter.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.nodeExporter.serviceAccount.name }}
{{- end }}
{{- end }}
