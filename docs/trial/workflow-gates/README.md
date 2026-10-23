# Workflow の承認ゲートを手で試す（単一・N-of-M・roles・separation・timeout、CLI と承認 API、不変条件）

最小 Workflow（§6.3）の承認ゲート（§6.4・§14.4）を、常駐の `sakutei`（`--workflow-store`・`--audit-log`・`--approval-listen`）で回して確かめる。
これまで自動テスト（`tests/acceptance/workflow_test.go`・`workflow_nofm_test.go`）でしか通していなかったもの。2026-10-23 にこのリポジトリのコンテナで回した（Backlog #101）。

相手は fake provider（`--state` の JSON ファイル）。3 台（n1〜n3）を 1 台ずつ drain → version 2 に上げる → undrain → 不変条件の確かめ、と進める。
外のサービスには繋がない。承認 API は `127.0.0.1` で待ち受ける。

## 置いてあるもの（docs/trial/workflow-gates/）

| ファイル | 中身 |
|---|---|
| `intent/rolling.yaml` | 3 台のあるべき姿（version 2・active）と Workflow `rolling`。loader で読めることを `internal/loader` のテスト `TestWorkflowGatesTrialIntentLoads` が確かめる |
| `initial/state.json` | 最初の現実（3 台とも version 1・active）。fake の `provider_version` を入れておく（無いと provider の登録で止まる・§10.4） |
| `run.sh` | 下の A〜D を通して流すスクリプト（go・curl・python3 が要る。python3 は現実の JSON を手で書き換えるのに使う） |

Workflow `rolling` のゲート：

| ステージ | 対象 | ゲート |
|---|---|---|
| s1 | n1 | `manual_approval: required`（単一承認、timeout 24h） |
| s2 | n2 | `requirements`: `quorum {n: 2, of: 3}`・`roles [platform-admin]`・`separation: applicant_ne_approver`（申請者は `alice`）、timeout 24h |
| s3 | n3 | なし（終われば done） |

不変条件は `min_active: 2`（3 台のうち 2 台は `active: "true"`）。

## 通しで流す

```sh
docs/trial/workflow-gates/run.sh            # 作業ディレクトリは /tmp/sakutei-wfg（毎回作り直す）。PORT=18470 で承認 API のポートを変えられる
```

シナリオごとに作業ディレクトリ（`a`・`b`・`c`・`d`）を分け、それぞれに intent・現実（`state.json`）・S2（`wf.json`）・S4（`audit.jsonl`）を置く。
デーモンは 1 つずつ次のように起こす：

```sh
./sakutei --intent intent --state state.json --workflow-store wf.json --audit-log audit.jsonl \
  --approval-listen 127.0.0.1:18470 --interval 1s
```

## 確かめる観点（設計書と受け入れテストから）

| # | 観点 | 根拠 |
|---|---|---|
| 1 | ステージを終えると `StageAwaitingApproval` を出し、「待ち」で止まる。次のステージの対象に触らない | §6.4 フロー |
| 2 | デーモンを起こし直しても待ちに留まる（進行位置と承認の数え上げは S2 にある） | §6.4「S2 の永続化により留まる」 |
| 3 | `sakutei approve`（CLI）と `POST /workflows/{id}/gates/{gate}/approve` のどちらでも通る。待っていないゲートへの判断は拒否 | §6.4 API 形 |
| 4 | N-of-M：異なる承認者 N 人で通る。同じ人の二重承認は 1 と数える | §6.4・§14.4 |
| 5 | roles：要件の役割を名乗らない承認は数えない（403）。役割の真偽は見ない（上位 IdP） | §6.4「承認 API 自体は認可を持たない」・§7 |
| 6 | separation：申請者本人の承認を拒否（403） | §14.4 4-eyes |
| 7 | 却下（API・CLI）で halt。halt した Workflow の対象は収束ループに返らない | §6.3 on_failure: halt・keep_drained |
| 8 | ゲートの timeout で halt。期限を過ぎた承認は受け付けない | §6.4 timeout |
| 9 | 全承認者（ID・役割・署名）・却下・timeout・cancel が S4 監査に append される | §6.4「全承認者を S4 監査に記録」 |
| 10 | 不変条件（min_active）を破る手順には進まず、現実が戻れば進む（level-triggered） | §6.3 invariant |
| 11 | `sakutei workflow cancel` で run を消すと、次の周に最初のステージからやり直す | §6.3（v1.5 の実装） |
| 12 | done になれば対象は収束に戻る（手でずらすと直す） | §6.3 |

