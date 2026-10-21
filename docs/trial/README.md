# v1 を実際に使って確かめる手順

v1（タグ v1.0.0）を、ファイル系の実 provider の dnszone（DNS のゾーンファイル）と nftables（ファイアウォールの ruleset ファイル）を相手に、
`import` → `plan` → `reconcile` → 手でずらす → `status` → 常駐デーモン、の順で動かして確かめる。
v2（Backlog #20）に入るかどうかは、この手順で v1 を使ってみてから決める（2026-10-21 の決定）。

## まず知っておくこと

- **相手はファイルだけ**。dnszone はゾーンファイルを、nftables は ruleset ファイルを読み書きする。DNS サーバへの反映（再読み込み）や
  カーネルへの `nft -f` は Sakutei はしない。実機で試すなら、書き出されたファイルを自分で読み込ませる（下の「実機につなぐ」）。
- **devenv について**：設計書の devenv（§26.2、物理 1 台にネストで段 0〜段 2 を再現する）はまだ無い。この手順は 1 つの作業ディレクトリ
  （既定 `/tmp/sakutei-trial`）で完結する、いちばん小さい devenv である。
- **ID の書き方**：dnszone は「ゾーンファイルに書いた名前/型」（例 `www/A`。FQDN にはしない）。nftables は「family/table/chain」
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
ONE="--intent intent --zone example.com.zone --state firewall.rules"
```

`plan`・`reconcile` には今のところ nftables 用の `--ruleset` が無いので、ruleset のパスは `--state` で渡す（Backlog #67 で `--ruleset` を足す）。
`status`・`import` には `--ruleset` がある。

| 手順 | コマンド | 確かめること |
|---|---|---|
| 1. import | `./sakutei import --provider dnszone,nftables --zone example.com.zone --ruleset firewall.rules --out imported --samples 1` | いまの現実が intent の形で `imported/` に出る。全部 `ExplicitAbsentOnly`（消さない）。base chain の `input` は出ない（管理の外） |
| 2. plan | `./sakutei plan $ONE` | `www` が UPDATE、`api` が CREATE、`sakutei_allow` が UPDATE（cause A／B 付き）。ファイルは変わらない |
| 3. reconcile | `./sakutei reconcile $ONE` → もう一度 `plan` | 3 件が適用され、2 回目の plan は `Converged (no changes)` |
| 4. ずれ | ゾーンファイルの `api` を手で書き換え → `./sakutei status --intent intent --zone example.com.zone --ruleset firewall.rules` → `reconcile` | status に `DRIFTED dnszone//api/A`（cause B）が出て、reconcile で元に戻る |
| 5. alert-only | `www` を手で書き換え → `./sakutei reconcile $ONE --mode alert-only` | 「would be UPDATE」と知らせるだけで、ファイルは書き換えない |
| 6. 消さない | intent から `api` を消して `plan` | `Converged`（削除の計画が出ない）。現実の `api` は残る。消すには `deletion_policy: PruneWithinScope` を明示する |

## 常駐デーモン（Backlog #67 が直ってから）

いまのデーモンは fake provider にしか対応していない（provider を決め打ちしている）ので、dnszone・nftables の intent は回せない。
#67 が直ったら、次の形で回す予定：

```sh
./sakutei --intent intent --zone example.com.zone --ruleset firewall.rules --interval 5s
```

確かめること：起動して数周で収束する／別の端末でゾーンファイルや ruleset を手で変えると、次の周期で戻る／
`--mode alert-only` では戻さずに知らせる／`--max-destructive-per-cycle 1` で、intent から 2 件を `PruneWithinScope` で消すと
halt する／Ctrl-C で周期の切れ目で止まる。

## 実機につなぐ（任意）

ファイルを書き出すだけなので、実機で効き目を見るには自分で読み込ませる。

- DNS：BIND なら `named-checkzone example.com example.com.zone` で確かめ、`rndc reload`。
  **注意**：Backlog #70 が入るまで、Sakutei は SOA のシリアルを上げない。セカンダリへの転送はシリアルが変わらないと起きない。
- nftables：`nft -c -f firewall.rules` で文法を確かめる（`-c` は読み込まない）。読み込むなら、ネットワーク名前空間など壊れてもよい場所で。

## この手順を作ったときに見つかったこと（2026-10-21）

| 見つかったこと | 扱い |
|---|---|
| デーモンが fake provider にしか対応していない。plan・reconcile に `--ruleset` が無い | Backlog #67（バグ） |
| **nftables の Apply が base chain の `type … hook … priority …; policy …;` の行を消す**（直す前は、手順 3 の後の ruleset で `input` の宣言行が消えていた） | Backlog #68（バグ・安全）。直した：Apply は管理する chain の行だけを書き直し、base chain・set/map などの行はバイト単位で残す |
| dnszone が $ORIGIN を解釈しない。ゾーンに絶対名 `www.example.com.` で書いたレコードを、intent の `www/A` は「無い」とみなして CREATE する（同じ名前が 2 つになる）。パッケージコメントの ID の例（`www.example.com./A`）も実装と食い違う | Backlog #69（バグ・identity）。直るまで、ゾーンファイルの名前は相対名で統一して試す |
| SOA のシリアルが上がらない | 設計書 v3.12 §29.1 で「書き換えた Apply のときだけ YYYYMMDDnn で上げる（既定。`--zone-soa-serial keep` で止める）」と決めた。Backlog #70 で実装 |

## 試した結果の残し方

v2 に入るかを決める材料にするので、気づいたことは Backlog に積む。

- 動かない・危ない：kind `bug`。再現の手順（このページの手順の番号）を書く。
- 使いにくい・足りない：kind `task`、ラベル `trial-feedback`。
- 設計を変えないと直らない：本文にそう書き、人に聞く。
