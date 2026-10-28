# CHANGELOG v3.26 → v3.27

**改訂日: 2026-10-28**

物理サーバの BMC・BIOS などの FW を宣言した版に合わせられるようにした改訂である（Backlog #148。親のストーリー
「FW の更新と入れ直しを宣言で扱う」の 1：管理 API〈Redfish の UpdateService〉で更新できる部品）。多相 Task（§6.2、#51）と
危険区間の人手ゲート（#52）の上に乗せたもので、設計の向きは変えていない。

## #148：Redfish の FW の更新（§6.2・§29.1）

- **背景**: FW の更新は中断できず、リブートをまたぎ、冪等でない。収束ループの Apply では扱えず、多相 Task の代表例（§6.2 の図）
  として設計だけがあった。実の provider は無く、fake provider の模型（#51）だけだった。
- **決定**（具体は §29.1「Redfish の FW の更新」）:
  - provider `redfish-firmware`。ID は `<ノード名>/firmware/<部品の Id>`（FirmwareInventory の Id）。観測は部品の `Version`。
  - 宣言は版・イメージの在りか・`sha256`・送り方（SimpleUpdate / multipart）・ApplyTime・再起動の有無・待つ上限・
    イメージの資格（`image_password` は SecretRef だけ）。版以外は PlanRefiner で差分から落とす。
  - 版の比べ方は前後の空白を除いた文字列の完全一致。それ以上は正規化しない。
  - `strategy: task` 専用（Apply は拒む、§6.5）。`rollback_on_fail` は書けない。版の違いの Op は破壊的（§13 の予算に数える）。
  - 相は verifying（承認の要る相）→ staging → applying（危険区間）→ rebooting → confirming。各相の入口で、版と
    「この更新の BMC の Task」（TaskService の Task の Payload で見つけ直す。URI は持たない）を観測して済んだかを問う。
  - 前の更新の残りかす（同じイメージで前に済んだ BMC の Task が残ったまま、人が版を戻した）で相を飛ばさない:
    verifying（承認の要る相）は版が宣言どおりのときだけ済み。BMC の Task は StartTime を時刻として読み、この Task の
    起動の時刻（S2 の記録の `started_at`、`task.Request.StartedAt` として実行体に渡す）から時計のずれの許し（既定 5 分）を
    引いた時刻より前のものを数えない。StartTime を読めない Task は数える（二重に送る側に倒さない）。
- **§6.2 に足した決まり：承認の要る相**: 相の宣言に「入口で済んでいなければ、手順の前に人の承認を待つ」を持てる。
  ゲートは #52 の人手ゲートと同じもの（S2 の記録を `awaiting_approval` にし、`sakutei task approve` / `reject`、S4 監査に
  残す、既定 24h で halt、待つ間に Observe で済んだと見えれば承認なしで閉じる）。記録に `gate: approval` と、承認した相
  （`approved_step`）を置く。承認はその相を手順してよいという判断で、その相の入口で引き継いだときだけ効く。
- **§6.2 に足した決まり：待つ相の引き継ぎ**: Await の相（rebooting）の RunStep が返ったら、Critical の印を下ろすのと同じ
  書込で S2 の記録に `awaiting` の印を置く（相が替われば下ろす）。待ちの間に落ちた Task を引き継いだデーモンは、その相では
  入口の観測も RunStep もせずに待ちを続ける。redfish-firmware は再起動を宣言したとき rebooting を危険区間にし、Reset を
  送る最中に落ちた Task は人手ゲート（再起動で版が合ったと観測できれば承認なしで閉じる）。入口の観測は版しか見ないので、
  これが無いと、ホストがまだ起動中・部品を書いている途中に二度目の Reset を送ってしまう。
- **予算と承認の役割**: 予算（§13）は「一度にいくつまで」を、承認ゲートは「この 1 台を流してよいか」を決める。
  どちらかが止めれば Task は起きない／流れない。
- **見送り**:
  - まとめて承認する口（planned_change の承認で個々の Task の承認に代える）。今は Task ごとに単一承認。
  - 部品の版の互換の表・全体を止める型の更新・管理 API に更新の口が無い装置（親のストーリーの 2〜4）。
- **統合テスト**: qemu-bmc 0.10.0 は UpdateService・TaskService を持たないので、統合テストは偽のサーバまで。

## 受け入れテスト

`internal/redfish/system/firmware_test.go`（偽 BMC `internal/redfish/redfishtest` の上の契約テストと単体）:

- `[AC-148-1]` 契約テスト（相を順に走らせるアダプタで、Apply 相当が契約を満たす。2 回目は版を見て何もしない）。
- `[AC-148-2]` 版が合えば差分なし（前後の空白は比べない・大文字と小文字は区別する）。違えば版だけの破壊的な Op。
- `[AC-148-3]` 更新できない部品・無い部品・UpdateService / TaskService / MultipartHttpPushUri の無い BMC・選べない ApplyTime は計画の時点のエラー。
- `[AC-148-4]` 承認の前は BMC に何も送らず、承認の後で SimpleUpdate（資格は SecretRef から解決）を 1 回、OnReset の保留を
  宣言した再起動で効かせ、版が合って閉じる。資格の値は記録・監査に出ない。
- `[AC-148-5]` BMC の Task が Exception で終われば失敗（送り直さない）。
- `[AC-148-6]` `sha256` が合わなければ BMC に送らない。multipart は確かめた中身をそのまま送る。
- `[AC-148-12]` 前の更新の残りかすの BMC の Task を「もう送った」と数えない（verifying は版だけで決める・起動より前の
  Task は数えない・StartTime は時刻として比べる・時計のずれの許しの内は数える）。

`tests/acceptance/redfish_firmware_test.go`（常駐ループ）:

- `[AC-148-7]` 版が違えば多相 Task が起き、承認ゲートで止まり、承認で流れて、版を観測して閉じる。Apply はしない。閉じたあとは何もしない。
- `[AC-148-8]` 承認が無ければ何周回しても流さない。却下は halt。
- `[AC-148-9]` 版が合っていれば Task を起こさず、BMC に何も書かない。
- `[AC-148-10]` staging の後で落ちた Task は送り直さない。applying の途中で落ちた Task は人手ゲートで止まり、BMC が書き終えて版が合えば承認なしで閉じる。
- `[AC-148-11]` 更新は破壊的として予算に数え、超えれば Task を起こさない。
- `[AC-148-14]` rebooting の待ちの間に落ちた Task は二度目の Reset を送らずに待つ。Reset の最中に落ちた Task は人手ゲートで
  止まり、再起動で版が合えば承認なしで閉じる。
- `[AC-148-13]` 前の更新の Completed の Task が残ったまま版が戻った BMC で、承認なしでは何も送らず再起動もしない。承認すると
  送り直して版を合わせる。

`internal/task/phased_test.go`: 承認の要る相（承認で続ける・却下で halt・待つ間に済めば承認なしで閉じる）と、待つ相の
引き継ぎ（`awaiting` の印のある相は RunStep をもう一度呼ばない）。
