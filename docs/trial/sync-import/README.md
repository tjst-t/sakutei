# import と Sync Mode（現実を generated/ に書き git にコミット）を手で試す

`sakutei import`（§24.4）と `sakutei sync`（Sync Mode、§24.5・§27.3・§29.1）を、本物のファイル（dnszone のゾーンファイル・fake provider の状態ファイル）と
ローカルの git リポジトリ（bare の origin とそのクローン）で回した記録。2026-10-23 にこのコンテナで回した。外のサービスには繋がない。

## 置いてあるもの（docs/trial/sync-import/）

| ファイル | 中身 |
|---|---|
| `run.sh` | 手順 0〜16 を 1 回で通すスクリプト（`docs/trial/sync-import/run.sh [作業ディレクトリ]`、既定 `/tmp/sakutei-trial-sync`。毎回作り直す） |
| `initial/example.com.zone` | import に使うゾーン（www・api・TXT・dyn の 4 レコード + SOA/NS） |
| `initial/state.json` | Sync に使う fake provider の現実（`site=legacy` に db-1・app-1、`site=tokyo` に web-1） |
| `intent/scopes.yaml` | `site=legacy` を `mode: sync`（`target_path: generated/legacy/`）、`site=tokyo` の web-1 は従来どおり収束 |

**Sync は dnszone では試せなかった**（下の「見つけたこと」1）。dnszone・nftables・Redfish のリソースは scope のラベルを持たない（root scope）ため、
空でない `mode: sync` の scope に 1 件も入らない。そこで Sync の挙動は、リソースごとに scope を持てる fake provider の状態ファイル（JSON を手で書き換える）で確かめた。
import は dnszone の本物のゾーンファイルで回した。

## 確かめる観点（設計書と受け入れテストから）

| # | 観点 | 根拠 | 手順 | 結果 |
|---|---|---|---|---|
| 1 | import の直後の plan は差分ゼロ。import も plan も現実を書き換えない | §24.4 | 1 | ○ |
| 2 | 既存の出力は `--force` なしでは上書きしない | §24.4 | 2 | ○ |
| 3 | 観測のあいだに値が変わるフィールドに managed 候補の印（直前のコメント） | §24.4 | 3 | ○ |
| 4 | secret の平文を書き出さない（SecretRef の形のときだけ書く） | §24.4・§20 | — | CLI で選べる provider に secret フィールドが無く手では試せない。import は既存の `[#22-3]`、Sync は今回足した `[#97-1]` で確かめる |
| 5 | sync の scope の現実だけが `<target_path>/sync-<provider>.yaml` に書かれ、自動生成ヘッダと本文の sha256 が付く | §24.5 | 6 | ○ |
| 6 | コミッター（author も）は `sakutei-sync <sakutei-sync@localhost>`、署名なし（人の設定が `commit.gpgsign=true` でも）、trailer `Sakutei-Sync:` | §24.5・§29.1 | 6 | ○ |
| 7 | 現実が変わらなければ書かず、コミットしない | §24.5 | 7・8 | ○ |
| 8 | 常駐（`--interval`）で、手で変えた現実がそのたびにコミットされる。scope の外の変化は記録しない | §24.5 | 8 | ○ |
| 9 | Apply しない（現実に書かない） | §24.5 | 9 | ○ |
| 10 | 揺れるだけのフィールドではコミットしない（印を残す 1 回だけ）。ほかの変化のときに落ち着いた値も記録される | §24.5 | 10 | ○ |
| 11 | `--committer-name` / `--committer-email` | §29.1 | 11 | ○ |
| 12 | 手で編集した生成物は上書きしない。戻せば再開 | §24.5 | 12 | ○ |
| 13 | 人のコミットで変わった生成物は（ハッシュを合わせ直していても）上書きもコミットもしない。revert すれば再開 | §24.5「直接コミットの拒否」 | 13 | ○（拒否は「Sync が止まる」形。下の「見つけたこと」3） |
| 14 | 人がステージしているものは Sync のコミットに入らない | §24.5 | 14 | ○ |
| 15 | List が不完全なら書かずに前のスナップショットを残す（0 以外で終わる） | §24.5 | 15 | ○（エラーの表示を直した。「見つけたこと」2） |
| 16 | dnszone を sync の scope で記録できる | §24.5 | 16 | ✗（「見つけたこと」1） |

