# DR モード（DR plan・break-glass の緊急書込と署名・到達不能の判定・事後合流）を手で試す

v1.5 の DR モード（設計 §25.1・§25.4・§29.1、Backlog #45）は、これまで自動テスト
（`tests/acceptance/dr_mode_test.go`・`internal/gitdist/dr_test.go`）でしか確かめていなかった。ここでは実バイナリの常駐デーモン
3 本（東京・大阪・大阪の DR シャード）と常駐の `sakutei mirror` を立て、本物の git（東京にあるローカルの bare リポジトリを中央 git に、
`sakutei mirror` で作った大阪のローカルミラーを大阪の pull 元に）を相手に、手で通す。外のサービスには繋がない。
中央に届かない状態は、bare リポジトリを動かす（東京ごと `mv`）、権限で弾く（`chmod 000`）、
応答しない中央（`git daemon` を `SIGSTOP`。TCP はつながるが返事が無い）で作る。

2026-10-23 にこのコンテナ（git 2.43.0、Go 1.22 以上）で `run.sh` を流し、73 の観点がすべて OK になった（見つけた不具合 4 件を直した後。
設計の判断が要るもの 2 件は「見つけたこと」の行として出る）。

## 確かめる観点（設計書と受け入れテストから）

| # | 観点 | 設計 |
|---|---|---|
| 1 | 平時、DR シャード（`shards/osaka-dr.yaml` が `claim:` 空）は idle。東京のシャードが site=tokyo を持つ | §25.1 (a) |
| 2 | 宣言の矛盾（`enabled` の書き忘れ・`to` が平時 idle でない）は、その周を飛ばす。飛ばしている間はずれも直さない。戻せば再開する | §25.1「v1.5 の実装」 |
| 3 | 中央が健在なら、プリステージした plan の `enabled: false → true` の **1 行**だけで DR シャードが東京の scope を引き継ぎ（`DRPlanActive`・fencing の世代を上げる・ずれを直す）、戻ってきた東京は idle に留まる。`false` に戻せば東京が取り戻す。引き継ぎで何も prune しない | §25.1 (a)・§17.7・§25.4 |
| 4 | break-glass は中央に**届かないときだけ**書ける。判定は中央（ミラーではない）への `git ls-remote` を 1 回 10s で 10s おきに 3 回、すべて失敗したとき。1 回でも届けば（途中で戻っても）拒否。3 回未満の判定は受け付けない | §29.1 |
| 5 | actor・理由・S4 の指定が無い、S4 に書けない、手元がミラーの先端にいない、ときは書かない（ミラーにも S4 にも何も残らない） | §25.1 (b) |
| 6 | 書いたコミットは ed25519 で署名され（trailer）、`sakutei breakglass verify` で確かめられる。別の鍵・改竄（署名を写して tree を変えた）は通らない。S4 に `breakglass.write`、`BreakGlassWriteUsed`（critical）Event。拠点の常駐ループも届いた break-glass コミットごとに Event を出す | §25.1 (b) |
| 7 | session の間、ミラーは中央を追わず break-glass の先端を配り続ける。同じ session に 2 つ目も書ける | §25.1「v1.5 の実装」 |
| 8 | 中央が戻ると、break-glass コミットをそのまま（rebase・squash せず）`breakglass/<session>` として中央へ push し、PR の下書き（コミット・実行者・鍵・両側で変わったパス・取り込み方）を書き、S4 に `breakglass.merge_back.opened`。中央が進んでいれば両側で変わったパスだけを並べ、自動では解かない | §25.1・§29.1 |
| 9 | 中央が break-glass の先端を含むまで（merge commit で取り込むまで）ミラーは中央を追わない。取り込まれると session を閉じて追従に戻り、S4 に `breakglass.merged`・`breakglass.key.retired`。失効した鍵では二度と書けず、rotate した鍵なら書ける | §25.1 |
| 10 | 既知の限界：squash merge では祖先関係が残らないので session が閉じない | §25.1「既知の限界」 |
| 11 | 判定の変種：応答しない中央は 1 回 10s で切って「届かない」に数える。権限で弾かれる中央（認証の失敗の代わり）も「届かない」に数える | §29.1 |

