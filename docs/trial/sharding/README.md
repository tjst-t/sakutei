# 静的シャーディング（claim の git 配布・S2-free シャード・ハンドオフ）と inventory 検証を手で試す

v1b の静的シャーディング（設計 §17・§15.2）と inventory の検証（§9.2）は、これまで自動テスト
（`tests/acceptance/claim_git_test.go`・`claim_fencing_test.go`・`claim_prune_boundary_test.go`・`inventory_test.go`）でしか
確かめていなかった。ここでは実バイナリの常駐デーモンを何本も立て、本物の git（ローカルの bare リポジトリを中央に、
`sakutei mirror` で作った読み取り専用ミラーを拠点の pull 元に）を相手に、手で通す。外のサービスには繋がない。

2026-10-23 にこのコンテナ（git 2.43.0、Go 1.22 以上）で `run.sh` を流し、44 の観点がすべて OK になった（見つけた不具合 2 件を直した後。設計の判断が要るもの 2 件は「見つけたこと」の行として出る）。

## 確かめる観点（設計書と受け入れテストから）

| # | 観点 | 設計 |
|---|---|---|
| 1 | 2 つのデーモンが `shards/<名前>.yaml` の claim で担当を分け、互いの scope を Observe も Apply もしない。手元写しは自 claim の `intent/<scope>/` と `shards/`・`intent/platform/` だけ（sparse checkout + partial clone） | §17.1・§17.4 |
| 2 | 同じ scope を 2 つのデーモンが掴むと、あとから世代を上げた方だけが書き、古い方は `FENCED` で書かない | §15.2・§17.1 |
| 3 | claim 変更 PR のマージがハンドオフになる（旧担当は `claim released`、新担当は世代を上げて収束）。旧担当が claim 変更をまだ pull していなくても、fencing で二重に書かない | §17.6・§17.7 |
| 4 | ミラーに届かない間（WAN 断）は前回 pull した intent で自己修復を続ける | §17.4 |
| 5 | S2-free シャードはホストを全損しても、同じフラグで起こすだけで pull → 収束で戻る（止まっている間のずれも、進んだ intent も） | §17.5 |
| 6 | claim の外の現実は prune の候補にならない。claim の中の孤児は `PruneWithinScope` なら消える | §17.1・FIX #6 |
| 7 | inventory が前回の半分未満に急減したら halt（Apply も prune もしない）、ちょうど半分は受け入れる、不達は前回を保持、再起動直後は期待基数で守る | §9.2 |

## 置いてあるもの（docs/trial/sharding/）

| ファイル | 中身 |
|---|---|
| `repo/` | 中央の intent リポジトリの最初の中身。`intent/site=a`・`site=b`・`site=c` の DNS レコード（dnszone）と `shards/` の 4 シャード（`store-a`・`store-b`・`central` は site=c を持つ、`store-c` は開店前で idle） |
| `initial/example.com.zone` | 現実。全拠点が 1 つのゾーンに自分のレコードを持つ（`www` だけがある） |
| `initial/fake-reality.json` | 手順 6b の現実（fake provider）。List が scope を返す。site=a の `a-vm`・`a-old`、site=b の `b-vm`・`b-old`、scope なしの `stray` |
| `inventory/intent.yaml` | 手順 7 の intent（`www` だけ。残りは inventory から合成する） |
| `run.sh` | 手順 0〜7 を通して流し、観点ごとに OK／NG を出すスクリプト |

## 通しで流す

```sh
docs/trial/sharding/run.sh            # 作業ディレクトリは /tmp/sakutei-trial-sharding（毎回作り直す）
```

Go と git が要る。リポジトリのファイルは書き換えない。2 分ほどかかる。各デーモンのログは作業ディレクトリの `logs/` に残る。

作業ディレクトリの中の配置：

| パス | 見立て |
|---|---|
| `central.git` | 中央 git（bare）。`work/` でコミットして push する = PR のマージ |
| `mirror.git` | 拠点が pull する読み取り専用ミラー（`sakutei mirror --once` で中央に追従させる） |
| `host-a`・`host-b`・`host-c`・`host-central` | 各デーモンのホストのディスク（`--workdir`。手元写し `repo/` と render 出力 `generated/`） |
| `example.com.zone` | 現実（拠点ホストの外にある DNS） |
| `fencing.json` | 全シャードで共有する fencing 世代のストア（§15.2。拠点ホストの外に置く） |

