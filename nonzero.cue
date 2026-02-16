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

import "struct"

x?: int
x:  0

_nonzero: {
	#arg?: _

	[if x != _|_ {
		[
			if (#arg & int) != _|_ {#arg != 0},
			if (#arg & string) != _|_ {#arg != ""},
			if (#arg & float) != _|_ {#arg != 0.0},
			if (#arg & bool) != _|_ {#arg},
			if (#arg & [...]) != _|_ {len(#arg) > 0},
			if (#arg & {...}) != _|_ {(#arg & struct.MaxFields(0)) == _|_},
			false,
		][0]
	}, false][0]
}

res: _nonzero & {#arg: x, _}
