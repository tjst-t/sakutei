# v1 を実際に使って確かめる手順

v1（タグ v1.0.0）を、ファイル系の実 provider の dnszone（DNS のゾーンファイル）と nftables（ファイアウォールの ruleset ファイル）を相手に、
`import` → `plan` → `reconcile` → 手でずらす → `status` → 常駐デーモン、の順で動かして確かめる。
v2（Backlog #20）に入るかどうかは、この手順で v1 を使ってみてから決める（2026-10-21 の決定）。

## まず知っておくこと

- **相手はファイルだけ**。dnszone はゾーンファイルを、nftables は ruleset ファイルを読み書きする。DNS サーバへの反映（再読み込み）や
  カーネルへの `nft -f` は Sakutei はしない。実機で試すなら、書き出されたファイルを自分で読み込ませる（下の「実機につなぐ」）。
- **devenv について**：設計書の devenv（§26.2、物理 1 台にネストで段 0〜段 2 を再現する）はまだ無い。この手順は 1 つの作業ディレクトリ
  （既定 `/tmp/sakutei-trial`）で完結する、いちばん小さい devenv である。
- **ID の書き方**：dnszone は「FQDN/型」（例 `www.example.com./A`。`name` も同じ FQDN）。ゾーンファイルの `$ORIGIN` と `@` は解釈するので、
  ファイルに相対名 `www` で書いても絶対名 `www.example.com.` で書いても同じレコードになる（`www.` は別のレコード）。intent に相対名の ID
  （`www/A`）を書くとロードで拒否される。書き戻しはファイルの書き方を保つ（相対名の行は相対名のまま書き換え、新しいレコードは FQDN で足す）。
  nftables は「family/table/chain」
  （例 `inet/filter/sakutei_allow`）。迷ったら手順 1 の `import` が出す ID をそのまま使う。
- **nftables の rules** は順序つきの 1 本の文字列で、ルールの区切りは ` ; `。並べ替えも差分になる。

## 置いてあるもの（docs/trial/v1/）

| ファイル | 中身 |
|---|---|
| `initial/example.com.zone` | 最初の現実。`www` の A レコードだけがある |
| `initial/firewall.rules` | 最初の現実。base chain の `input`（policy drop）と、管理する regular chain の `sakutei_allow`（22 番だけ許可） |
| `intent/main.yaml` | あるべき姿。`www` を 192.0.2.20 に、`api` を足す、`sakutei_allow` に 443 番を足す |
| `run.sh` | 手順 1〜6 を通して流すスクリプト |

## 通しで流す

```sh
docs/trial/v1/run.sh            # 作業ディレクトリは /tmp/sakutei-trial（毎回作り直す）
```

Go が要る（`go build` する）。リポジトリのファイルは書き換えない。

## 1 つずつ流す

`run.sh` と同じことを手で行う。作業ディレクトリで：

```sh
W=/tmp/sakutei-trial; rm -rf $W; mkdir -p $W
cp docs/trial/v1/initial/* $W/; cp -r docs/trial/v1/intent $W/
go build -o $W/sakutei ./cmd/sakutei
cd $W
ONE="--intent intent --zone example.com.zone --ruleset firewall.rules"
```

`plan`・`reconcile`・`status`・`import`・デーモンのどれも、dnszone のゾーンファイルは `--zone`、nftables の ruleset は `--ruleset` で渡す（fake provider は `--state`）。

