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

//go:build unix

// Command sakutei-nft-guard は nftables provider の専用ツール（設計 §7.1、
// E-E1・E-B3）。対象のホストに置き、sudo で許すのはこのツールだけにする。
package main

import (
	"flag"
	"os"

	"github.com/tjst-t/sakutei/v3/providers/nftguard"
)

func main() {
	fs := flag.NewFlagSet(nftguard.Name, flag.ContinueOnError)
	config := fs.String("config", nftguard.DefaultConfigPath, "専用ツールの設定ファイル（root が所有し、group・other は書けないこと）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(nftguard.ExitRefused)
	}
	os.Exit(nftguard.Main(fs.Args(), nftguard.Options{ConfigPath: *config}))
}
