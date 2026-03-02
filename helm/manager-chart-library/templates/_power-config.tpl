{{- define "manager-chart-library.powerconfig" -}}
apiVersion: power.openshift.io/v1
kind: PowerConfig
metadata:
  name: {{ .Values.powerconfig.name }}
  namespace: {{ .Values.powerconfig.namespace }}
spec:
  powerNodeSelector:
    {{ .Values.powerconfig.nodeselector.label }}: "{{  .Values.powerconfig.nodeselector.value  }}"
  powerProfiles:
  - "performance"
  - "balance-performance"
  - "balance-power"

{{- end -}}
