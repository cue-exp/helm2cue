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
	tmpl, err := template.ParseFiles("_helpers.tpl", "config.yaml.tmpl")
	if err != nil {
		panic(err)
	}
	if err := tmpl.ExecuteTemplate(os.Stdout, "config.yaml.tmpl", map[string]any{"Values": values}); err != nil {
		panic(err)
	}
}