## 結果（2026-10-23、run.sh の出力の抜粋）

### A. 通しの流れ（観点 1〜6・9・12）

**A1. ゲート s1 で止まる。** cycle 1 で n1 の 4 手順を施し、ゲート s1 で待つ。n2・n3 は version 1 のまま。

```
sakutei: cycle 1: WorkflowStarted: workflow rolling: run rolling-1792769959518485333 started (3 stage(s), 3 target(s))
sakutei: cycle 1: WorkflowStepDone: workflow rolling: stage s1, target n1, step 1 (apply): applied map[active:false]
...
sakutei: cycle 1: StageAwaitingApproval: workflow rolling: stage s1 completed; awaiting approval at gate s1 (1 approval(s), timeout 24h0m0s, deadline 2026-10-24T15:39:19Z): POST /workflows/rolling/gates/s1/approve
sakutei: cycle 2: WorkflowWaiting: workflow rolling: awaiting approval at gate s1 until 2026-10-24T15:39:19Z (0 of 1 approval(s); POST /workflows/rolling/gates/s1/approve)
n1:active=true,version=2  n2:active=true,version=1  n3:active=true,version=1
```

**A2. 起こし直しても待つ。** `sakutei workflow list` と、起こし直したデーモンの cycle 1：

```
WORKFLOW                 PHASE              GATE         POS      REASON
rolling                  awaiting_approval  s1           0/1/0
sakutei: cycle 1: WorkflowWaiting: workflow rolling: awaiting approval at gate s1 until 2026-10-24T15:39:19Z (0 of 1 approval(s); ...)
```

**A3. CLI で承認。** 待っていない s2 への承認は拒否（exit 1）、s1 は bob 1 人で通り、次の周で n2 を進めて s2 で待つ。

```
sakutei approve: gate is not awaiting approval: workflow rolling gate s2 (workflow is awaiting_approval at gate s1)
approved gate s1 of workflow rolling (run rolling-1792769959518485333); the daemon proceeds on its next cycle
sakutei: cycle 3: GateApproved: workflow rolling: gate s1 approved by bob (1 of 1 required)
sakutei: cycle 3: StageAwaitingApproval: workflow rolling: stage s2 completed; awaiting approval at gate s2 (2 approval(s); roles platform-admin, separation applicant_ne_approver, timeout 24h0m0s, ...
```

**A4. 承認 API で要件に合わない承認を拒否する。**

```
POST s2/approve {"approver":"alice","roles":["platform-admin"],"signature":"sig-a"} -> 403 ... separation applicant_ne_approver: alice is the applicant and cannot approve
POST s2/approve {"approver":"dave","roles":["developer"],"signature":"sig-d"} -> 403 ... approver dave holds none of the required roles [platform-admin] (asserted roles [developer])
POST s2/approve {"approver":"erin","signature":"sig-e"} -> 403 ... approver erin holds none of the required roles [platform-admin] (asserted roles [])
POST s2/approve {"approver":"bob","roles":["platform-admin"]} -> 400 invalid decision: signature is required
```

**A5・A6. 二重承認は 1。起こし直しても 1 of 2。**

```
POST s2/approve {"approver":"bob",...,"signature":"sig-b2"} -> 200
POST s2/approve {"approver":"bob",...,"signature":"sig-b3"} -> 200
sakutei: cycle 5: WorkflowWaiting: workflow rolling: awaiting approval at gate s2 until ... (1 of 2 approval(s); roles platform-admin, separation applicant_ne_approver; ...
（起こし直し）
sakutei: cycle 1: WorkflowWaiting: workflow rolling: awaiting approval at gate s2 until ... (1 of 2 approval(s); ...
```

**A7. 2 人目の carol で通り、s3 を経て done。** 決着したゲートへの承認は 409。

```
sakutei: cycle 2: GateApproved: workflow rolling: gate s2 approved by bob, carol (2 of 2 required)
sakutei: cycle 2: WorkflowStepDone: workflow rolling: stage s3, target n3, step 1 (apply): applied map[active:false]
...
sakutei: cycle 2: WorkflowCompleted: workflow rolling: run rolling-1792769959518485333 completed; targets return to convergence
POST s2/approve {"approver":"frank",...} -> 409 gate is not awaiting approval: workflow rolling gate s2 (workflow is done)
n1:active=true,version=2  n2:active=true,version=2  n3:active=true,version=2
```

