{{- define "union-csi-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "union-csi-driver.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "union-csi-driver.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "union-csi-driver.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "union-csi-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "union-csi-driver.selectorLabels" -}}
app.kubernetes.io/name: {{ include "union-csi-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "union-csi-driver.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "union-csi-driver.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "union-csi-driver.backend" -}}
{{- if not (has .Values.backend (list "mergerfs" "overlay")) -}}
{{- fail (printf "backend must be mergerfs or overlay, got %q" .Values.backend) -}}
{{- end -}}
{{- .Values.backend -}}
{{- end -}}

{{- define "union-csi-driver.driverName" -}}
{{- default (printf "%s.csi.example.io" (include "union-csi-driver.backend" .)) .Values.driverName -}}
{{- end -}}

{{- define "union-csi-driver.socketDir" -}}
{{- printf "%s/plugins/%s" (trimSuffix "/" .Values.kubeletRootDir) (include "union-csi-driver.driverName" .) -}}
{{- end -}}

{{- /* Whether the host D-Bus socket is made available to the mergerfs backend. */ -}}
{{- define "union-csi-driver.wantsSystemd" -}}
{{- if and (eq (include "union-csi-driver.backend" .) "mergerfs") (ne .Values.mergerfs.daemonLifetime "in-container") -}}
true
{{- end -}}
{{- end -}}