## 回した結果（抜粋）

### import（手順 1〜4、dnszone）

```console
$ sakutei import --provider dnszone --zone zone --out intent --samples 3 --sample-interval 1s
import: dnszone: 4 resource(s) → intent/imported-dnszone.yaml
import: done (4 resource(s), 0 managed candidate(s), 0 skipped). This is a snapshot, not an ideal: ...
$ sakutei plan --intent intent --zone zone
Converged (no changes)
→ import も plan もゾーンファイルを書き換えていない
$ sakutei import --provider dnszone --zone zone --out intent --samples 1
sakutei import: intent/imported-dnszone.yaml already exists (it may contain your edits); use --force to overwrite
```

書き出された intent（抜粋）。全部 `ExplicitAbsentOnly`、TXT の引用符もそのまま往復する。

```yaml
  - provider: "dnszone"
    id: "txt.example.com./TXT"
    deletion_policy: "ExplicitAbsentOnly"
    fields:
      name: "txt.example.com."
      ttl: "300"
      type: "TXT"
      value: "\"v=spf1 -all\""
```

dyn の値を 0.4 秒おきに書き換えながら import すると（手順 3）、印が付く。

```console
import: managed candidate: dnszone//dyn.example.com./A field "value" changed across 3 observations: 10.0.0.1 -> 10.0.0.3 -> 10.0.0.5
      type: "A"
      # managed 候補: 観測のたびに値が変化（"10.0.0.1" → "10.0.0.3" → "10.0.0.5"）
      value: "10.0.0.5"
```

（揺れ続けているあいだに plan を回すと、当然 dyn は `cause: B` の UPDATE に出る。印を見て Schema で managed にするか、その行を消すのは人の仕事。）

### Sync Mode（手順 5〜15、fake provider の状態ファイル）

1 回目（手順 6）。人のリポジトリは `commit.gpgsign=true` にしてあるが、Sync のコミットは署名なし（`%G?` が `N`）。

```console
$ sakutei sync --intent intent --provider fake --state state.json --samples 1
sync: scope site=legacy provider fake: 2 resource(s) → generated/legacy/sync-fake.yaml (updated)
sync: committed 46fb4639d777... as sakutei-sync <sakutei-sync@localhost>
$ git log --format='%h %an <%ae> | %cn <%ce> | sig=%G? | %s'
46fb463 sakutei-sync <sakutei-sync@localhost> | sakutei-sync <sakutei-sync@localhost> | sig=N | sakutei sync: record reality (1 snapshot(s))
dd74cc2 alice <alice@example.com> | alice <alice@example.com> | sig=N | intent: legacy を mode: sync にする
```

コミットの本文と trailer、生成物の頭：

```text
sakutei sync: record reality (1 snapshot(s))

Sync Mode（§24.5）が現実から自動で記録したコミットです。generated/ を手で編集・コミットしないでください。

Sakutei-Sync: site=legacy fake
```

```yaml
# sakutei:generated sync
# Sync Mode（§24.5）が現実から自動で書き出したスナップショットです。手で編集しないでください
# （次の反映で上書きされるため、編集を検知したら上書きを止めます）。
# converge に切り替えるときは、人がレビューして intent/ へ移します（§24.5 cutover）。
# managed 候補（観測のたびに値が変わったフィールド）の値の変化だけでは書き直しません。
# scope: site=legacy
# provider: fake
# body-sha256: e4b7f7832901d94135ccf2be5ceb060e364b6b971ec975cb0e59564a2885aa03
# significant-sha256: e18bf5762b3fad6ce67edbeb7f1632e9fdd16760c3eca9a1af4809e9015424e4
# flapping-fields: []
resources:
  - provider: "fake"
    id: "app-1"
    scope:
      site: "legacy"
    deletion_policy: "ExplicitAbsentOnly"
    fields:
      name: "app-1"
      size: "2"
  - provider: "fake"
    id: "db-1"
    ...
```

