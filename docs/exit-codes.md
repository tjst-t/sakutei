# 終了コードの表

`sakutei` の各コマンドの終了コードを 1 か所にまとめる（Backlog #166）。コマンドごとの説明はそれぞれのソースの
コメントと `-h` にもあるが、ここが一覧の正。値を足す・意味を変えるときはこの表も直す。

## 全体に共通の決まり

- **0** は「そのコマンドがすることを終えた」。収束に変更があったか（drift があったか）は 0 を変えない。
- **1** は「できなかった」（使い方の誤り・intent を読めない・provider を解決できない・エラー）と、`reconcile` では
  「収束していないことが確か（観測できない・skip）」。
- **2** は使わない値として避ける：Go のプログラムが panic したときの終了コードが 2 で、包む道具（make など）も失敗に
  2 を返すので紛れる。例外は二つ ——（1）`allocator` の使い方の誤り（v1.2 から）、（2）`flag.ExitOnError` で旗を
  読むコマンド（下の表で「旗の誤り 2」と書いたもの）の旗の誤りで、Go の `flag` パッケージが 2 で終える。
- **`-h` / `--help`** は 0（求められた表示で、誤りではない）。
- 知らないサブコマンドは 1。

## コマンドごとの値

| コマンド | 0 | 1 | 2 | 3 | 4 |
| --- | --- | --- | --- | --- | --- |
| `sakutei`（常駐） | SIGTERM / SIGINT で止まった（周の切れ目で止め、in-flight の Apply を待つ） | 起動できない（旗・intent の provider の検証・`--cause-state` を渡した、#165）・ループの致命的なエラー | — | — | — |
| `sakutei plan` | 計画を出した（差分の有無によらない。`--json` も同じ） | 計画を出せない（旗・intent・観測のエラー・計画の時点のエラー。互換の表に合わない版の組み合わせ・更新の順序、#151） | — | — | — |
| `sakutei reconcile` | 1 回の収束を終え、skip したものも効かない Apply も無い（変更の有無によらない。Apply の失敗だけなら 0 のまま、要約が「not converged」と言う、#110。alert-only も 0） | エラー（旗・intent・`--expect-revision` の不一致・予算 / 容量の halt があった・S4 に書けない・互換の表に合わない版の組み合わせ・更新の順序〈何もしない〉、#151）、または観測できない・skip したリソースがある（収束していない） | — | — | **#166**：Apply は成功を返したが現実が宣言どおりにならなかった（settle まで読み直しても Diff が残る）、または Apply を止めたリソース（oscillating）がある。1 の条件と同時なら 1 |
| `sakutei status` | 報告を出し、すべて観測できた（drift があっても 0） | 報告を出せない（旗・intent・provider の解決） | — | 報告は出したが観測できないもの（Read のエラー・availability ≠ ok・計画のエラー）か、List が失敗した provider がある（#117） | — |
| `sakutei graph` | DAG を出した | 出せない（旗・intent） | — | — | — |
| `sakutei import` | intent を書き出した | 書き出せない | — | — | — |
| `sakutei sync` | mode: sync の scope の現実を書いた | 書けない | — | — | — |
| `sakutei fence` | 世代を出した | 出せない | 旗の誤り | — | — |
| `sakutei task list / cancel / approve / reject` | した | できない（記録が無い・S4 に書けない等） | 旗の誤り | — | — |
| `sakutei workflow list / cancel`・`sakutei approve`・`sakutei reject` | した | できない | 旗の誤り | — | — |
| `sakutei allocator`（サービス） | 止めた | 動かせない（待ち受け・ストア） | 使い方の誤り（`--store` が無い・旗の誤り） | — | — |
| `sakutei allocator release / list / import` | した | できない（届かない・記録が無い。import はファイルと store が食い違えば書かずに断る、#157） | 使い方の誤り | — | — |
| `sakutei apply-check list / release`（#166） | した | できない（使い方・ファイルを読めない・解く記録が無い・S4 に書けない） | — | — | — |
| `sakutei mirror` | 止めた | 動かせない | 旗の誤り | — | — |
| `sakutei secrets extract` | 書き出した | できない | 旗の誤り | — | — |
| `sakutei breakglass keygen / commit / verify / status` | した（verify は署名が正しい） | できない（verify は署名が正しくない） | 旗の誤り | — | — |
| `sakutei version` / `help` | 表示した | — | — | — | — |

## `reconcile` の 4（#166）

`reconcile` は Apply が成功を返したリソースを、その場で読み直して Diff が消えたかを確かめる（§11.1 Tier2）。型ごとの
settle（provider の既定。redfish は 15 秒、proxmox-vm は 30 秒、ほかは 1 回読み直すだけ。intent の `apply_check:` と `--settle` で変える）の
間は待って読み直す。

- Diff が消えた → 効いた（今までどおり 0）。
- settle を過ぎても Diff が Apply したときと同じまま → 効かなかった。`ApplyIneffective` の Event を出し、要約は
  「Reconcile: not converged: N change(s) reported applied, but M resource(s) still differ …」と言う（「applied」と
  言い切らない）。終了コード 4。
- Diff が変わった（縮んだ）が残った → 進んだので回数には数えないが、収束していないので 4。
- 観測できない・相手が Converging と言う → 確かめられなかった（unknown ≠ ineffective）。要約に「not yet confirmed」と
  出すが、終了コードは変えない。

`--apply-check-state <file>` を渡すと、同じ Diff で効かなかった回数を回をまたいで数え、上限（`--max-ineffective-applies`、
既定 3、または `--max-ineffective-duration`）に達したリソースへは Apply しない（oscillating、§11.1 Tier3）。そのとき
も 4。解くのは Diff の中身（desired か現実の値）が変わったとき・現実が宣言どおりになったとき・
`sakutei apply-check release` のとき。ファイルを渡さなければ回数は 1 回の中だけで、止めることはない（毎回 4 で終わる）。

## スクリプトでの使い分けの例

```sh
sakutei reconcile --intent intent/ --redfish-config bmcs.yaml
case $? in
  0) echo converged ;;
  4) echo "applied but reality did not change — look at the ApplyIneffective Event" ;;
  *) echo "not converged or failed" ;;
esac
```
