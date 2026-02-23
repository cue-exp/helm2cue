package main

//go:generate go run . chart ./examples/simple-app/helm ./examples/simple-app/cue
//go:generate sh -c "go run . template ./examples/standalone/_helpers.tpl ./examples/standalone/config.yaml.tmpl > ./examples/standalone/config.cue"
