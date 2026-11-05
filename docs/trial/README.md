# 試用の手順

Sakutei（`v3/`・`v3/providers/`）を、ファイル系の実 provider の dnszone（DNS のゾーンファイル）と nftables（ファイアウォールの ruleset ファイル）を相手に動かして確かめる手順と、v2 の intent を書き直す手順。

v2 の各機能の試用の手順（v1 の dnszone・nftables、sync-import・allocation・sharding・planned-change・workflow-gates・remediation-profile・dr-mode・observe・sqlmigrate・redfish-os-install）は v2 の実装と一緒に消した。タグ `v2.12.0` の `docs/trial/` で読める。その intent と最初の現実は、`sakutei-migrate` のテストの入力として `v3/providers/migrate/testdata/trial/` に移してある。

## v3 で試す（新しいコア、docs/trial/v3/）

新しいコア（`v3/`、設計 `docs/core/README.md`）に実の provider（`v3/providers/` の dnszone・nftables）を組み込んだ `sakutei` で、v2 の試用と同じ手順（plan → reconcile → 手動で変更する → 元に戻る → 通知だけにする → 宣言から外しても削除しない）を流す。

```sh
docs/trial/v3/run.sh            # 作業ディレクトリは /tmp/sakutei-trial-v3（毎回作り直す）
```

v2（v1）との違い：

- 接続設定（`config.yaml`）と intent に分かれる。ゾーン（`zone`）と扱う table（`host`・`tables`）は intent の `ranges` に書く。
- ID は dnszone が `<FQDN>/<型>`（type `dns/record`）、nftables が `<host>/<family>/<table>/<chain>`（type `fw/chain`）。`rules` はルール文のリスト。
- import は別のツール `sakutei-import`（Stage 9、下の「v2 の intent を新しいコアへ書き直す」）。status の代わりに plan がずれを出す。
- alert-only は `sakutei freeze`（理由・期限・誰が。`unfreeze` で終える）。
- 宣言から外したものは `ranges` の `prune: false`（既定）なら削除せず、unmanaged として出す。`prune: true` にしても、前の cycle の件数が無いとき・budget（`budget.destructive_per_cycle`）を超えるときは削除しない。

同じ手順と v1 の試用の D3（unmanaged の削除が budget で halt する）を `v3/providers/cmd/sakutei` の `TestTrial` が回す（`make test` に入る）。

## v2 の intent を新しいコアへ書き直す（sakutei-migrate）

v2 の intent と v2 に渡していた provider の設定（同じ名前の引数）を `sakutei-migrate` に渡すと、新しいコアの intent と接続設定に書き直す。
v1 の試用の intent（`sakutei-migrate` のテストの入力として `v3/providers/migrate/testdata/trial/v1/` に置いてある）なら：

```sh
cd v3/providers && go build -o /tmp/sakutei-migrate ./cmd/sakutei-migrate && go build -o /tmp/sakutei-v3 ./cmd/sakutei && cd ../..
T=v3/providers/migrate/testdata/trial/v1
W=/tmp/sakutei-migrate-trial; rm -rf $W; mkdir -p $W; cp $T/initial/* $W/
/tmp/sakutei-migrate --intent $T/intent --zone $W/example.com.zone --ruleset $W/firewall.rules --out $W/v3
/tmp/sakutei-v3 plan --intent $W/v3/intent --config $W/v3/config.yaml    # v2 の plan と同じ 3 件（作る 1・変える 2）
```

- `planned_change:` は別枠の Policy（`allow`・`windows`・`approval`・`applicant`）に、`remediation:` の時間帯は窓の Policy に、`profile:` は承認の数（team 1 人・enterprise 2 人と申請者≠承認者・solo は cooling-off）になる。v2 は承認者を intent に書いたが、新しいコアでは書き直したあと承認者が `sakutei approve <依頼>` で Record に残す（依頼は reconcile が書き、plan と `sakutei approvals` に出る）。
- 書き直せないもの（Workflow の `disruption`、window の中だけ通知する `remediation` の window など。どれも試用の intent には無い。一覧は `v3/README.md` の「Stage 9 の決まり」）があれば、全部を挙げて何も書き出さない（終了コード 2）。
- Workflow の gate と planned_change の承認者は名前で渡す（`--role platform-admin=alice,bob`・`--approver bob`）。秘密の参照は `--secret-file secretref:file/bmc/node1=/path/to/password`。
- 今の現実を宣言の形に書き出すのは `sakutei-import --intent <intent> --config <config> --out <file>`（`--every 1m` で同期、`--commit` で 1 回 1 commit）。
- v2 の試用の intent を全部（planned-change の planned_change の付いた intent と remediation-profile の時間帯・プロファイルの intent を含む 32 件。v2 の試用の run.sh が今の時刻から書いていた window は、表では時刻を差し込む形で書く）書き直して、新しいコアの plan が v2 の plan と同じ操作を出すことを、`v3/providers/migrate` のテストで確かめる（表は `v3/providers/migrate/testdata/trial-cases.json`、intent と最初の現実は `v3/providers/migrate/testdata/trial/`。表に入れていない v2 の run.sh の step（daemon で window が開いて閉じる・`--mode`・`--freeze`・宣言のエラー・急減の判定）と、確かめた範囲は `v3/README.md` の「Stage 9 の決まり」）。

## 試した結果の残し方

気づいたことは Backlog に積む。

- 動かない・危ない：kind `bug`。再現の手順を書く。
- 使いにくい・足りない：kind `task`、ラベル `trial-feedback`。
- 設計を変えないと直らない：本文にそう書き、人に聞く。
