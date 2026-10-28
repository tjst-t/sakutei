# CHANGELOG v3.51 → v3.52

**改訂日: 2026-10-28**

長く効く起動の引数を intent に置く改訂である（Backlog #183。親のストーリー「M7：本番に向けた固め」の C）。正しい状態は
宣言に置き、ツールの中だけに持たない（§1.7）の仕上げの 1 つで、設計の向きは変えていない。

## #183：長く効く起動の引数を intent の `runtime:` に書く（§26.7）

- **背景**: 予算（`--max-changes-per-cycle`・`--max-destructive-*`）・既定の mode・settle・効かない Apply の上限・観測し直しの上限・
  stuck の周数・周期は、常駐を起こすたびに同じ値を旗で渡し続けるもので、「どう回るべきか」の正が systemd のユニットや手で打つ
  コマンドの中にだけあった。git でレビューされず、起こし直しで黙って変わりうる。
- **決定**:
  - intent のトップレベルの `runtime:` に `interval`・`mode`・`max_changes_per_cycle`・`max_destructive_per_cycle`・
    `max_destructive_per_window`・`window_duration`・`on_exceed`・`settle`・`max_ineffective_applies`・`max_ineffective_duration`・
    `reobserve_max`・`reobserve_time`・`stuck_after` を書ける。1 つのファイルに 1 回だけ。inventory のファイルには書けない。
  - 旗だけ・宣言だけならそれを使う。食い違ったとき：
    - 大小のある引数（予算・上限）と `mode`・`on_exceed`：旗が締める向きなら旗を使い、一時的な締めとして level の Event
      `RuntimeTightened` と status（`runtime_tightened`）に旗の値と宣言の値を両方出す。緩める向きなら起動を断る。上限の 0 は上限なし。
    - 向きの無い引数（`interval`・`settle`・`window_duration`・`reobserve_*`・`stuck_after`）：食い違えば起動を断る。
    - 断るときは引数ごとに 1 行で、引数の名前・旗の値と宣言の値・直し方（旗を外すか、宣言を変えるか。宣言のファイルの名前つき）。
  - `RuntimeTightened` は運用者が選んだ締めなので、続いても stuck に上げない。
  - 常駐は起動したときの値で回り続け、途中で `runtime:` が変わったら `RuntimeDeclarationChanged`（level）と status の
    `runtime_changed` で「起こし直すと取り込む」と知らせる。宣言が戻れば resolved。
  - `--intent-repo` の常駐は起動のときに 1 回取得して読み、その取得は最初の周がそのまま使う（break-glass の知らせを落とさない）。
- **見送り**:
  - 常駐の途中で `runtime:` の変更をその場で取り込むこと（予算の窓・level の状態など起動のときに作るものがあるため。いまは知らせて
    起こし直しを待つ）。
  - `--freeze`・`--approve-large-change`（その場の操作）と、`--unreachable-skip-limit` などほかの引数を `runtime:` に置くこと。

## 影響

- §26.7 を新設した。spec/README.md の §26 の要約に足した。
- Event の kind `RuntimeTightened`・`RuntimeDeclarationChanged`（level）を足した。
- `--daemon-status`（と `sakutei status --json` の `daemon`・読み取りの口の /status）に `runtime_tightened`・`runtime_changed` を足した
  （足しただけなので版は上げない）。
- 一回性の `reconcile` と `plan` は、旗を読む前に intent を読むようになった（断るときは何も観測しない）。