## 置いてあるもの（docs/trial/dr-mode/）

| ファイル | 中身 |
|---|---|
| `repo/` | 中央の intent リポジトリの最初の中身。`intent/site=tokyo`（`tk-web` size 2・`tk-db` size 4）・`intent/site=osaka`（`os-web` size 2）と、`shards/` の 3 シャード（`tokyo`・`osaka`・平時 idle の `osaka-dr`）、プリステージした DR plan `shards/dr/tokyo-to-osaka.yaml`（`enabled: false`） |
| `initial/reality.json` | 現実（fake provider の state。空から始める） |
| `run.sh` | 手順 0〜7 を通して流し、観点ごとに OK／NG を出すスクリプト |

## 通しで流す

```sh
docs/trial/dr-mode/run.sh            # 作業ディレクトリは /tmp/sakutei-trial-dr（毎回作り直す）
```

Go と git（`git daemon` を含む）と python3 が要る。リポジトリのファイルは書き換えない。判定を既定の 10s×3 のまま回す所があるので 4 分ほどかかる。
各デーモンのログは作業ディレクトリの `logs/` に残る。

作業ディレクトリの中の配置：

| パス | 見立て |
|---|---|
| `tokyo/central.git` | 中央 git（bare）。東京にあり、被災で東京ごと落ちる（`mv tokyo tokyo.down`） |
| `work/` | 平時の人の手元。ここでコミットして中央へ push する = PR のマージ |
| `osaka/mirror.git` | 大阪のローカルミラー（§25.2）。常駐の `sakutei mirror --interval 1s` が中央を追う |
| `op/` | 大阪の運用者の手元（ローカルミラーの clone）。break-glass の緊急書込はここで作る |
| `offline/` | break-glass 鍵（平時に `sakutei breakglass keygen` で作ってオフライン保管しておくもの） |
| `s4/audit.jsonl` | S4 監査（append-only。拠点の外） |
| `hosts/<シャード>` | 各デーモンのホストのディスク（`--workdir`） |
| `reality.json`・`fencing.json` | 現実（fake provider）と、全シャードで共有する fencing の世代のストア |

デーモンはどれも次の形で起こす（`run.sh` の `start`。東京は中央を直接、大阪の 2 本はミラーを読む）：

```sh
./sakutei --intent-repo file://$W/osaka/mirror.git --workdir hosts/osaka-dr --shard osaka-dr --shard-id osaka-dr \
  --git-poll 1s --interval 1s --state $W/reality.json --fencing-store $W/fencing.json
```

break-glass の緊急書込は次の形（既定の判定 10s×3）：

```sh
./sakutei breakglass commit --repo op --key offline/bg1.key --actor osaka-oncall \
  --audit-log s4/audit.jsonl --reason "DR: tokyo is down, activate tokyo-to-osaka"
```

## 手順と結果（2026-10-23）

### 1. 平時

3 本を起こすと `tk-web`・`tk-db`・`os-web` が入り、`osaka-dr` は `shard claims nothing at revision …; idle`。

```
SCOPE                                    GENERATION  HOLDER                         ACQUIRED
site=osaka                                        1  osaka                          2026-10-23T16:18:02Z
site=tokyo                                        1  tokyo                          2026-10-23T16:18:02Z
```

### 2. 宣言の矛盾

- 2a：plan から `enabled:` の行を消してマージすると、plan に関わらない `osaka` も含めて全シャードが周を飛ばす。
  その間に `os-web` を 9 にずらしても直さない（3 秒見て 9 のまま）。plan を戻すと直す。

  ```
  sakutei: intent source unavailable; cycle skipped (no Observe/Apply/prune): gitdist: shards/dr/tokyo-to-osaka.yaml: on_activation.enabled must be true or false (written explicitly)
  ```