**A8. done のあとは収束に戻る。** n2 を手で version 1 に戻すと、次の周に直す。

```
sakutei: DriftEvent: fake//n2 [cause: B] drift B: version was "2" prev cycle now "1" (prev snapshot at 2026-10-23T15:39:26Z)
sakutei: cycle 4: RESOLVED Drift fake//n2: resolved after 1 cycle(s) (firing since 2026-10-23T15:39:27Z)
```

**A9. S4 監査ログ。** 承認 4 件（bob の 2 回を含む）が承認者・役割・署名つきで残る。拒否した承認（alice・dave・erin・署名なし）は残らない。

```
{"kind":"workflow.gate.approved",...,"gate":"s1","actor":"bob","signature":"sig-b1","detail":"stage s1 approved (1 of 1 required: bob); proceeding to the next stage"}
{"kind":"workflow.gate.approved",...,"gate":"s2","actor":"bob","roles":["platform-admin"],"signature":"sig-b2","detail":"stage s2 approved (1 of 2 required: bob); still awaiting approval"}
{"kind":"workflow.gate.approved",...,"gate":"s2","actor":"bob","roles":["platform-admin"],"signature":"sig-b3","detail":"stage s2 approved (1 of 2 required: bob); still awaiting approval"}
{"kind":"workflow.gate.approved",...,"gate":"s2","actor":"carol","roles":["auditor","platform-admin"],"signature":"sig-c","detail":"stage s2 approved (2 of 2 required: bob, carol); proceeding ...
```

### B. 却下と cancel（観点 7・9・11）

```
POST s1/reject {"approver":"mallory","signature":"sig-m"} -> 200
sakutei: cycle 5: WorkflowHalted: workflow rolling: run rolling-1792769968465008154 is halted: gate s1 rejected by mallory (targets held; `sakutei workflow cancel rolling` to restart)
n1 を手で version=9 にしたまま（halt した Workflow の対象は収束ループが直さない）:
n1:active=true,version=9  n2:active=true,version=1  n3:active=true,version=1
POST s1/approve {"approver":"bob","signature":"sig-b"} -> 409 gate is not awaiting approval: workflow rolling gate s1 (workflow is halted)
```

`sakutei workflow cancel --audit-log audit.jsonl rolling` で run を消すと、次の周に新しい run が s1 からやり直す（n1 の version=9 は step 2 の apply で 2 に戻る）。
そこで `sakutei reject`（CLI）でも halt する。

```
cancelled run rolling-1792769968465008154 of workflow rolling (was halted)
sakutei: cycle 6: WorkflowStepDone: workflow rolling: stage s1, target n1, step 2 (apply): applied map[version:2]
sakutei: cycle 6: StageAwaitingApproval: workflow rolling: stage s1 completed; awaiting approval at gate s1 ...
rejected gate s1 of workflow rolling (run rolling-1792769973741691840); the workflow is halted
rolling                  halted             s1           0/1/0    gate s1 rejected by mallory
{"kind":"workflow.gate.rejected",...,"run_id":"rolling-1792769968465008154","gate":"s1","actor":"mallory","signature":"sig-m","detail":"stage s1 rejected; workflow halted"}
{"kind":"workflow.cancelled",...,"run_id":"rolling-1792769968465008154","detail":"cancelled at phase halted"}
{"kind":"workflow.gate.rejected",...,"run_id":"rolling-1792769973741691840","gate":"s1","actor":"mallory","signature":"sig-m2",...}
```

### C. timeout（観点 8・9）

s1 の timeout を 6s に縮めた intent（`run.sh` が作業ディレクトリで書き換える）。

```
sakutei: cycle 1: StageAwaitingApproval: ... at gate s1 (1 approval(s), timeout 6s, deadline 2026-10-23T15:39:41Z): ...
sakutei: cycle 7: WorkflowHalted: workflow rolling: gate s1 timed out: no approval by 2026-10-23T15:39:41Z (opened 2026-10-23T15:39:35Z) (on_failure: halt; ...)
POST s1/approve {"approver":"bob","signature":"sig-b"} -> 409 gate is not awaiting approval: workflow rolling gate s1 (workflow is halted)
{"kind":"workflow.gate.timed_out",...,"gate":"s1","detail":"gate s1 timed out: no approval by 2026-10-23T15:39:41Z (opened 2026-10-23T15:39:35Z)"}
```