拠点のデーモンはどれも次の形で起こす（`run.sh` の `start`）：

```sh
./sakutei --intent-repo file://$W/mirror.git --workdir host-a --shard store-a --shard-id store-a --s2-free \
  --git-poll 1s --interval 1s --zone $W/example.com.zone --fencing-store $W/fencing.json
```

## 手順と結果（2026-10-23）

### 1. 2 つの店舗デーモンが claim で担当を分ける

`store-a`・`store-b`（S2-free）・`central`・`store-c`（S2-free、idle）の 4 本を起こす。

- 3 本がそれぞれの scope の世代 1 を取り、自分のレコードだけを書く。`store-c` は `shard claims nothing at revision 87fb2d18146e; idle`。

  ```
  SCOPE                                    GENERATION  HOLDER                         ACQUIRED
  site=a                                            1  store-a                        2026-10-23T15:33:42Z
  site=b                                            1  store-b                        2026-10-23T15:33:42Z
  site=c                                            1  central                        2026-10-23T15:33:42Z
  ```
- `host-a/repo` にあるのは `intent/site=a/dns.yaml` と `shards/*.yaml` だけ。`git rev-list --objects --missing=print HEAD` で、
  site=b・site=c の blob は手元に無い（欠けた blob 2 個）。
- ゾーンの `a-www` と `b-www` を同時に手で書き換えると、`a-www` は `store-a` だけが、`b-www` は `store-b` だけが直す。
  `store-a` のログに `site=b` は 1 度も出ない（逆も同じ）。

### 2. 二重 claim

同じ `--shard store-a` で 2 本目（`--shard-id store-a2`）を起こす。

```
store-a2: sakutei: fencing: scope site=a taken over from "store-a" (generation 1 -> 2); the previous holder is fenced from now on
store-a : sakutei: FENCED: fenced: scope site=a is held by "store-a2" at generation 2 (this shard "store-a" holds generation 1); double claim or re-shard — not writing (§15.2, §17.1)
store-a : sakutei: cycle 6: FENCED scope(s) [site=a]: another shard holds a newer fencing generation; not converging them
```

`a-api` をずらすと `store-a2` だけが直し、`store-a` のログに `a-api` は出ない。2 本とも止めて `store-a` を起こし直すと
`taken over from "store-a2" (generation 2 -> 3)` で取り戻す（§17.6 の 4）。

### 3. 開店・閉店（claim 変更 PR のハンドオフ）

開店：`shards/central.yaml` の claim を空に、`shards/store-c.yaml` を `site: c` にしてマージする。

```
central: sakutei: claim released at revision 7a3f91ca90d7; draining in-flight Applies, shard is idle
store-c: sakutei: claim=site=c shard=store-c
store-c: sakutei: fencing: scope site=c taken over from "central" (generation 1 -> 2); the previous holder is fenced from now on
store-c: sakutei: DriftEvent: dnszone/site=c/c-www.example.com./A [cause: B] drift B: value was "192.0.2.31" prev cycle now "198.51.100.31" ...
```

開店後のずれは店舗だけが直す（中央のログに `c-www` は出ない）。

閉店は、**店舗がまだ claim 変更を pull していない**場合を作って試した。店舗を `--git-poll 1h` で起こし直し（起動時の pull で
site=c を持ち、世代 3 を取る）、そのあとで claim を中央に戻す PR をマージする。

```
central       : sakutei: fencing: scope site=c taken over from "store-c" (generation 3 -> 4); the previous holder is fenced from now on
store-c (遅い): sakutei: FENCED: fenced: scope site=c is held by "central" at generation 4 (this shard "store-c" holds generation 3); double claim or re-shard — not writing (§15.2, §17.1)
central       : sakutei: DriftEvent: dnszone/site=c/c-www.example.com./A [cause: B] drift B: value was "192.0.2.31" prev cycle now "198.51.100.32" ...
```

店舗は自分がまだ site=c の担当だと思っているが、世代が古いので書かない。ずれは中央だけが直す（二重に書かない）。

### 4. WAN 断