- 2b：`to` を平時 `site=osaka` を持つ `osaka` にして `enabled: true` でマージすると、`osaka` も `tokyo` も周を飛ばし、
  `tokyo` は claim を手放さない（holder は tokyo のまま）。**直す前は `tokyo` だけが plan に従って idle になり、
  `osaka` は周を飛ばすので、誰も site=tokyo を収束させなかった**（見つけた不具合 3）。

  ```
  sakutei: intent source unavailable; cycle skipped (no Observe/Apply/prune): gitdist: DR plan tokyo-to-osaka: standby shard "osaka" already claims site=osaka; a DR shard must stand by idle
  ```

### 3. DR 訓練（中央は健在）

東京のデーモンを止め、plan を `enabled: true` にしてマージする。差分は `1	1	shards/dr/tokyo-to-osaka.yaml`（1 行）。

```
sakutei: EVENT DRPlanActive: DR plan tokyo-to-osaka is enabled at revision 6d641bdde6ee; this shard's claim follows it
sakutei: fencing: scope site=tokyo taken over from "tokyo" (generation 1 -> 2); the previous holder is fenced from now on
```

`tk-web` を 7 にずらすと DR シャードが 2 に戻す。東京を起こし直すと `DRPlanActive` を出して idle に留まり、holder は osaka-dr のまま。
`enabled: false` に戻すと東京が `taken over from "osaka-dr" (generation 2 -> 3)`、DR シャードは `claim released`。どちらのログにも `pruned=1` 以上は無い。

### 4. 被災：東京（中央 git ごと）が落ち、大阪は break-glass で DR plan を発動する

中央が落ちると PR で `enabled` を切り替えられない。大阪の運用者はローカルミラーの clone（`op/`）で plan を `enabled: true` にし、break-glass で書く。

- 4a：中央に届くうちは拒否（1 回目で届いた）。

  ```
  sakutei breakglass: probe 1/3: the central git /tmp/sakutei-trial-dr/tokyo/central.git answered
  sakutei breakglass: breakglass: the central git /tmp/sakutei-trial-dr/tokyo/central.git is reachable (probe 1/3 answered); write through the normal PR flow
  ```
- 4b：東京を落としてから判定を始め、3 秒後に戻す。2 回目で届いて拒否。

  ```
  sakutei breakglass: probe 1/3: /tmp/sakutei-trial-dr/tokyo/central.git unreachable: git ls-remote: exit status 128: fatal: '/tmp/sakutei-trial-dr/tokyo/central.git' does not appear to be a git repository
  sakutei breakglass: probe 2/3: the central git /tmp/sakutei-trial-dr/tokyo/central.git answered
  sakutei breakglass: breakglass: the central git /tmp/sakutei-trial-dr/tokyo/central.git is reachable (probe 2/3 answered); write through the normal PR flow
  ```
- 4c：actor・reason・S4 の指定が無い、S4 に書けない（`/proc/nope/audit.jsonl`）、`--probe-attempts 2`、のどれも拒否。ミラーの先端は変わらず、S4 は空のまま。

  ```
  sakutei breakglass: breakglass: refusing to write: audit: open /proc/nope/audit.jsonl: ...
  sakutei breakglass: breakglass: at least 3 probe attempts are required to call the central git unreachable
  ```
