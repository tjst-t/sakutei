# CHANGELOG v3.68 → v3.69

**改訂日: 2026-10-29**

Workflow の「まだ済んでいない」を store（S2）でなく宣言と観測で決め、一回性の `reconcile` でも done の Workflow の対象を宣言どおりに保つ
改訂である（Backlog #209）。Workflow の進め方（直列・不変条件・承認ゲート・S2 の進行位置、§6.3・§6.4）と claim の決まり（§17）は変えていない。

## #209：done になった対象を一回性の reconcile でも宣言どおりに保つ（§6.3）

- **背景**: 一回性の `reconcile` は Workflow の進み具合の store（`--workflow-store`）を持たず、Workflow の対象（台の電源・起動先・仮想
  メディアなど）は Workflow が done になったあとも「Workflow に任せる」に見えて触られなかった。終わりの姿を保てるのは常駐が動いている
  間だけだった。`trigger: reimage` の Workflow も、store が無いと planned_change が開いていないときも対象を持った（§6.3 は開いていなければ
  ふつうに収束させる決まり）。進み具合を store の中にだけ持つと、store を失ったときの振る舞いが決まらない（§1.7 に反する）。
- **決定**:
  - **一回性の `reconcile` に `--workflow-store`（読むだけ）**：進み具合を読むためだけに使い、store を書かない。一回性は Workflow を進め
    ない（進めるのは常駐と `sakutei workflow`・承認の口だけ）。done の Workflow（定義のハッシュが同じ）の対象はふつうのリソースとして宣言
    どおりに保つ。一回性で Apply する口は `reconcile` だけ（`apply` は主 verb にしない）。
  - **まだ済んでいないことは宣言と観測で決める**（`workflow.Judge`。常駐・一回性・plan・status が同じ判定を使う）：
    - store に run があればそれに従う（store の Run は写しと作業の記録）。
    - `trigger: reimage` は、run が無いか done なら、対象の scope を覆う `planned_change` の `kind: reimage` が開いていて `max_count` が
      残っているときだけ持つ。開いていなければ store が無くても収束させる。`--audit-log` の無い一回性・plan・status はレーンを評価
      しないので、窓の中の宣言を開いているものと数える（承認を見ない＝持つ側）。
    - store が無いか store にその run が無いときは、Workflow の `done_when: { resource, fields }`（`require` と同じ形）が全 target で
      観測で満たされれば済んだと見て持たない。無い・満たされない・観測できない（unknown ≠ 済んだ）ときは今どおり持ち、理由
      （`progress unknown (no --workflow-store, and done_when … is not observed on …)` など）を plan・status・reconcile に出す。
    - `done_when` は定義のハッシュに入れない（書き足しても done の run を起こし直さない）。`trigger: reimage` には書けない（入れ直しは
      今の姿が宣言どおりでもやり直す。ロード時に断る）。
  - **store を失った常駐**：store にその run が無く `done_when` が満たされていれば、新しい run を起こさず done の記録（RunID
    `<workflow>-observed-<時刻>`）を store に書き、`WorkflowCompleted` を出す。満たされなければ今どおり run を起こす。
  - **二重に走らないこと**：Workflow を進める常駐は store の排他のロック（`<store>.advance.lock` の flock。常駐の終わりか、落ちたときに
    放される）を取ってからだけ進める。取れない常駐は進めず（`WorkflowWaiting`）、done でない Workflow の対象を持つ。一回性・plan・
    status・承認の口・cancel はこのロックを取らない。同じリソースに常駐と一回性が両方 Apply しうる点は今の claim の決まりのまま。
  - **halt の扱い**：halt した Workflow の対象は触らない。理由は plan・status・reconcile の出力と、新しい level の Event
    `WorkflowTargetHeld`（対象ごと、warning、attrs の `workflow`・`stage`）に出す。進み具合が分からずに持つ対象も同じ Event で知らせる。
    halt は人が `sakutei workflow cancel` で片づけるまで保ち、持たなくなった周に resolved。`done_when` が満たされていても宣言どおりに
    保つ向きへ自動で切り替えない（halt は導入の途中の姿のことがあり、途中の台を勝手に動かしうる）。
- **検証**: 単体（`internal/workflow` の判定・ハッシュ・ロック、`internal/filelock` の TryLock、level の resolved）、受け入れ
  （`[AC-209-1]`〜`[AC-209-6]`、`tests/acceptance/workflow_done_converge_test.go`）。
- **見送り**: halt からの resume の verb（今は cancel だけ）。ロックを持つ常駐が生きたまま止まっている（進めない）ときの引き継ぎ（flock は
  プロセスが終われば放される）。