`site=tokyo` の web-1 は書かれない。2 回目（手順 7）は `unchanged` / `nothing to commit` でコミット数は 2 のまま。

常駐（手順 8、`--interval 1s`）：db-1 の size を 4 → 8、legacy に cache-1 を足す、tokyo の web-1 を変える、を約 2.5 秒おきに手でした。
コミットは前の 2 つだけ（web-1 の変化は記録されない）。ほかの 7 周は `nothing to commit`。

```console
sync: scope site=legacy provider fake: 2 resource(s) → generated/legacy/sync-fake.yaml (updated)
sync: committed eb2008d62af8... as sakutei-sync <sakutei-sync@localhost>
sync: scope site=legacy provider fake: 3 resource(s) → generated/legacy/sync-fake.yaml (updated)
sync: committed 0159f290d3c1... as sakutei-sync <sakutei-sync@localhost>
（unchanged / nothing to commit の行: 7 周）
$ git log -p -2 -- generated/   （差分の行だけ）
--- 0159f29 sakutei sync: record reality (1 snapshot(s))
+  - provider: "fake"
+    id: "cache-1"
+    ...
--- eb2008d sakutei sync: record reality (1 snapshot(s))
-      size: "4"
+      size: "8"
```

手順 9：sync の前後で状態ファイルはバイト単位で同じ（Apply しない）。

手順 10（揺れるだけのフィールド）：lb-1 を足してコミット → lb-1 の size を 0.3 秒おきに変えながら `--samples 3` で回すと、印を残すコミットが 1 回。
そのあと揺らしても、落ち着いても `nothing to commit`。db-1 を変えたときにコミットされ、印は `以前の観測で値が変化` に変わって残る。

```console
      # managed 候補: 観測のたびに値が変化（"1" → "3" → "6"）
      size: "6"
# flapping-fields: [{"scope":"site=legacy","id":"lb-1","field":"size"}]
sync: nothing to commit
sync: nothing to commit
コミット数: 5 → 6（印を残す 1 回だけ）
```

手順 11〜15：

```console
$ sakutei sync ... --committer-name sync-bot --committer-email sync-bot@dc.example
sync: committed 2c51be2f27ba... as sync-bot <sync-bot@dc.example>
2c51be2 sync-bot <sync-bot@dc.example> | sync-bot <sync-bot@dc.example> | sig=N | sakutei sync: record reality (1 snapshot(s))

# 12. 生成物を手で編集
sync: scope site=legacy provider fake: skipped, previous snapshot kept: syncmode: snapshot was edited by hand (generated/ is auto-generated only): generated/legacy/sync-fake.yaml; restore it or delete it so Sync can rewrite it
→ 終了コード 1          （git checkout で戻すと nothing to commit に戻る）

# 13. 本文を変えてハッシュも合わせ直し、alice がコミット
sync: scope site=legacy provider fake: skipped, previous snapshot kept: syncmode: snapshot was changed by hand in a commit (generated/ refuses direct commits): generated/legacy/sync-fake.yaml (last changed in 06d709a by alice <alice@example.com>); revert it to Sync's last commit, or delete it in a commit so Sync can rewrite it
→ 終了コード 1          （git revert すると nothing to commit に戻る）

# 14. alice が NOTES.md をステージしたまま
$ git show --stat HEAD
sakutei-sync: sakutei sync: record reality (1 snapshot(s))
 generated/legacy/sync-fake.yaml | 6 +++---
$ git status --short
A  NOTES.md

# 15. List が不完全（list_partial）で、db-1 を 33 → 1 に
sync: scope site=legacy provider fake: skipped, previous snapshot kept: provider "fake": List is not complete (status partial); refusing to import an incomplete view of reality (§24.1)
→ 終了コード 1
      size: "33"          （記録は前のまま）
```

最後のリポジトリの履歴（手順 15 のあと）：

