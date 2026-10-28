# CHANGELOG v3.43 → v3.44

**改訂日: 2026-10-28**

全体を止める型の更新を宣言で扱う改訂である（Backlog #152。親のストーリー「FW の更新と入れ直しを宣言で扱う」の 4 つめ）。
Workflow の手順ごとのリソースと `require`（#87）・通知の口（#131）・`metrics-probe`（#149）・承認ゲート（§6.4）・remediation
schedule（§14.2）の上に乗せたもので、設計の向きは変えていない。

## #152：全体を止める型の更新（§6.3・§6.4・§14.2・§22.3・§29.1）

- **背景**: 止めている間は下流が使えない更新（ゲートウェイのソフト更新など）は、いつ止めるか・どれだけ止めるか・誰が影響を受けるかを
  人に見せ、上流が溢れない時にだけ止めたい。これまでは `require` と stage のゲートを組み合わせて近いことはできたが、ゲートは
  stage の終わりにしか無く、止める前に見積もりと影響を見せる口も、止める前の知らせも無かった。
- **決定**:
  - Workflow に **`disruption:`**（`estimate`〈必須〉・`affects`〈必須、`<provider>/<id>`〉・`window`・`upstream`〈`{ resource, fields }`
    の並び〉・`max_wait`〈upstream があれば必須〉・`notify_before`・`gate`〈`manual_approval`・`timeout`、既定は単一承認・24h〉）。
    未知のキーは読み込みで断る。Task の update_policy には持たせず、止める Task は Workflow の `task` 手順で包む。
  - 対象ごとに最初の手順の前で、**時間帯**（`window` の中で、見積もりが終わりまでに収まること。remediation が alert-only の間は
    Workflow 自体が進まない）→ **上流の積もり**（観測の一致。満たさない・観測できない間は止めずに待ち、観測値を待ちの理由に出す。
    `max_wait` を過ぎたら止めずに halt して知らせる）→ **止める前の承認ゲート**（`<stage>-disruption-<n>`。見積もり・下流・上流の
    観測値・時間帯を Run の `gate_notice`・`reason` と `StageAwaitingApproval` の Event に出す）→ 承認の後に時間帯と上流をもう一度
    → **知らせ**（Event の kind `Disruption`、level・warning。出した周は止めない。`notify_before` の間も待つ）→ 手順。
  - 進みは Run の `disruption`（`waiting_since`・`approved`・`notified_at`、S2）に置き、対象を移るときに消す。上流の観測値は持たない。
  - `Disruption` は対象の手順の間（halt を含む）は firing のまま、終えた周に resolved（webhook・Alertmanager にも届く）。続いた
    周数では stuck に上げず、止め始めて見積もりを過ぎたら `overrun: true` で stuck（critical）。Workflow を進めなかった周は resolved
    にしない。
  - `affects`・`upstream` は Workflow が触るリソース（収束ループが Apply しないもの）に入れない。
- **見送り**:
  - 見積もりを過去の実績から出すこと。宣言の値だけを使う。
  - 下流を depends_on から自動で求めること。`affects` に書いたものだけを出す。
  - 止めている間の下流の Event の抑制。抑制は送り先（Alertmanager の inhibit など）に委ねる（§22.1）。

## 影響

- §6.3・§6.4・§14.2・§22.3 に決まりを、§29.1 に 1 行を足した。
- 受け入れテスト [AC-152-1]〜[AC-152-7]（`tests/acceptance/disruptive_update_test.go`）。loader の `TestDisruptionDocExampleLoads`。
