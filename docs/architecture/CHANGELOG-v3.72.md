# CHANGELOG v3.71 → v3.72

**改訂日: 2026-10-29**

Workflow を進めない常駐を静かにし、Workflow の run と手を S4 監査の鎖に残す改訂である（Backlog #210）。Workflow の進め方（直列・
不変条件・承認ゲート・S2 の進行位置、§6.3・§6.4）、まだ済んでいないことの決め方（#209）と監査の鎖の形（#187）は変えていない。

## #210：進めない常駐を静かにする（§6.3）

- **背景**: Workflow を宣言すると、store を渡さない常駐（alert-only など、Workflow を進めない役）が周ごとに
  `WorkflowError: no workflow store configured (--workflow-store)` を出し続けた。Apply はしないので害は無いが、Event と status が汚れた。
  alert-only の常駐と、ほかの常駐が進めるロックを持つ常駐も、周ごとに `WorkflowWaiting` を出した。また alert-only の常駐は store を渡すと
  進めるロックを取ってしまい、進める役の常駐を締め出しえた。
- **決定**:
  - **常駐にも読むだけの store**：`--workflow-observe-only`（`--workflow-store` が要る）で、常駐は store を読むだけにする（書かない・
    進めない・進めるロックを取らない）。#209 の一回性の `reconcile` と同じく、done の Workflow の対象は宣言どおりに収束させる。
  - **進めないことは誤りでない**：store を渡さない・読むだけ・remediation が alert-only・ほかの常駐が進めるロックを持つ常駐は、Workflow を
    進めないことを `WorkflowError` にも毎周の `WorkflowWaiting` にもせず、「Workflow は別の処理が進める」として扱う。知らせは新しい level の
    Event `WorkflowAdvancedElsewhere`（info、resource なし、attrs の `workflow` に名前・`reason` に理由）で、続く間は最初の周に 1 回だけ出し、
    stuck に上げない（役割であって失敗ではない）。進めるようになった周か Workflow が無くなった周に resolved。常駐の status（`--daemon-status` の
    `workflows_advanced_elsewhere`、`sakutei status --daemon-status` と読み取りの口の `/status`）に 1 行（`注意: Workflow は別の処理が進める（§6.3）: …`）出す。
    進めるロックを取ろうとして誤りになったとき（ロックのファイルを開けないなど）は今どおり `WorkflowWaiting`。
  - **進めない常駐はロックを取らない**：読むだけの常駐と alert-only の常駐は進めるロックを取りに行かない。進める役の常駐が同じ store で
    ロックを取れる。
  - 対象の扱いは #209 の決め方（読むだけの store・`done_when`・開いている planned_change）のまま。進み具合が分からずに持つ対象の
    `WorkflowTargetHeld`（level）もそのまま出す。

## #210：Workflow を監査の鎖に残す（§6.3・§26.3）

- **背景**: Workflow で何かをしても、監査（`--audit-log`）に残るのは手から出た `resource.apply` だけで、cause も B（ずれ）になっていた。
  Workflow の名前・一度きりの操作（`boot_once`・`reset`）・手の順番・待ちの結果が残らず、「どの Workflow のどの手で何をしたか」を監査
  から追えなかった。
- **決定**: S4 の鎖（#187）に次の種別を足す。
  - `workflow.run.start`：Workflow の名前・run の id・起こした理由（宣言を初めて見た〈store に run が無い〉・定義が変わった〈前の done の
    run の id〉・`trigger` の宣言〈planned_change の名前〉と、定義のハッシュの頭）。
  - `workflow.step`：済んだ・失敗した手ごとに 1 行（待ちの周は書かない）。run の id・stage・target・手の番号（1 から）・手の種類（`apply`・
    `task`・`wait`・`require`・`verify`・`action`、ステージの後の承認は `gate`）・手が触るリソース・欄の名前（値は書かない）・結果
    （`ok`・`sent`・`satisfied`・`timed_out`・`failed`・`approved`・`rejected`）・理由。`action` は op と args を、`wait`・`require` は
    手に入ってからかかった時間（`elapsed`）も残す。ゲートの timeout は今の `workflow.gate.timed_out` に加えて手の `gate`（`timed_out`）。
  - `workflow.run.end`：`done`・`halted`・`cancelled` と理由。halt は止めた周に 1 回だけ（halt したまま回る周は書かない）。承認の却下は
    `workflow.gate.rejected` に続けて手の `gate`（`rejected`）と `workflow.run.end`（`halted`）。`sakutei workflow cancel --audit-log` は
    `workflow.cancelled` に続けて、終わっていない（実行中・承認待ちの）run なら `workflow.run.end`（`cancelled`）。store を失った常駐が
    `done_when` で done と書くときも始まりと終わりを 1 件ずつ残す。
  - Workflow の手から出た `resource.apply` に、Workflow・run の id・stage・target・手の番号を付ける。cause は A にする（手の欄は Workflow
    の宣言であって、現実のずれではない）。
  - どれも同じ鎖に書くので `sakutei audit verify` でたどれる。書けなかったときは `WorkflowError` で知らせるが、手は戻さない。

## docs

- OS の導入の例（`docs/trial/redfish-os-install/`）に、BMC の一般的な注意を書いた：ゲストの中からの再起動でまた CD から起動して導入が
  繰り返されることがあるので、導入の終わりは電源 Off にし Off を待ってから On にする形を勧める。導入中の GracefulRestart は無視される
  ことがあるので、時間切れの扱い（halt・ForceRestart・電源の操作への切り替え）を決めておく。

- **検証**: 受け入れ（`[AC-210-1]`〜`[AC-210-5]`、`tests/acceptance/workflow_audit_test.go`）。既存の Workflow の受け入れの期待
  （store の無い常駐の Event・alert-only・ロックを取れない常駐の Event）を `WorkflowAdvancedElsewhere` に改めた。
- **見送り**: 監査の Workflow の記録から run の進み具合を作り直すこと（store は今どおり S2 の写し。監査は追うためのもの）。
