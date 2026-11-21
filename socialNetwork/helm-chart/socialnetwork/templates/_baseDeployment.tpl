{{- define "socialnetwork.templates.baseDeployment" }}
{{- $exporter := and (.Values.memcachedExporter | default false) .Values.global.memcached.exporter.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    service: {{ .Values.name }}
  name: {{ .Values.name }}
spec: 
  replicas: {{ .Values.replicas | default .Values.global.replicas }}
  selector:
    matchLabels:
      service: {{ .Values.name }}
  template:
    metadata:
      labels:
        service: {{ .Values.name }}
        app: {{ .Values.name }}
      {{- if $exporter }}
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: {{ .Values.global.memcached.exporter.port | quote }}
        prometheus.io/path: "/metrics"
      {{- end }}
    spec:
      {{- include "socialnetwork.templates.scheduling" . | nindent 6 }}
      {{- if .Values.nodeName}}
      nodeName: {{ .Values.nodeName }}
      {{ end }}
      containers:
      {{- with .Values.container }}
      - name: "{{ .name }}"
        image: {{ .dockerRegistry | default $.Values.global.dockerRegistry }}/{{ .image }}:{{ .imageVersion | default $.Values.global.defaultImageVersion }}
        imagePullPolicy: {{ .imagePullPolicy | default $.Values.global.imagePullPolicy }}
        ports:
        {{- range $cport := .ports }}
        - containerPort: {{ $cport.containerPort -}}
        {{ end }} 
        {{- if .env }}
        env:
        {{- range $e := .env}}
        - name: {{ $e.name }}
          value: "{{ (tpl ($e.value | toString) $) }}"
        {{ end -}}
        {{ end -}}
        {{- if .command}}
        command: 
        - {{ .command }}
        {{- end -}}
        {{- if .args}}
        args:
        {{- range $arg := .args}}
        - {{ $arg }}
        {{- end -}}
        {{- end }}
        {{- if hasKey . "resources" }}  
        resources:
          {{ toYaml .resources | nindent 10 | trim }}
        {{- else if hasKey $.Values.global "resources" }}           
        resources:
          {{ toYaml $.Values.global.resources | nindent 10 | trim }}
        {{- end }}  
        {{- if $.Values.configMaps }}        
        volumeMounts: 
        {{- range $configMap := $.Values.configMaps }}
        - name: {{ $.Values.name }}-config
          mountPath: {{ $configMap.mountPath }}
          subPath: {{ $configMap.name }}
        {{- end }}
        {{- end }}
      {{- end -}}
      {{- if $exporter }}
      {{- with .Values.global.memcached.exporter }}
      - name: memcached-exporter
        image: "{{ .image }}:{{ .imageVersion }}"
        imagePullPolicy: {{ $.Values.global.imagePullPolicy }}
        args:
        - "--memcached.address=127.0.0.1:11211"
        - "--web.listen-address=:{{ .port }}"
        ports:
        - name: http-metrics
          containerPort: {{ .port }}
        resources:
          {{- toYaml .resources | nindent 10 }}
      {{- end }}
      {{- end }}
      {{- if $.Values.configMaps }}
      volumes:
      - name: {{ $.Values.name }}-config
        configMap:
          name: {{ $.Values.name }}
      {{- end }}
      {{- if hasKey .Values "topologySpreadConstraints" }}
      topologySpreadConstraints:
        {{ tpl .Values.topologySpreadConstraints . | nindent 6 | trim }}
      {{- else if hasKey $.Values.global  "topologySpreadConstraints" }}
      topologySpreadConstraints:
        {{ tpl $.Values.global.topologySpreadConstraints . | nindent 6 | trim }}
      {{- end }}
      hostname: {{ $.Values.name }}
      restartPolicy: {{ .Values.restartPolicy | default .Values.global.restartPolicy}}

{{ include "socialnetwork.templates.baseHPA" . }}
{{- end}}