デーモンが止まっている間に期限が過ぎた場合：run はまだ `awaiting_approval` だが、承認は期限で拒否され、デーモンを起こした最初の周に halt する。

```
sakutei approve: gate is not awaiting approval: gate s1 of workflow rolling timed out at 2026-10-23T15:39:48Z
sakutei: cycle 1: WorkflowHalted: workflow rolling: gate s1 timed out: no approval by 2026-10-23T15:39:48Z (opened 2026-10-23T15:39:42Z) ...
```

### D. 不変条件 min_active（観点 10）

n2 を外から `active: "false"` にしておくと、n1 の drain（`active: "false"`）は施した後の見込みが 1 台で min_active 2 を破るので進まない。n1 は触られない。

```
sakutei: cycle 1: InvariantBlocked: workflow rolling: stage s1, target n1, step 1 (apply): invariant min_active 2 would break (1 active); not proceeding until it holds
n1:active=true,version=1  n2:active=false,version=1  n3:active=true,version=1
```

n2 を `active: "true"` に戻すと、次の周に進んでゲート s1 で待つ。

```
sakutei: cycle 4: WorkflowStepDone: workflow rolling: stage s1, target n1, step 4 (verify): invariant holds
sakutei: cycle 4: StageAwaitingApproval: workflow rolling: stage s1 completed; awaiting approval at gate s1 ...
n1:active=true,version=2  n2:active=true,version=1  n3:active=true,version=1
```

すべて設計どおりに動いた。

## 見つけたこと

### 直したもの

1. **Workflow の対象が毎周「skip … in-flight or unobservable」と出て、skipped に数えられていた。** ゲートで待っている間、デーモンは毎周
   `sakutei: skip fake//n1: in-flight or unobservable this cycle` を対象の数だけ出し、`skipped=3`・`3 resource(s) skipped (retry next cycle)` と言っていた。
   対象は in-flight でも観測不能でもなく、Workflow に任せているだけ（同じ周に `3 Workflow target(s) driven by their Workflow` とも出ている）。
   収束ループ（`internal/reconcile/loop.go` の `applyWavesAsync`）で、Workflow の対象は下流を待たせることはそのままに、skip としては出さず数えないようにした。
   受け入れテスト `TestWorkflowHeldTargetsAreNotSkips`（`[AC-101-1]`）。depends_on の下流（`app`）は従来どおり `skip fake//app: upstream "n1" ...` と出る。

### 直していないもの（設計の判断が要る・気づいた点）

1. **`sakutei workflow cancel` の S4 記録は任意。** `--audit-log` を付けなければ cancel は監査に残らない。却下や timeout で halt した Workflow を
   人が cancel するとやり直しになる（＝ゲートを通さずに最初からもう一度走らせる判断）ので、承認と同じく S4 を必須にするかは設計の判断。
2. **却下は誰でもできる。** requirements（roles・separation）は承認にだけ課し、却下は誰の判断でも効く（`checkDecidable` のコメント「却下 = halt は安全側」）。
   設計どおりだが、§6.4・§14.4 の本文には書かれていない。申請者本人も却下できる（自分の申請の取り下げとしては自然）。
3. **拒否した承認（403）・期限後の承認は S4 に残らない。** 「全承認者を S4 に記録」は満たしているが、要件に合わない承認の試み（申請者本人の自己承認など）は
   監査の目で見たいことがありうる。残すかは設計の判断。
4. **`sakutei workflow list` にゲートの期限と承認の数が出ない。** `GET /workflows/{id}` と `sakutei status --workflow-store`（`awaiting_approval (… 1 of 2 approval(s))`）には出る。
   **（#111 で直した）** `APPROVALS`（得た数/要る数）と `DEADLINE` の列を足した。`[AC-111-8]`（`TestWorkflowCLI`）。
5. **手では通していないもの。** 手順の後で不変条件が破れて halt する経路（同じ周の中で手順が続けて進むので、手で割り込めない。受け入れテスト
   `TestWorkflowInvariantBrokenAfterStepHalts` が確かめる）、`StageAwaitingApproval` の sink への通知（Event の sink は別の試用）、実行中の定義変更での halt。
