// Copyright 2026 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const usageText = `usage: helm2cue <command> [arguments]

Commands:
    chart      convert a Helm chart directory to a CUE module
    template   convert a Go text/template file to CUE
    version    print helm2cue version information

Run "helm2cue help" for more information.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "chart":
		cmdChart(os.Args[2:])
	case "template":
		cmdTemplate(os.Args[2:])
	case "version":
		cmdVersion()
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "helm2cue: unknown command %q\n", os.Args[1])
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}
}

func cmdChart(args []string) {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: helm2cue chart <chart-dir> <output-dir>\n")
		os.Exit(1)
	}
	if err := ConvertChart(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		os.Exit(1)
	}
}

func cmdTemplate(args []string) {
	var helpers [][]byte
	var templateFile string

	for _, arg := range args {
		if strings.HasSuffix(arg, ".tpl") {
			h, err := os.ReadFile(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
				os.Exit(1)
			}
			helpers = append(helpers, h)
		} else {
			if templateFile != "" {
				fmt.Fprintf(os.Stderr, "helm2cue: multiple template files specified\n")
				os.Exit(1)
			}
			templateFile = arg
		}
	}

	var input []byte
	var err error
	if templateFile != "" {
		input, err = os.ReadFile(templateFile)
	} else {
		input, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		os.Exit(1)
	}

	output, err := Convert(TemplateConfig(), input, helpers...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(output)
}

func cmdVersion() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("helm2cue: version information unavailable")
		return
	}

	version := bi.Main.Version
	if version == "" {
		version = "(devel)"
	}
	fmt.Printf("helm2cue %s\n", version)
	fmt.Printf("go %s\n", bi.GoVersion)

	var revision, timeVal, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			timeVal = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision != "" {
		fmt.Printf("commit %s\n", revision)
	}
	if timeVal != "" {
		fmt.Printf("committed %s\n", timeVal)
	}
	if modified == "true" {
		fmt.Println("modified true")
	}
}
