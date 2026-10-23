# tier 別の Observe 頻度・BulkObserve・status とデーモンの状態を手で試す

v1b の tier 別の Observe 頻度（`observe_tiers`、設計 §16.5）、BulkObserve（§5.7・§16.3）、適合レポート `sakutei status`（§26.4）と
デーモンの状態ファイル `--daemon-status`（#15、§9.2）は、これまで自動テスト（`tests/acceptance/tier_observe_test.go`・
`bulk_observe_file_test.go`・`bulk_observe_test.go`・`status_test.go`・`unreachable_skip_test.go`）でしか確かめていなかった。
ここでは実バイナリの常駐デーモンを 1 秒周期で回し、dnszone（ゾーンファイル）と nftables（ruleset ファイル）を相手に手で通す。
外のサービスには繋がない。

2026-10-23 にこのコンテナ（Go 1.22 以上）で `run.sh` を流し、35 の観点がすべて OK になった（見つけた不具合 3 件を直した後。
3 回続けて流し、3 回とも NG 0）。

## 確かめる観点（設計書と受け入れテストから）

| # | 観点 | 設計 |
|---|---|---|
| 1 | BulkObserve を宣言した provider（dnszone・nftables）は、常駐ループでバルク経路を使う。それがログで見分けられる | §5.7 |
| 2 | `observe_every: N` の tier のリソースは、収束しているあいだ N 周に 1 回だけ Observe される。tier の無いリソースは毎周。間引いた周も「converged」で、unobservable にはしない | §16.5 |
| 3 | 間引いている間に手で変えたずれは、次に予定された周（最大 N 周後、tier なしより最大 N−1 周遅れ）で拾われて直る。直した後は収束するまで毎周 Observe する | §16.5 |
| 4 | intent を変えたリソースは間引かず、次の周に Apply する（cause A） | §16.5 |
| 5 | 間引きの記録は S3：再起動の周 1 は全部を Observe する | §16.5・§3 |
| 6 | `sakutei status` はデーモンの間引きと関係なく、そのとき Read で全部を観測して drifted を出す。`--daemon-status` でデーモンの周と飛ばした周の数が見える | §26.4・#15 |
| 7 | BulkObserve は complete-or-error：ファイルが読めないと、その周の provider の全リソースを unobservable にし、Read に回して部分的に答えない。Apply も prune もしない | §5.7 |
| 8 | BulkObserve の結果は per-resource Read と等しい（`BulkObserve(scope) ≡ {Read(id)}`）。素直なファイルでも、手で書いた癖のあるファイルでも | §5.7・§28.3 |
| 9 | 外部入力（inventory）が最初から届かない周は halt-safe に飛ばし、連続回数をログと status に出す。上限で `ExternalInputUnreachableProlonged`。届けば 0 に戻り収束を再開する | §9.2・#15 |

## 置いてあるもの（docs/trial/observe/）

| ファイル | 中身 |
|---|---|
| `initial/example.com.zone` | 最初の現実。`www`・`api`・`mail` の A レコード（intent と一致） |
| `initial/firewall.rules` | 最初の現実。base chain の `input` と、管理する `sakutei_allow`（22 番と 443 番。intent と一致） |
| `intent/main.yaml` | `observe_tiers` に `stable`（`observe_every: 4`）。`www` は tier なし、`api`・`mail`・nftables の `sakutei_allow` が `stable` |
| `messy/example.com.zone`・`messy/firewall.rules` | 手順 7 の、手で書いた癖のあるファイル（相対名・大文字・同じ名前の 2 行目・`$TTL`・途中の `$ORIGIN`・CNAME・MX・TXT／同じ chain を 2 回書く・空の chain・別の table） |
| `bulkeq/main.go` | 手順 7 の道具。SDK の `BulkEquivalenceTester` を実ファイルに当て、BulkObserve と Read の観測を並べて出す（`go build -tags trial` でだけビルドされる） |
| `run.sh` | 手順 1〜11 を通して流し、観点ごとに OK／NG を出すスクリプト |