```text
44c611d sakutei-sync | sakutei sync: record reality (1 snapshot(s))   ← 14
31b774b alice        | Revert "手で db-1 を 64 に"                     ← 13
06d709a alice        | 手で db-1 を 64 に                              ← 13
2c51be2 sync-bot     | sakutei sync: record reality (1 snapshot(s))   ← 11
c0bf847 sakutei-sync | ...                                            ← 10（db-1 の変化）
...     sakutei-sync | ...                                            ← 10（lb-1 を足す・印を残す）、8（2 つ）、6
dd74cc2 alice        | intent: legacy を mode: sync にする
```

### dnszone を sync の scope で（手順 16）

```console
$ cat intent/scopes.yaml
scopes:
  - scope:
      zone: example.com
    mode: sync
    sync:
      target_path: generated/dns/
$ sakutei sync --intent intent --provider dnszone --zone zone --samples 1
sync: scope zone=example.com provider dnszone: 0 resource(s) → generated/dns/sync-dnszone.yaml (updated)
sync: committed 7fe6efa48dc3... as sakutei-sync <sakutei-sync@localhost>
$ tail -1 generated/dns/sync-dnszone.yaml
# (no resources in scope)
```

ゾーンには 4 レコードあるのに、空のスナップショットがコミットされる。

## 見つけたこと

1. **（設計の判断が要る・直していない）root scope の provider は Sync Mode で使えない。** dnszone・nftables・Redfish は List で全リソースを root scope（ラベル無し）で返す
   （`internal/dnszone/dnszone.go` の List に「provisional — v1a uses root scope」）。sync の scope は空を拒否する（§24.5 のロード時の検証 (1)）ので、
   どんな sync の scope にもこれらのリソースは入らず、`0 resource(s)` の空のスナップショットが何も警告なく書かれ、コミットされる（手順 16）。
   `import --scope` も同じで、ラベルで絞ると何も残らない（手順 4）。つまり v1 で Sync Mode を使えるのは、scope を持てる fake provider だけである。
   直すには「ファイルの provider のリソースにどう scope のラベルを付けるか」（例：provider の設定でラベルを付ける・ゾーンの origin を `zone` ラベルにする）を
   決める必要があり、ID のキー（§5.7「キーの照合」）・intent の書き方・prune の scope にも響くので、ここでは直さない。
   当面の手当てとして「List は N 件返したが scope に入るものが 0 件」を警告するかも、合わせて決めるとよい。
2. **（直した）List が不完全なときのエラーが `(status 1)` と数字で出ていた。** `core.ListStatus` に `String()` を足し、`(status partial)` と名前で出すようにした
   （#85 で `Availability` にしたのと同じ）。prune の halt 理由（`List status is not Complete (...)`）と `status` の「unmanaged 検出不可」の表示も名前になる。
   テスト `[#97-2]`（`tests/acceptance/syncmode_secret_test.go`）。
3. **（記録だけ）「直接コミットの拒否」は、git が拒否するのではなく Sync が止まる形である。** 手順 13 で alice の生成物へのコミットそのものは通り、
   次の Sync がその組を止めて 0 以外で終わる（§24.5「v1b の実装」の決まりどおり）。§9.6・§21 の「直接コミットは弾く」を git の側（pre-receive フックや CI）で
   強制するかは v1 の範囲の外で、必要なら別に決める。
4. **（記録だけ）secret は手では試せない。** CLI で選べる provider（fake・dnszone・nftables・Redfish・sqlmigrate）に `Secret: true` のフィールドが無い。
   import は既存の `[#22-3]` が、Sync は今回足した `[#97-1]`（`tests/acceptance/syncmode_secret_test.go`：平文の secret をスナップショットにもどのコミットにも書かない・
   secret の値だけが変わってもコミットしない・SecretRef は参照の形で記録する）が確かめる。
5. **（小さい・直していない）** Sync の List 不完全のメッセージが importer のものを流用していて `refusing to import ...` と出る（sync でも import と読める）。
   一つの組が止まった周でも最後に `sync: nothing to commit` が出るので、止まった行を見落としやすい（終了コードは 1 になる）。
   **（#111 で直した）** Sync では `not recording an incomplete view of reality (§24.5)` と出し、止まった組がある周は最後の行に
   `sync: N of M scope/provider pair(s) stopped this pass (previous snapshot kept; see above)` を出す。`[AC-111-4]`。
