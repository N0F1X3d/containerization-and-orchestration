{{/* Общие метки — их требует политика require-labels. */}}
{{- define "shop.labels" -}}
app.kubernetes.io/part-of: shop
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* Безопасный securityContext контейнера: не root, без повышения прав, read-only ФС. */}}
{{- define "shop.containerSecurity" -}}
securityContext:
  privileged: false
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  capabilities: {drop: [ALL]}
{{- end }}

{{/* Адрес базы: оператор кладёт его в Secret shop-db-app (ключ uri). Монтируем Secret томом с optional:
     пока базы нет (до части 3), под всё равно стартует; появится Secret — kubelet сам подложит файл. */}}
{{- define "shop.dbEnv" -}}
- {name: DATABASE_URL_FILE, value: /etc/db/uri}
{{- end }}
{{- define "shop.dbMount" -}}
volumeMounts:
  - {name: db, mountPath: /etc/db, readOnly: true}
{{- end }}
{{- define "shop.dbVolume" -}}
volumes:
  - name: db
    secret: {secretName: shop-db-app, optional: true}
{{- end }}