## 通しで流す

```sh
docs/trial/observe/run.sh            # 作業ディレクトリは /tmp/sakutei-trial-observe（毎回作り直す）
```

Go が要る。リポジトリのファイルは書き換えない。1 分ほどかかる。デーモンのログは作業ディレクトリの `logs/` に、
status の出力は `status-*.txt` に残る。デーモンは次の形で起こす：

```sh
./sakutei --intent intent --zone example.com.zone --ruleset firewall.rules --interval 1s --daemon-status ds.json
./sakutei status --intent intent --zone example.com.zone --ruleset firewall.rules --daemon-status ds.json   # 別の端末から
```

周ごとの `observed=N` が「その周に Observe したリソースの数」、`N resource(s) not due for Observe (tier thinning)` が
「間引いた数」である。どのリソースを見たかは数で見分ける（この intent では、4 なら全部、1 なら `www` だけ）。

## 手順と結果（2026-10-23）

### 1. BulkObserve の経路

デーモンは provider ごとに、最初に Observe するときに 1 度だけ経路を出す（この試用で足した。見つけたこと 1）。

```
sakutei: provider dnszone: Observe via BulkObserve (one call per cycle for the claim, §5.7)
sakutei: provider nftables: Observe via BulkObserve (one call per cycle for the claim, §5.7)
```

BulkObserve を持たない provider（Redfish など）は `Observe via per-resource Read (no BulkObserve, §5.7)` になる。
バルク経路が毎周使われていることは、手順 6（壊れたファイルのエラーが `bulk observe (complete-or-error, §5.7)` から出る）でも分かる。

### 2. 間引き

初期の現実は intent と一致しているので、周 1 から収束している。

```
周  observed  間引いた数
  1         4           0
  2         1           3
  3         1           3
  4         1           3
  5         4           0
  6         1           3
  ...
 13         4           0
```

`stable` の 3 つ（nftables の唯一のリソースを含む）は周 1・5・9・13 にだけ Observe され、`www` は毎周。間引いた周の行は
`cycle 2: observed=1 converged (no changes)` と `cycle 2: 3 resource(s) not due for Observe (tier thinning)`。

### 3. 間引いている間のずれ

`stable` を Observe した周 13 の直後に、ゾーンの `api` を 198.51.100.30 に書き換えた。

- その直後の `sakutei status` は、デーモンの間引きと関係なく `DRIFTED dnszone//api.example.com./A`（原因タグ B）を出す。
  status は毎回すべてを Read で観測するからである。ずれた値は status には出ないので（見つけたこと 4）、`sakutei plan` で見ると
  `~ value "198.51.100.30" → "192.0.2.30"`。
- デーモンが拾ったのは 4 周目の周 17（tier なしなら周 14。3 周遅れ＝ N−1）：

  ```
  sakutei: DriftEvent: dnszone//api.example.com./A [cause: B] drift B: value was "192.0.2.30" prev cycle now "198.51.100.30" ...
  sakutei: cycle 17: observed=4 applied=0 launched=1
  sakutei: cycle 18: observed=2 converged (no changes)
  sakutei: cycle 18: RESOLVED Drift dnszone//api.example.com./A: resolved after 1 cycle(s) ...
  ```

  DriftEvent の値（BulkObserve で見た）と plan の値（Read で見た）は同じ。直した次の周 18 は `api` を間引かず（`observed=2`：
  `www` と `api`）、周 19 からまた間引く（`observed=1`）。
- tier なしの `www` を周 19 の後に書き換えると、次の周 20 に拾って直す。
- nftables の `sakutei_allow`（`stable`）を、それを Observe した周 21 の後に書き換える（443 → 4443）と、4 周目の周 25 に拾って直す。
  この間 nftables の Observe は 1 つも無い。

### 4. intent の変更は間引かない

周 26 の後に intent の `mail`（`stable`）の値を 192.0.2.44 に変えると、次の周 27 に `observed=2 applied=0 launched=1` で Apply する
（`mail` は周 25 に Observe したばかりで、間引きなら周 29 まで見ない）。

