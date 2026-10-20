# Sakutei 設計書 v3.10 変更一覧（CHANGELOG）

対象: v3.9 → v3.10。改訂日 2026-10-20。骨格（§1〜§29 の機構）と設計判断（原則・機構）は不変。本改訂は **BulkObserve（§5.7）の実装（Backlog #27）に合わせ、v1 の実証対象と、収束の経路でのバルク経路の使い方を書き足す**小改訂である。

形式: **変更ID / 対応 / 章 / 変更内容 / 設計判断の理由**。対応の `#nn` は Backlog の項目番号。

---

## CHG-01 — v1 の BulkObserve は dnszone・nftables で実証する

- **対応**: #27
- **章**: §5.7・§27.3
- **変更内容**: v1 の BulkObserve の実証は、ファイルを 1 回読めば全件が分かる dnszone と nftables で行うと書いた（2026-10-20 の人の決定）。実際の一括 API を持つ対象（Proxmox・vCenter 等）は Backlog #49 に切り出した。§27.3 の v1b 状況表を「実装済み」に改めた。
- **理由**: 一貫性契約（complete-or-error・prune 非流用・`observed_at`・等価性テスト）と収束の経路での使い方は、素直なバルクで先に固められる。一括 API 固有の問題（ページング・走査窓内遷移）は対象の provider を書く段で扱う。

## CHG-02 — 収束の経路でのバルク経路の使い方

- **対応**: #27
- **章**: §5.7
- **変更内容**: 収束の経路（常駐ループの定期 Observe と `sakutei reconcile`）は SDK の `CycleObserver` を周期ごとに作り、宣言した provider は周期の最初の Observe で `BulkObserve(claim)` を 1 回だけ呼ぶ、と書いた。あわせて次を決めた。(1) 宣言と実装の両方があるときだけバルク経路を使う。(2) バルクのエラー（と `ObservedAt` の無いエントリ）は、その周期の全リソースを `Read` が `unknown` を返したときと同じに扱い、`Read` へは回さない。(3) バルクの結果に無いリソースは個別に `Read` で観測する。(4) desired の ID で引けなければ root scope の ID で引く。(5) 早期収束パスは per-resource `Read` のまま。SDK の `ObserveScope` も同じ規律にそろえ、結果に無い ID を `unknown` にする扱いを `Read` で観測する扱いに改めた。
- **理由**: (3) バルクの結果は「何が無いか」を言わない（`unknown ≠ absent`）。無い ID を `unknown` のままにすると、まだ作っていないリソースが永久に作られない。「無い」と言えるのは個別の `Read` だけにすれば、型分離を崩さずに正しく収束する。(2) バルクのエラーは「いま現実を読めない」という provider の申告であり、同じ現実を個別に読み直しても負荷を増やすだけで答えは変わらない。(4) ファイルのように scope の概念を持たない現実は、`List` と同じく root scope でキーを作るしかない。(5) 早期収束パスが見るのは影響サブ DAG の数件で、scope 全体の一括読みは割に合わない。

---

## 見送り

- **一括 API を持つ provider（Proxmox・vCenter 等）の実装**: Backlog #49。
- **`sakutei plan` / `sakutei status` のバルク経路**: 収束の経路ではないので per-resource `Read` のまま。必要になったら `CycleObserver` を使うだけで足せる。
