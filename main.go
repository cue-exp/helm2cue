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
	"flag"
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
	os.Exit(main1())
}

func main1() int {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		return 1
	}

	switch os.Args[1] {
	case "chart":
		return cmdChart(os.Args[2:])
	case "template":
		return cmdTemplate(os.Args[2:])
	case "version":
		cmdVersion()
		return 0
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "helm2cue: unknown command %q\n", os.Args[1])
		fmt.Fprint(os.Stderr, usageText)
		return 1
	}
}

func cmdChart(args []string) int {
	fs := flag.NewFlagSet("chart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	allowDup := fs.Bool("allow-duplicate-helpers", false, "allow conflicting helper definitions (last wins)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(os.Stderr, "usage: helm2cue chart [-allow-duplicate-helpers] <chart-dir> <output-dir>\n")
		return 1
	}
	opts := ChartOptions{
		AllowDuplicateHelpers: *allowDup,
	}
	if err := ConvertChart(fs.Arg(0), fs.Arg(1), opts); err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		return 1
	}
	return 0
}

func cmdTemplate(args []string) int {
	var helpers [][]byte
	var templateFile string

	for _, arg := range args {
		if strings.HasSuffix(arg, ".tpl") {
			h, err := os.ReadFile(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
				return 1
			}
			helpers = append(helpers, h)
		} else {
			if templateFile != "" {
				fmt.Fprintf(os.Stderr, "helm2cue: multiple template files specified\n")
				return 1
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
		return 1
	}

	output, err := Convert(TemplateConfig(), input, helpers...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		return 1
	}

	os.Stdout.Write(output)
	return 0
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
	fmt.Printf("helm2cue version %s\n\n", version)
	fmt.Printf("go version %s\n", bi.GoVersion)
	for _, s := range bi.Settings {
		if s.Value == "" {
			continue
		}
		fmt.Printf("%16s %s\n", s.Key, s.Value)
	}
}
