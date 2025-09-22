{{- define "dependencyTemplate" -}}
{{- range $index, $dependency := . }}
{{ "-" | line }}
Dependency: {{ $dependency.Name }}

Version: {{ $dependency.Version }}
Date: {{ $dependency.VersionTime }}
License: {{ $dependency.LicenceType }}
{{ "-" | line }}

{{ $dependency | licenceText }}

{{ "=" | line }}
{{ end }}
{{- end -}}

dependency licenses

{{ "=" | line }}

{{ template "dependencyTemplate" .Direct }}

{{ if .Indirect }}
{{ template "dependencyTemplate" .Indirect }}
{{ end }}