- 4d：既定の判定（10s×3）で書ける。判定に 20 秒かかった。ミラーの先端が break-glass コミットになり、S4 に `breakglass.write`（コミット・署名・鍵・actor）。

  ```
  sakutei breakglass: probe 1/3: /tmp/sakutei-trial-dr/tokyo/central.git unreachable: git ls-remote: exit status 128: fatal: ... does not appear to be a git repository
  sakutei breakglass: probe 2/3: ...
  sakutei breakglass: probe 3/3: ...
  sakutei: EVENT BreakGlassWriteUsed severity=critical: osaka-oncall wrote b794fea3d2cc... to the local mirror /tmp/sakutei-trial-dr/osaka/mirror.git with key sha256:61736139a0e5188d06005c97eabd241f (session bg-20261023T161847Z-61736139); recorded in S4 /tmp/sakutei-trial-dr/s4/audit.jsonl; the central /tmp/sakutei-trial-dr/tokyo/central.git is unreachable
  ```

  コミットの中身（`git cat-file commit`）：

  ```
  author osaka-oncall <break-glass@sakutei.invalid> 1792772327 +0000
  committer osaka-oncall <break-glass@sakutei.invalid> 1792772327 +0000

  break-glass: DR: tokyo is down, activate tokyo-to-osaka

  Sakutei-Break-Glass: bg-20261023T161847Z-61736139
  Sakutei-Break-Glass-Actor: osaka-oncall
  Sakutei-Break-Glass-Key: sha256:61736139a0e5188d06005c97eabd241f
  Sakutei-Break-Glass-Signature: hrY4of7w3pHbj/XSHVSTBzReFHKL6aeM3Mo+u/uza/mflXMprDw5LprZfz58oDRAOcyjMGEyCNf9y9bu2p9TAg==
  ```

  `verify` は正しい鍵で `ok`、別の鍵・改竄したコミット（メッセージと署名をそのまま写して tree を変えた）で `FAIL … signature does not verify`。
- 4e：大阪の 2 本の常駐ループが `EVENT BreakGlassWriteUsed severity=critical: break-glass commit b794fea3d2cc arrived at revision …` を出し、
  DR シャードが `DRPlanActive` を出して `site=tokyo taken over from "tokyo"`。`tk-db` を 1 にずらすと 4 に戻す。
  ミラーは `holding break-glass session …: central git still unreachable; serving the break-glass head …` で先端を保つ。
- 4f：手元がミラーの先端にいない clone からは `pull it first` で拒否。同じ session に 2 つ目（`tk-web` を 2 → 3）を書くと DR シャードが 3 にする。
  `breakglass status` は `2 commit(s) since …`・`merge-back: not opened yet (central unreachable)`。`mirror --once` は失敗して先端を保つ。

### 5. 中央が戻る：事後合流

東京を戻し、直後に平時の手元から（break-glass を知らずに）`tk-web` を 6・`tk-db` を 8 にし、別のファイル（`intent/site=osaka/notes.yaml`）も足してマージする。

- 常駐のミラーが `merge-back branch breakglass/<session> pushed to the central; PR draft at …` を出す。中央の `breakglass/<session>` は break-glass の先端そのもの。
  S4 に `breakglass.merge_back.opened`。PR の下書き（抜粋）：

  ```markdown
  | コミット | 実行者 | 鍵 | 件名 |
  |---|---|---|---|
  | `b794fea3d2cc` | osaka-oncall | `sha256:61736139a0e5188d06005c97eabd241f` | break-glass: DR: tokyo is down, activate tokyo-to-osaka |
  | `c642189db8b1` | osaka-oncall | `sha256:61736139a0e5188d06005c97eabd241f` | break-glass: DR: grow tk-web for the osaka load |

  ## 競合

  中央は base から 1 コミット進んでいます。

  **両側で変わったパス（競合の可能性）:**

  - `intent/site=tokyo/vms.yaml`
  ```

  両側で変わった `intent/site=tokyo/vms.yaml` だけが並び、中央だけが変えた `notes.yaml` は並ばない。
- 取り込まれるまでミラーは中央を追わない（先端は break-glass のまま）。大阪の DR シャードが収束させる現実も緊急書込のまま（`tk-web` 3・`tk-db` 4）。
- 5b：合流前に東京のデーモンを起こし直す（東京は中央を直接読む。中央はまだ `enabled: false`）。東京が `site=tokyo taken over from "osaka-dr"` で取り返し、
  `tk-web` 6・`tk-db` 8（中央の値）にする。緊急書込が巻き戻る（「見つけたこと」1）。
