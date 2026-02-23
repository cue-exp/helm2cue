// execute.go renders config.yaml.tmpl using data from data.yaml.
//
// Usage:
//
//	go run execute.go
package main

import (
	"os"
	"text/template"

	"gopkg.in/yaml.v3"
)

func main() {
	// Read data.
	raw, err := os.ReadFile("data.yaml")
	if err != nil {
		panic(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		panic(err)
	}

	// Parse and execute template.
	//
	// We register a "default" function so the template can use
	// {{ .Values.x | default "fallback" }} — the same pipeline
	// that Helm provides via Sprig.
	funcs := template.FuncMap{
		"default": func(dflt, val any) any {
			if val == nil {
				return dflt
			}
			if s, ok := val.(string); ok && s == "" {
				return dflt
			}
			return val
		},
	}
	tmpl, err := template.New("config.yaml.tmpl").Funcs(funcs).ParseFiles("config.yaml.tmpl")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(os.Stdout, map[string]any{"Values": values}); err != nil {
		panic(err)
	}
}