### 5. status と --daemon-status

```
sakutei status — 適合レポート（conformance report; read-only, §26.4）
...
適合（drift なし）: 宣言どおり在る。

デーモン（cycle 28, 2026-10-23T15:58:42Z 時点）: 外部入力不達による連続スキップ consecutive_unreachable_skips=0（上限 3）

要約: drifted=0 converged=4 unobservable=0 unmanaged=0
```

`ds.json`（デーモンが毎周書き直す）：

```json
{
  "cycle": 28,
  "consecutive_unreachable_skips": 0,
  "unreachable_skip_limit": 3,
  "consecutive_error_cycles": 0,
  "interval": "1s",
  "updated_at": "2026-10-23T15:58:42.794356443Z"
}
```

`consecutive_error_cycles`・`last_error`・`interval` はこの試用で足した（見つけたこと 2。手順 9・10）。

### 6. ゾーンファイルが壊れたら（complete-or-error）

ゾーンファイルの末尾に `oops this is not a record` を足す。

```
sakutei: observe dnszone//www.example.com./A: bulk observe (complete-or-error, §5.7): transient: dnszone: BulkObserve: parse zone example.com.zone: line 7: unknown record type "this": ...
sakutei: prune halted this cycle: List returned error: transient: dnszone: List: parse zone ...
sakutei: cycle 29: observed=0 halted (0 op(s) held) applied=0 launched=0 skipped=1
sakutei: cycle 30: 2 resource(s) skipped (retry next cycle): [www.example.com./A api.example.com./A]
sakutei: cycle 32: 3 resource(s) skipped (retry next cycle): [www.example.com./A api.example.com./A mail.example.com./A]
```

- エラーは BulkObserve の呼び出し全体のエラーとして出て、`dnszone: Read:` は 1 度も出ない（Read に回して部分的に答えない）。
  ゾーンファイルは書き換えない。prune も止まる（List も同じファイルを読めない）。
- 間引きは続く：`api`・`mail` はそれぞれの予定の周（30・32）に来てから unobservable になり、以後は直るまで毎周見る。
- nftables は同じ周も影響を受けない。
- このとき `sakutei status` は `sakutei status: observe dnszone//www.example.com./A: transient: dnszone: Read: parse zone ...` で
  終了コード 1 になり、nftables の適合も出さない（見つけたこと 5）。
- ファイルを戻すと次の周から収束に戻り、`RESOLVED PruneHalted provider=dnszone` が出る。

### 7. BulkObserve ≡ Read（実ファイルに BulkEquivalenceTester）

`bulkeq` は SDK の `BulkEquivalenceTester`（宣言と実装の一致・List の全 ID がバルクに有る・ObservedAt・Availability と State が
Read と等しい・List に無いものをバルクが返さない）を実ファイルに当て、両方の観測を並べる。

- 収束した作業ディレクトリのファイル：dnszone 3/3、nftables 1/1 が等しい。
- `messy/`（手で書いた癖のあるファイル）：dnszone 8/8、nftables 3/3 が等しい。例：

  ```
    www.example.com./A
        bulk name="www.example.com." ttl="300" type="A" value="192.0.2.10"
     == read name="www.example.com." ttl="300" type="A" value="192.0.2.10"
    inet/filter/sakutei_allow
        bulk chain="sakutei_allow" family="inet" rules="tcp dport 22 accept ; tcp dport 443 accept" table="filter"
     == read chain="sakutei_allow" family="inet" rules="tcp dport 22 accept ; tcp dport 443 accept" table="filter"
  ```

  大文字の `WWW`（2 行目）はどちらの経路でも最初の行が勝ち、`$ORIGIN sub.example.com.` の後の `deep` は `deep.sub.example.com.` になる。
  最初は nftables が「BulkObserve returned 3, List 4」で NG だった（見つけたこと 3。直した）。

### 8. 再起動

