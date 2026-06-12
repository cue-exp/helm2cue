module github.com/cue-exp/helm2cue

go 1.25.0

require (
	cuelang.org/go v0.16.0
	github.com/rogpeppe/go-internal v1.15.0
	golang.org/x/tools v0.45.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cuelabs.dev/go/oci/ociregistry v0.0.0-20260601085548-328ff8e2c943 // indirect
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/cockroachdb/apd/v3 v3.2.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/emicklei/proto v1.14.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pelletier/go-toml/v2 v2.3.1 // indirect
	github.com/protocolbuffers/txtpbfmt v0.0.0-20260420112717-c39628bde8b5 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	cuelang.org/go/cmd/cue
	honnef.co/go/tools/cmd/staticcheck
)

replace cuelang.org/go => github.com/myitcvscratch/cue v0.4.0-beta.1.0.20260612134833-f88c737c8ff6
