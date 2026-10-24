# CHANGELOG v3.18 → v3.19

**改訂日: 2026-10-24**

v1 を実際に使う（dogfooding）試用の中で見つかった既存の設計の穴を2つ、人が決定し実装・テスト済み。両方とも Backlog の
`needs-decision` から着手済みで、設計を変えずに決め方を書き足す改訂である。

## Backlog #105：allocator Lookup キャッシュの cooldown 衝突バグ

**問題**：§19.6 は「割当事実は Release まで不変だから `Lookup` の結果は無期限キャッシュしてよい——無効化は自分の
`Release` のときだけ」としていた。だが v1 には `POST /v1/release`（手の解放。§19.9 の縮退から外の運用経路）という、
どのシャードの「自分の Release」でもない形で割当事実を変える経路がある。無期限キャッシュのままだと、この経路で
解放された値をシャードが知らずに使い続け、cooldown 明けに別 owner へ再払い出しされて衝突しうる（§19.5 が
最悪級と呼ぶ事故）。

**決定（2026-10-24 人が決定。案B＝キャッシュに TTL を入れて定期的に裏で確かめ直す、の折衷）**：

- Lookup キャッシュ（`internal/allocator.Cache`）のエントリに、確かめた時刻を持たせる。
- **TTL（既定 `DefaultLookupTTL` = 15 分、cooldown の既定 24h より十分短い）を超えたエントリは、使う前に `Lookup`
  で 1 回だけ確かめる**。authority にまだ同じ owner の記録があればその値を使い続け（値が動いていれば更新）、
  無くなっていれば（他所で解放された）キャッシュを捨てて新たに `Allocate` する。
- authority に届かないときは確定した無効化の証拠が無いので、古い値を使い続ける（§19.9 の waiting-allocation は
  新規の採番だけに掛かる仕組みを変えない）。
- 仮予約（`Reserve`）は元から未キャッシュのままで変更なし。
- 定常状態のコール数は依然「周回数」ではなく「churn + 1/TTL」に比例する——secret の last-resolved + TTL
  キャッシュ（§20.3）と同型になった。

**変更箇所**：`docs/architecture/spec/part6-satellites.md`（§19.6 の無期限キャッシュの記述を TTL 付きに改訂）、
`docs/architecture/spec/part8-implementation.md`（§29.1 に新しい行を追加）。実装は `internal/allocator/cache.go`
（既存）。

## Backlog #106：dnszone / nftables provider の scope 固定（Option A）

**問題**：dnszone・nftables provider の `List` / `BulkObserve` は常に root scope（ラベル無し）で ID を返していた。
このため：

- `PruneWithinScope`（§12.2）で非 root の scope を宣言すると、`inAnyScope` が一致せず orphan が宣言した境界の外
  （open-world）として扱われ、**消えないまま黙って残る**（prune が効かない。エラーも出ない）。
- Sync Mode（§24.5）が非 root の sync scope を宣言すると、importer の `Selector.Matches` が常に false になり、
  **generated/ のスナップショットが常に 0 件になる**（実際にレコード／チェインがいくつあっても）。

**決定（2026-10-24 人が決定。案A＝provider 設定で固定 scope）**：

- provider インスタンスごとに固定の scope を 1 つ設定できるようにする（`dnszone.WithScope(scope)` /
  `nftables.WithScope(scope)`、CLI は `--zone-scope` / `--ruleset-scope`、`--claim` と同じ `key=value,...` 書式）。
- 設定されていれば、`List` / `BulkObserve` が返す全 ID をその scope で返す。呼び出し側が渡す scope 引数
  （List の第2引数）は無視する——1 インスタンス 1 scope に固定するためで、動的な切替は持たない。
- 未設定の既定は従来どおり root scope。
- これにより、非 root scope の `PruneWithinScope` と Sync Mode は、provider 側の scope 設定を宣言と正しく
  合わせれば動くようになる。合わせ忘れれば従来どおり（orphan が残る／スナップショットが 0 件になる）なので、
  設定の運用規律は利用者の責任である。

**変更箇所**：`docs/architecture/spec/part8-implementation.md`（§29.1 に新しい行を追加）。実装は
`internal/dnszone/dnszone.go`・`internal/nftables/nftables.go`・`cmd/sakutei/providers.go`（`--zone-scope` /
`--ruleset-scope` フラグ）・`pkg/registry/default.go`。受け入れテストは
`tests/acceptance/scope_provider_test.go`（AC-106-1〜AC-106-5）。