起こし直した周 1 は全部（`observed=4`）、周 2 からまた間引く（`observed=1`）。間引きの記録はメモリだけにある。

### 9. 周がエラーで終わり続ける

intent を壊す（`resources: {broken`）と、毎周 `cycle N error: load intent: ...` になる。status は：

```
デーモン（cycle 6, 2026-10-23T15:58:55Z 時点）: 外部入力不達による連続スキップ consecutive_unreachable_skips=0（上限 3）
  警告: 周がエラーで終わり続けている consecutive_error_cycles=3 — 収束が止まっている。最後のエラー: load intent: loader: parse intent file intent/main.yaml: yaml: 2 行目: フロースタイル（{}/[]）は非対応です
```

直すと次の周に `consecutive_error_cycles` が 0 に戻る。

### 10. デーモンが止まる

デーモンを止めて 4 秒（周期 1s の 3 倍超）待つと、status は：

```
デーモン（cycle 7, 2026-10-23T15:58:56Z 時点）: 外部入力不達による連続スキップ consecutive_unreachable_skips=0（上限 3）
  警告: このファイルは周期（1s）の数倍のあいだ更新されていない — デーモンが止まっているかもしれない。
```

### 11. 外部入力が届かず飛ばした周（#15）

別の作業ディレクトリ `inv/` で、まだ無い inventory ファイルを `--inventory-file` に渡して起こす。

```
sakutei: ExternalInputUnreachable (first call): external input unreachable [inventory inv/inventory.yaml]: ... no previous inventory to fall back to; skipping cycle (halt-safe, §9.2)
sakutei: cycle 1: SKIPPED (external input unreachable, no previous value): consecutive_unreachable_skips=1
sakutei: EVENT CycleSkipped: external input unreachable with no previous value; ...
sakutei: cycle 3: SKIPPED (external input unreachable, no previous value): consecutive_unreachable_skips=3
sakutei: EVENT ExternalInputUnreachableProlonged severity=critical: 3 consecutive cycles skipped (limit 3); ...
sakutei: cycle 4: SKIPPED (external input unreachable, no previous value): consecutive_unreachable_skips=4
```

status：

```
デーモン（cycle 4, 2026-10-23T15:59:04Z 時点）: 外部入力不達による連続スキップ consecutive_unreachable_skips=4（上限 3）
  警告: ExternalInputUnreachableProlonged — 外部の権威が長く届かず、収束が止まっている（§9.2）。
```

飛ばした周はゾーンに触れない。inventory ファイルを置くと：

```
sakutei: cycle 5: observed=2 applied=0 launched=1
sakutei: cycle 5: external input reachable again after 4 skipped cycle(s)
sakutei: cycle 5: RESOLVED CycleSkipped: resolved after 4 cycle(s) ...
sakutei: cycle 5: RESOLVED ExternalInputUnreachableProlonged: resolved after 2 cycle(s) ...
```

inventory の `h1` が入り、status の連続スキップ回数は 0 に戻る。

## 見つけたこと

1. **（直した）BulkObserve の経路が使われているかをログで見分けられない。** 常駐ループは BulkObserve を宣言した provider に
   周ごとに 1 回のバルク呼び出しをするが、平常時はそれを示す出力が何も無く、per-resource Read との違いは壊れたファイルの
   エラー文でしか分からなかった。provider ごとに最初の Observe のときに 1 度だけ、どちらの経路かを出すようにした
   （`internal/reconcile/loop.go` の `observePathText`、テスト `TestDaemon_LogsObservePathOncePerProvider` [AC-104-1]）。