| 手順 | コマンド | 確かめること |
|---|---|---|
| 1. import | `./sakutei import --provider dnszone,nftables --zone example.com.zone --ruleset firewall.rules --out imported --samples 1` | いまの現実が intent の形で `imported/` に出る。全部 `ExplicitAbsentOnly`（消さない）。base chain の `input` は出ない（管理の外） |
| 2. plan | `./sakutei plan $ONE` | `www` が UPDATE、`api` が CREATE、`sakutei_allow` が UPDATE（cause A／B 付き）。ファイルは変わらない |
| 3. reconcile | `./sakutei reconcile $ONE` → もう一度 `plan` | 3 件が適用され、2 回目の plan は `Converged (no changes)` |
| 4. ずれ | ゾーンファイルの `api` を手で書き換え → `./sakutei status --intent intent --zone example.com.zone --ruleset firewall.rules` → `reconcile` | status に `DRIFTED dnszone//api.example.com./A`（cause B）が出て、reconcile で元に戻る |
| 5. alert-only | `www` を手で書き換え → `./sakutei reconcile $ONE --mode alert-only` | 「would be UPDATE」と知らせるだけで、ファイルは書き換えない |
| 6. 消さない | intent から `api` を消して `plan` | `Converged`（削除の計画が出ない）。現実の `api` は残る。消すには `deletion_policy: PruneWithinScope` を明示する |

## 常駐デーモン

デーモンは intent に出てくる provider 名ごとに provider を引いて回す（Backlog #67）。dnszone・nftables の intent は次の形で回す：

```sh
./sakutei --intent intent --zone example.com.zone --ruleset firewall.rules --interval 5s
```

別の端末からゾーンファイルや ruleset を手で書き換えて、次の周期の動きを見る。止めるのは Ctrl-C（周期の切れ目で止まる）。

### D1. 収束と自己修復（2026-10-21 に確かめた）

初期状態（`docs/trial/v1/initial/`）から `--interval 1s` で起動する。

- cycle 1 で 3 件を適用し、cycle 2 から `converged (no changes)`。SOA のシリアルは DNS の書き換え 2 回ぶん上がる（…01 → …03）。
- ゾーンファイルの `api` の値と ruleset の `tcp dport 443 accept` を手で書き換えると、次の周期に
  `DriftEvent ... [cause: B] drift B: value was "192.0.2.30" prev cycle now "198.51.100.99"` が出て、元に戻る。
- base chain の `type filter hook input priority 0; policy drop;` は残る。

### D2. alert-only（2026-10-21 に確かめた）

収束させてから `--mode alert-only` を足して起動し、D1 と同じように手で書き換える。

```sh
./sakutei --intent intent --zone example.com.zone --ruleset firewall.rules --interval 1s --mode alert-only
```

- 書き換えた次の周期から毎周、次のように出て、**ファイルは書き換えない**（手で変えた値が残り、SOA のシリアルも変わらない）。

  ```
  sakutei: cycle 4: alert-only: 2 drifted resource(s) detected — no changes applied
  sakutei: cycle 4: DRIFT EVENT: would be UPDATE dnszone//api.example.com./A
  sakutei: cycle 4: DRIFT EVENT: would be UPDATE nftables//inet/filter/sakutei_allow
  ```
- 気づいたこと：同じ知らせが毎周そのまま出続ける。設計 §22.3 は「firing / resolved の level 報告で、毎周同じアラートを撒かない」と決めているので、Backlog #74 で実装の状況を確かめる。

### D3. 破壊的な操作の上限で halt（2026-10-21 に確かめた）

ゾーンに intent に無いレコードを 2 件置き、intent 側を `deletion_policy: PruneWithinScope` にして、上限 1 件で起動する。

```sh
# ゾーン：www・old1・old2 の 3 件。intent：www だけを PruneWithinScope で宣言（old1・old2 が prune の対象になる）
./sakutei --intent intent --zone example.com.zone --ruleset firewall.rules --interval 1s --max-destructive-per-cycle 1
```

- 毎周、2 件の DELETE を止めて halt する。**1 件も消さない**（上限 1 件に対して 2 件なので、1 件だけ消すこともしない）。

  ```
  sakutei: budget: blocked destructive op DELETE dnszone//old1.example.com./A (budget limit)
  sakutei: budget: blocked destructive op DELETE dnszone//old2.example.com./A (budget limit)
  sakutei: cycle 1: BUDGET HALTED: rate-limit (per-cycle): 2 destructive ops (+ 0 in-flight = 2) exceeds max_destructive_per_cycle=1 (approve-large-change=0)
  ```
