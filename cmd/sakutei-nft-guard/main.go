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

// Command sakutei-nft-guard は nftables provider の決まった道具（Backlog #191）。
// 対象のホストの決まった場所（/usr/local/libexec/sakutei-nft-guard）に置き、
// sudo で許すのはこの道具だけにする。設定ファイルは /etc/sakutei/nft-guard.json
// （決まった場所。呼び手からは変えられない）。中身は internal/nftguard。
package main

import (
	"os"

	"github.com/tjst-t/sakutei/v2/internal/nftguard"
)

func main() {
	os.Exit(nftguard.Main(os.Args[1:], nftguard.Options{ConfigPath: nftguard.DefaultConfigPath}))
}
