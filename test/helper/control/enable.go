// Copyright 2025 MongoDB Inc
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

package control

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

func Enabled(envvar string) bool {
	envSet, _ := strconv.ParseBool(os.Getenv(envvar))
	return envSet
}

func MustEnvVar(envvar string) string {
	value, ok := os.LookupEnv(envvar)
	if !ok {
		panic(fmt.Errorf("missing required environment variable: %v", envvar))
	}
	return value
}

func SkipTestUnless(t *testing.T, envvars ...string) {
	for _, envvar := range envvars {
		if !Enabled(envvar) {
			t.Skipf("Skipping tests, %s is not set", envvar)
		}
	}
}
