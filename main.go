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
	"strings"
)

func main() {
	// Chart-level mode: first arg is a directory.
	if len(os.Args) >= 2 {
		if info, err := os.Stat(os.Args[1]); err == nil && info.IsDir() {
			if len(os.Args) != 3 {
				fmt.Fprintf(os.Stderr, "usage: helm2cue <chart-dir> <output-dir>\n")
				os.Exit(1)
			}
			if err := ConvertChart(os.Args[1], os.Args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	var input []byte
	var helpers [][]byte
	var err error

	// Separate args: .tpl files are helpers, other file is the template,
	// no file reads the template from stdin.
	var templateFile string
	for _, arg := range os.Args[1:] {
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

	if templateFile != "" {
		input, err = os.ReadFile(templateFile)
	} else {
		input, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		os.Exit(1)
	}

	output, err := Convert(input, helpers...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helm2cue: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(output)
}