`mirror.git` を退けると、各デーモンは `intent source refresh failed; converging on the last pulled revision a821566b97f4: git fetch: exit status 128: fatal: '/tmp/sakutei-trial-sharding/mirror.git' does not appear to be a git repository`
を出し、その間も `a-www` のずれを直す。ミラーは `sakutei mirror --once` で作り直せる（状態を持たない中継）。

4b. ただし、fencing のストアにも届かないと話が変わる。別のゾーンと別の fencing ストアで `store-b` 相当を 1 本起こし、
fencing ストアのディレクトリを読めなくしてから `b-www` をずらすと、毎周 `fencing store unreachable; cycle skipped` で飛ばし、
ずれは直らない。届くようになった周に直る（見つけたこと 6）。

### 5. S2-free シャードのホスト全損

`store-b` を `kill -9` し、`host-b` を丸ごと消す。止まっている間にゾーンの `b-www` をずらし、中央では `b-mail` を足す PR をマージする。
同じフラグで起こすと：

```
sakutei: fencing: scope site=b acquired by "store-b" at generation 2
sakutei: DriftEvent: dnszone/site=b/b-www.example.com./A [cause: B] no previous snapshot (first cycle or post-restart)
sakutei: cycle 1: observed=2 applied=0 launched=2
sakutei: cycle 2: RESOLVED Drift dnszone/site=b/b-www.example.com./A: resolved after 1 cycle(s) ...
```

`b-mail` が入り、`b-www` が戻る。復旧後の `host-b` にあるのは pull した `repo/`（`intent/site=b/dns.yaml` と `shards/`）と
`generated/expanded.json` だけ。

### 6. claim の外は prune しない

- **6a（dnszone）**：site=a の intent から `a-api` を外し、`a-www` を `deletion_policy: PruneWithinScope` にしてマージする。
  b・c・`www` のレコードは残る。ただし **`a-api` も消えない**（見つけたこと 3）。
- **6b（fake provider。List が scope を返す）**：site=a に `fake` の `a-vm` だけを `PruneWithinScope` で宣言し、`store-a` を
  `--state fake-reality.json` を足して起こし直す。

  ```
  sakutei: pruned fake/site=a/a-old (reality − desired; PruneWithinScope)
  sakutei: cycle 1: observed=2 applied=0 launched=0 pruned=1
  ```

  現実に残ったのは `a-vm b-old b-vm stray`。claim の中の孤児 `a-old` だけが消え、claim の外（`b-vm`・`b-old`）と scope の無い `stray` は残る。

### 7. inventory の検証

`--inventory-file inventory.yaml` で起こし（`--intent` は `www` だけ）、inventory（`h1`〜`h4`、`PruneWithinScope`）を書き換える。

| inventory | 結果 |
|---|---|
| 4 件 | 4 件が入る |
| 4 → 1 件 | `InventoryIncomplete: inventory dropped from 4 to 1 item(s) (below 50% of the previous) ...`・`EVENT InventoryHalted`（1 回）。何も消さない |
| 4 → 2 件（ちょうど半分） | 受け入れる。`pruned dnszone//h4…`・`pruned dnszone//h3…`・`pruned=2`、`RESOLVED InventoryHalted` |
| ファイルを消す（不達） | `ExternalInputUnreachable: ... using the previous inventory (§9.2 last-value-on-unreachable)`。消さない |
| 再起動直後に 1 件、`--inventory-expected-min 2` | `inventory has 1 item(s), fewer than the expected 2` で最初の周から halt。消さない |
| 再起動直後に 1 件、期待基数なし | 受け入れて `h2` を prune する（基準は S3 で再起動で消える。設計どおり §9.2。守りたいなら期待基数を宣言する） |

## 見つけたこと

1. **（直した）相対パスの `--workdir` で、拠点のデーモンが最初の pull から先へ進まない。** `git clone` を `--workdir` の中で
   走らせながら clone 先を相対パスのまま渡していたため、`host-a/host-a/repo` に clone され、続く sparse-checkout が
   `chdir host-a/repo: no such file or directory` で失敗する。その残骸のせいで、以後の clone は毎周
   `destination path 'host-a/repo' already exists` で失敗し続けた（周は飛ばすので壊しはしないが、永遠に収束しない）。
   受け入れテストは絶対パスしか使っていなかった。clone 先を絶対パスにして直した（`internal/gitdist/checkout.go`、
   回帰テスト `TestCheckout_RelativeDir`）。