2. **（直した）`--daemon-status` が、収束が止まっているデーモンを健全に見せる。** ファイルに載るのは #15 の「外部入力不達で
   飛ばした周」だけで、(a) 周がエラーで終わり続ける場合（intent が読めない、fencing のストアに届かない〈sharding の
   見つけたこと 6〉など）も `consecutive_unreachable_skips=0` で何も警告しなかった。(b) デーモンが止まってもファイルは残り、
   古い「cycle N, skips=0」が出続けた（git の intent source に最初から届かない周は、ファイルを書かずに飛ばすのでこれに当たる）。
   ファイルに `consecutive_error_cycles`・`last_error`・`interval` を足し、status は周のエラーが続いていれば警告し、
   更新が周期の 3 倍より長く途絶えていれば「止まっているかもしれない」と警告するようにした
   （`internal/statusreport/daemonhealth.go`、テスト `TestDaemonStatus_ReportsErrorCycles` [AC-104-2]・`TestDaemonHealth_ErrorCyclesAndStale`）。
3. **（直した）nftables の List が、同じ chain を 2 回書いた ruleset で同じ ID を 2 回返す。** List は ID の集合（§5.4）で、
   Read と BulkObserve は最初の出現で 1 つにしているのに、List だけが重複していた（BulkEquivalenceTester が「BulkObserve returned 3,
   List 4」で検出）。prune の急減の基準を水増しし、import は同じ ID を 2 回書き出しうる。dnszone の List と同じく最初の出現で
   1 回だけ出すようにした（テスト `TestBulkObserve_NFTablesEquivalentToRead` に追加）。
4. **（直していない・小さい）`sakutei status` の DRIFTED に、ずれている値が出ない。** 「1 field(s) out of sync」とだけ出て、
   どのフィールドが何になっているかは `sakutei plan` で見るしかない。§26.4 は drifted の一覧だけを求めているので設計の範囲内だが、
   人が見る適合レポートとしては足りない。
5. **（直していない・小さい）`sakutei status` は 1 つのリソースの観測エラーで終了コード 1 になり、他の provider の適合も出さない。**
   ゾーンファイルが壊れると、nftables が適合していることも見えない。Availability が unknown のときは unobservable に数えて
   続けているので、エラーも同じく unobservable に数えて最後まで出すか（unknown ≠ absent）、今のまま止めるか。
6. **（直していない・設計の判断が要る）`status`・`plan`・`reconcile` には `--inventory-file` が無い。** inventory で合成して回す
   デーモンと desired が違い、inventory 由来のリソースは status で UNMANAGED（宣言外）に見える（手順 11 の `h1`）。
   一回性のコマンドにも inventory を渡せるようにするかの判断が要る。
7. **（直していない・設計の判断が要る）間引いた周も List は毎周呼ぶ。** prune の急減の基準（§12.4b）を新しく保つため、
   List を持つ provider は intent の消し方（`ExplicitAbsentOnly` か）に関係なく毎周 List する（手順 6 で、間引いた周にも
   `prune halted: List returned error` が出る）。ファイル系では BulkObserve 1 回と List 1 回で、間引いても読む回数は変わらない。
   §16.5 の狙い（Observe の時間を減らす）は Diff の分しか効かず、実 API の provider（#49）では List の毎周のコストが §16 の
   天井算術に残る。prune する宣言が無い provider の List を間引くかは設計の判断。
8. **（設計どおり・記録）dnszone は `$TTL` を解釈しない**（TTL の無い行は 300。`internal/dnszone` に書いてある）。`messy/` の
   `api IN A` は BulkObserve でも Read でも ttl=300。nftables は同じ chain の 2 回目の塊を見ない（最初の出現で決める）が、
   `nft -f` は 2 つの塊を 1 つの chain に足し合わせるので、2 回目の `tcp dport 8080 accept` は Sakutei には見えない。

## この試用で試していないこと

- 早期収束パス（§8）の per-resource Read：dnszone・nftables は `DesiredSource.Watch()` を持たないので、早期収束の経路に乗らない。
- 実 API の一括観測（Proxmox `/cluster/resources` など、#49）。ファイル系の BulkObserve は「1 回読めば全件分かる」素直なものだけ。
- 間引いた周にファイルを本当に読んでいないこと（読む回数）は、この環境に strace が無いので数えていない。数は受け入れテスト
  `TestBulkObserve_DaemonCycleReadsFileOnce`・`tier_observe_test.go` が数えている。
