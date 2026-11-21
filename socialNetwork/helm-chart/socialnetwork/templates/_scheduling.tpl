{{/* Explicit local values, including empty maps/lists, override global defaults. */}}
{{- define "socialnetwork.templates.scheduling" -}}
{{- range $key := list "nodeSelector" "affinity" "tolerations" -}}
{{- $value := index $.Values.global $key -}}
{{- if hasKey $.Values $key -}}
{{- $value = index $.Values $key -}}
{{- end -}}
{{- with $value }}
{{ $key }}:{{ toYaml . | nindent 2 }}
{{- end -}}
{{- end -}}
{{- end -}}
