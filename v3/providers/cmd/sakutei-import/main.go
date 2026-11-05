// Copyright 2026 tjst-t
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

// Command sakutei-import は、今ある現実を intent の形に書き出すツール（import と、
// --every で繰り返す同期。設計 §7.2・§8 の Stage 9）。使い方は importer.Run。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tjst-t/sakutei/v3/providers/builtin"
	"github.com/tjst-t/sakutei/v3/providers/importer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(importer.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, importer.Options{
		Modules: builtin.Modules(), Expanders: builtin.Expanders(), Secrets: builtin.SecretBackends(), Sinks: builtin.Sinks(),
	}))
}