- `--approve-large-change 2` を足して起動し直すと、cycle 1 で 2 件を消す（`pruned=2`）。SOA のシリアルは 2 つ上がる。
- 気づいたこと（2 つ）：
  - halt した周の要約にも `converged (no changes)` と出て紛らわしい。Backlog #73（直した：止めた操作がある周の要約は `halted (N op(s) held)`）。
  - 承認して 2 件を消したあと、cycle 2 から毎周 `prune HALT: List count dropped from 3 to 1 (67% > threshold 50%)` で prune が止まり続ける。
    件数が減ったのは Sakutei 自身が承認つきで消したからだが、急減の判定（§12.4（b））は planned_change で消した分しか差し引かない。
    また、止まった周は比べる基準の件数を更新しないので、デーモンを起こし直すまで止まり続ける。人に判断を仰いでいる。

## 実機につなぐ（任意）

ファイルを書き出すだけなので、実機で効き目を見るには自分で読み込ませる。

- DNS：BIND なら `named-checkzone example.com example.com.zone` で確かめ、`rndc reload`。
  レコードを書き換えた Apply では、Sakutei が同じ書き込みで SOA のシリアルを `YYYYMMDDnn`（UTC）に上げるので、セカンダリへの転送も起きる。
  ゾーンのシリアルを別のツールや人が採番しているなら、`--zone-soa-serial keep` で上げないようにする。
- nftables：`nft -c -f firewall.rules` で文法を確かめる（`-c` は読み込まない）。読み込むなら、ネットワーク名前空間など壊れてもよい場所で。

## この手順を作ったときに見つかったこと（2026-10-21）

| 見つかったこと | 扱い |
|---|---|
| デーモンが fake provider にしか対応していない。plan・reconcile に `--ruleset` が無い | Backlog #67（バグ）。直した（デーモンに `--zone`・`--ruleset`、plan・reconcile に `--ruleset`） |
| **nftables の Apply が base chain の `type … hook … priority …; policy …;` の行を消す**（直す前は、手順 3 の後の ruleset で `input` の宣言行が消えていた） | Backlog #68（バグ・安全）。直した：Apply は管理する chain の行だけを書き直し、base chain・set/map などの行はバイト単位で残す |
| dnszone が $ORIGIN を解釈しない。ゾーンに絶対名 `www.example.com.` で書いたレコードを、intent の `www/A` は「無い」とみなして CREATE する（同じ名前が 2 つになる）。パッケージコメントの ID の例（`www.example.com./A`）も実装と食い違う | Backlog #69 で直した。名前は $ORIGIN・`@` を解いた FQDN で比べ、ID は `www.example.com./A`。相対名の ID はロードで拒否する |
| デーモンの非同期 Apply が同じファイルを同時に書き換え、変更を取りこぼす（初期状態から回すと cycle 2 で同じリソースを launch し直す。次の周期で直る）。2026-10-21 にデーモンを回して確認 | Backlog #72（バグ）で直した。dnszone・nftables の Apply は、ファイルを読んで書き換えて rename するまでを、ファイルのパスごとの排他（プロセス内の mutex と `<ファイル>.lock` の flock）の中で行う。Read・List・BulkObserve は共有の排他を取る。そのため作業ディレクトリに `example.com.zone.lock`・`firewall.rules.lock` ができる |
| SOA のシリアルが上がらない | 設計書 v3.12 §29.1 で「書き換えた Apply のときだけ YYYYMMDDnn で上げる（既定。`--zone-soa-serial keep` で止める）」と決め、Backlog #70 で直した。括弧で複数行に分けた SOA も読めるようにした（シリアルの数字だけを置き換える） |

## 試した結果の残し方

v2 に入るかを決める材料にするので、気づいたことは Backlog に積む。

- 動かない・危ない：kind `bug`。再現の手順（このページの手順の番号）を書く。
- 使いにくい・足りない：kind `task`、ラベル `trial-feedback`。
- 設計を変えないと直らない：本文にそう書き、人に聞く。
