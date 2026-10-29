# CHANGELOG v3.62 → v3.63

**改訂日: 2026-10-29**

intent の `runtime:`（§26.7、v3.52）に実行の種類（常駐・一回性）ごとの値を書けるようにする改訂である（Backlog #193。親のストーリー
「M7：本番に向けた固め」）。旗と宣言の食い違いの扱い（締める旗は使って知らせ、緩める旗と向きの無い食い違いは起動を断る）は
変えていない。

## #193：`runtime.daemon:`・`runtime.oneshot:`（§26.7）

- **背景**: `runtime:` には mode を 1 つしか書けない。常駐は alert-only・一回性の reconcile は auto のように使い分ける運用では、
  宣言にどちらか片方しか書けない。「宣言は auto にして常駐だけ旗で締める」は今の決まりで通るが、常駐の status と Event に
  `RuntimeTightened` がいつも出てしまい、mode を宣言に書かずに旗のままにすることになる（§1.7 に反する）。
- **決定**:
  - `runtime:` の下に `daemon:`（常駐）と `oneshot:`（一回性の `reconcile` と `plan`）を足す。中身は `runtime:` と同じ欄。
  - その種類に効く値は、キーごとに、種類の欄にあればそれ、無ければ共通の `runtime:` の値。
  - 種類に意味の無い欄（`oneshot:` の `interval`・`stuck_after`）・知らないキー・入れ子の種類は読み込みで断る。
  - 旗と比べる相手は、その種類に効く宣言の値。締める向きの旗は旗を使って `RuntimeTightened` で知らせ、緩める向きの旗と
    向きの無い引数の食い違いは起動を断る（§26.7 のまま）。知らせと断りの 1 行は、種類の欄の値ならその欄
    （`runtime.daemon.mode`）を指す。status の `runtime_tightened` に `intent_key` を足した。
  - 常駐の `RuntimeDeclarationChanged` は、常駐に効く値どうしで比べる（`oneshot:` だけを変えても出ない）。
- **互換**: 種類の欄を書かなければ今までどおり。
- **見送り**:
  - 「一回性なら明示した旗が宣言と食い違っても受ける」扱い（宣言より弱い側に寄せることになるため採らない。一回性で別の値を
    使いたいなら `oneshot:` に書く）。
  - `plan` を `oneshot:` と別の種類にすること（plan は一回性の reconcile と同じ予算で Diff を見せるので、同じ値を使う）。