2. **（直した）ミラーに届かないときのエラーが `and the repository exists.` だけになる。** git の標準エラーの最後の行だけを
   残していたため、原因（`fatal: '…/mirror.git' does not appear to be a git repository`）が消えていた。最初の `fatal:`／`error:`
   の行を残すようにした（`gitErrLine`、テスト `TestCheckout_UnreachableRemoteError`・`TestGitErrLine`）。
3. **（直していない・設計の判断が要る）dnszone・nftables では、scope 付きの intent の `PruneWithinScope` が何も消さず、知らせも出ない。**
   この 2 つの provider の List は現実を root scope（scope なし）で返す。prune は「その scope の現実 − desired」なので、
   `scope: {site: a}` の intent からレコードを外しても、現実側にその scope のものが 1 つも無く、`a-api` は残る。claim を付けても
   付けなくても同じ（`sakutei plan --intent` でも `Converged (no changes)`）。消し過ぎない方向なので安全ではあるが、
   claim でシャーディングする構成ではファイル系 provider の prune が実質使えず、しかも黙っている。
   provider が現実の scope を知る手段（ゾーンファイルのどこに書くか）を決めるか、scope 付きの `PruneWithinScope` に
   List が root scope しか返さない provider を組み合わせたら warn を出すか、の判断が要る。
4. **（直していない・設計どおり）`FENCED` は fence されている間、毎周 2 行ずつ出続ける。** §22.3 で fencing は edge の Event と
   決めてあり、§17.6 も「再起動しないと `FENCED` を出し続ける」としている。claim を `--claim` で固定したシャードを再シャードの
   後に再起動し忘れると、ログがこの行で埋まる。level にする（始まりと解消に 1 回ずつ）かは設計の判断。
5. **（直していない・小さい）デーモンの起動行が `state=sakutei-fake-state.json` と出る。** `--zone` だけで動かしていても、
   使っていない fake provider の `--state` の既定値が出る。紛らわしいが動作には関わらない。
   **（#111 で直した）** 起動行はコマンドラインで渡したファイル（`zone=…` など）だけを出す。何も渡さないときだけ `state=sakutei-fake-state.json (default)`。`[AC-111-5]`。
6. **（直していない・設計の判断が要る）fencing を付けた拠点は、fencing のストアに届かない間ずれを直さない。**
   `--fencing-store` は全シャードで同じものを見るので、実際の多拠点では拠点ホストの外（中央）に置くことになる。
   §15.2 の決まり「ストアに届かない周は飛ばす（世代を確かめられないまま書かない）」により、WAN 断で fencing ストアにも
   届かない拠点は Observe も Apply もしない。手順 4 の「WAN 断の間も前回の intent で自己修復を続ける」（§17.4）は、
   fencing ストアが届く場所にあったから成り立った。手順 4b で fencing ストアのディレクトリを読めなくすると：

   ```
   sakutei: cycle 4 error: fencing store unreachable; cycle skipped (no Observe/Apply/prune): fencing store lock: open …/fz/store/fencing.json.lock: permission denied
   ```

   その間ゾーンの `b-www` はずれたままで、届くようになった周に直る。原則 8（衛星の停止は維持を止めない）・§17.4 の
   「WAN 断の拠点でも収束は回り続ける」と、§15.2 の「確かめられないなら書かない」がぶつかっている。拠点の claim は
   その拠点しか持たない（二重 claim が起きにくい）ことを前提に、届かない間は手元の世代で維持だけ続ける（lease の考え方）
   か、今のまま止めるかは設計の判断。

## この試用で試していないこと

- in-flight の Apply を待ってから手を引く（`draining in-flight Applies`）こと：dnszone の Apply はすぐ終わるので、待つ場面を作れなかった。
  `AC-v1b-claim-git-4` と fencing のテストが見ている。
- 資格局所性（`sakutei secrets extract`・`--secret-store`、§20.5）：`AC-v1b-claim-git-5` が見ている。
- `--claim` で claim を固定したシャードの再シャード（§17.6 の手動手順）：手順 2 の「止めて起こし直す」が同じ動き。