- 5c：人が `git merge --no-ff` で取り込む。同じ行（`tk-web`）が両側で変わっているので競合する。break-glass 側を取り、中央の `tk-db` 8 を載せ直して merge commit（親 2 つ）にする。
  ミラーは `break-glass session bg-20261023T161847Z-61736139 merged into the central main (c4c9d722c06a); key(s) [sha256:61736139a0e5188d06005c97eabd241f] retired — rotate them` で session を閉じて中央に追いつき、`sakutei-breakglass.json` が消える。
  東京は合流後の中央（`enabled: true`）で `claim released`。**ところが FENCED になっていた DR シャードは取り戻さず、誰も site=tokyo を収束させない**（「見つけたこと」2）。
  DR シャードを起こし直すと取り戻し、現実は `tk-web` 3・`tk-db` 8 になる。中央で `verify --range` すると 2 つの break-glass コミットがどちらも `ok`。S4：

  ```
  breakglass.write bg-20261023T161847Z-61736139 b794fea3d2cc11f55c445adab1d7147e1cbdc9ef
  breakglass.write bg-20261023T161847Z-61736139 c642189db8b1b602fae36f2491b90eac954e4d68
  breakglass.merge_back.opened bg-20261023T161847Z-61736139 c642189db8b1b602fae36f2491b90eac954e4d68
  breakglass.merged bg-20261023T161847Z-61736139 c642189db8b1b602fae36f2491b90eac954e4d68
  breakglass.key.retired bg-20261023T161847Z-61736139 sha256:61736139a0e5188d06005c97eabd241f
  ```
- 5d：もう一度東京を落とし、同じ鍵で書こうとすると判定の前に拒否：
  `key sha256:… was retired after its session merged back; rotate to a new break-glass key`。新しく作った鍵なら書け、東京を戻すと 2 つ目の session の合流ブランチが push される。

### 6. 既知の限界：squash merge

2 つ目の session の合流ブランチを `git merge --squash` で取り込むと、中身は中央に入っても session は開いたままで、ミラーは中央を追わない
（ログは `holding break-glass session …: waiting for the central main to contain …`）。`git merge -s ours --no-ff` で履歴だけ入れ直すと閉じて追従に戻る。設計どおり。

### 7. 判定の変種

- 応答しない中央：中央を `git daemon` で出してミラーを作り、`git daemon` を `SIGSTOP`（TCP はつながるが返事が無い）。break-glass の判定は 1 回 10s で切って 3 回とも届かないに数える（判定に 50 秒＝10s×3 + 間隔 10s×2）。
  手元に変更が無いので判定の後の `nothing to commit` で止まる（判定だけを確かめる）。

  ```
  sakutei breakglass: probe 1/3: no answer from git://127.0.0.1:19419/central.git within 10s
  sakutei breakglass: probe 2/3: no answer from git://127.0.0.1:19419/central.git within 10s
  sakutei breakglass: probe 3/3: no answer from git://127.0.0.1:19419/central.git within 10s
  sakutei breakglass: breakglass: nothing to commit
  ```

  同じ中央に `sakutei mirror --once --timeout 5s` は 5 秒で `git remote: context deadline exceeded` で戻る。**直す前は止まったまま戻らなかった**（見つけた不具合 1）。
- 権限で弾かれる中央（`chmod 000 tokyo/central.git`。認証の失敗の代わり）：3 回とも `unreachable` に数える。

## 見つけたこと

### 直したもの

1. **応答しない中央で `sakutei mirror` が止まったままになる。** 1 回の同期に上限が無く、しかも `git remote update` は子の `git fetch` を起こすので、
   context で親を止めても子が標準出力を握ったまま待ち続け、呼び手も戻れなかった（break-glass の判定の `ls-remote` は子を起こさないので 10s で切れていた）。
   中央が戻っても止まった接続が切れるまで追従も事後合流も始まらない。git をプロセスグループで動かして取消でグループごと止め（`WaitDelay` も付けた）、
   `Mirror.Timeout`（`sakutei mirror --timeout`、既定 5 分）を足した。止めた git のエラーは理由（`context deadline exceeded`）を返す。
   テスト `internal/gitdist` の `TestBreakGlass_SilentCentral`。
