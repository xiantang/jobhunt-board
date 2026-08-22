{{/*
资源名。用 release 名而不是写死，这样同一集群里可以再装一份（比如 staging）不撞名。
truncate 63 是 k8s label value 的上限。
*/}}
{{- define "jobhunt.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
selector labels:Deployment.spec.selector 是不可变字段，一旦创建改不了。
所以这组必须最小、稳定，绝不能包含版本号之类会变的东西。
*/}}
{{- define "jobhunt.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
完整 label 集：selector labels + 会变的元信息。用在 metadata.labels（可变）上。
*/}}
{{- define "jobhunt.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "jobhunt.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
MySQL 的 selector labels。刻意【不】复用上面那组：
Deployment 和 Service 的 selector 就是那组 name+instance,MySQL 的 Pod
要是也带着它，应用的 Service 会把请求轮到数据库 Pod 上（8080 端口没人听），
Deployment 也会把它当成自己多出来的副本。所以 name 换成 <chart>-mysql,
两边从标签层面就不相交。
*/}}
{{- define "jobhunt.mysqlSelectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}-mysql
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
MySQL 的完整 label 集。
*/}}
{{- define "jobhunt.mysqlLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "jobhunt.mysqlSelectorLabels" . }}
app.kubernetes.io/component: mysql
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
