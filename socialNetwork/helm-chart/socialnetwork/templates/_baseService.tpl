{{- define "socialnetwork.templates.baseService" }}
{{- $serviceType := .Values.type | default .Values.global.serviceType }}

apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.name }}
spec:
  type: {{ $serviceType }}
  ports:  
  {{- range .Values.ports }}
  - name: "{{ .port }}"
    port: {{ .port }}
    {{- if .protocol}}
    protocol: {{ .protocol }}
    {{- end}}
    targetPort: {{ .targetPort }}
    {{- if and .nodePort (eq $serviceType "NodePort") }}
    nodePort: {{ .nodePort }}
    {{- end }}
  {{- end}}
  selector:
    service: {{ .Values.name }} 

{{- end }}