2. **break-glass の判定の間、何も出ない。** 既定の判定は 20 秒（応答しない中央では 50 秒）かかるのに、終わるまで何も出なかった。
   1 回ごとの結果（届いた／届かない理由／timeout）を標準エラーに出すようにした（`Probe.Log`）。同じテストで確かめる。
3. **発動中の DR plan の矛盾に、片側のシャードしか気づかない。** 矛盾の検査が「自分が from／to のときの自分の側」だけだったので、
   `to` が平時 idle でない plan を発動すると、`to` は周を飛ばすが `from`（東京）は plan に従って idle になり、誰も primary を収束させなかった
   （コードを読むと、2 つの plan が別の `to` で同じ `from` を動かした場合も、2 つの `to` がどちらも primary を claim していた）。発動中の plan はすべて、両側のシャードの平時の claim と
   突き合わせ、矛盾があれば全シャードが周を飛ばすようにした（§25.1「いずれかがあれば、その周を飛ばす」のとおり。`enabled` の書き忘れはもともと全シャードが飛ばしていた）。
   テスト `TestResolveClaim_DRPlan` に足した。
4. **`sakutei mirror --dir osaka/mirror.git`（親がカレントでない相対パス）でミラーを作れない。** 作りかけの置き場を、clone は親のディレクトリ基準、
   config は呼び手のカレント基準で解釈して食い違っていた（`git config: fatal: not in a git directory`）。絶対パスにして作るようにした。テスト `TestMirror_RelativeDir`。

### 直していないもの（設計の判断が要る）

1. **中央が戻ってから合流するまでの間、中央を直接読むシャード（や session を開いていない別拠点のミラー）は、緊急書込の前の intent で収束する。**
   手順 5b で、合流前に起こし直した東京は中央（`enabled: false`）に従って site=tokyo を fencing で取り返し、緊急書込（`tk-web` 3）を中央の値で巻き戻した。
   「ミラーは緊急書込を古い中央で巻き戻さない」は session を開いたミラーから読むシャードにしか効かない。運用で守る（合流まで被災拠点のデーモンを起こさない）か、
   中央側で守る（合流用ブランチが中央にある間は、そのブランチが触った scope の claim を動かさない・合流用ブランチの存在を Event にする等）かは設計で決める。
2. **一度 FENCED になったシャードは、新しい holder が claim を手放しても取り戻さない。** 手順 5c で、東京が合流後の中央で `claim released` になっても
   DR シャードは `FENCED … held by "tokyo" at generation 5` のまま書かず、site=tokyo を誰も収束させなかった（起こし直すと取り戻す。§17.6 の 4 と同じ）。
   fencing のストアには「手放した」記録が無く、FENCED のシャードは claim が変わらない限り取り直さない。1 と組み合わさると DR 中に scope が宙に浮く。
   claim の解放を fencing に書く（または FENCED のシャードが holder の解放を見て取り直す）かどうかは §15.2・§17.6 の判断。
3. （小さいもの）session を開いている間の `sakutei mirror` は、中央を追っていなくても毎回 `synced … from …` と出す（直前の `holding …` 行で分かる）。
   PR の下書きと session の印（`sakutei-breakglass.json`）は bare ミラーの中に置かれる。§17.4 は「ミラーは状態を持たず消して作り直せる」とするが、
   session の間にミラーを消すと break-glass コミット・session・下書きが失われる。
4. （小さいもの）失効した鍵の確認は、書込のときに渡した `--audit-log` の S4 を読む。別の S4 のパスを渡すと失効が見えない
   （S4 は拠点の外の 1 つ、という運用の前提に頼っている）。
