# dnszone provider（dnszone）

RFC1035 形式のゾーンファイルの DNS レコードを収束させる provider（v1a の第一号、§27.2）。決まりの正は設計書 §29.1（「dnszone の SOA シリアル」Backlog #70、「dnszone / nftables の scope 固定」#106、「離れたホストを扱う口：ssh 越しの実行の層」#130）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `dnszone` | `<FQDN>/<TYPE>`（例 `www.example.com./A`）。ホストを設定すると `<ホスト>/<FQDN>/<TYPE>` | 1 つの名前・型のレコード |

- フィールドは `name`・`type`（identity）と `value`・`ttl`。ID の FQDN は小文字、型は大文字で書く（List が返す形。違えば intent を読む時点で断る）。
- 完全階級（List・BulkObserve を持つ）。prune できる（`pruneable: true`）。
- SOA と NS は管理の外（観測も Apply もしない）。Apply がファイルを実際に書き換えたときだけ、同じ書き込みで SOA のシリアルを上げる（`--zone-soa-serial keep` で止める）。
- 書き込みは一時ファイル → rename（原子的）。Apply は `<zone>.lock` の flock で排他し、Read・List・BulkObserve は共有のロックで読む。読めない行が 1 つでもあれば、ゾーン全体を観測できない（unknown）にする。

## 設定

| フラグ | `--provider-config` のキー | 意味 |
| --- | --- | --- |
| `--zone <file>` | `zone` | ゾーンファイル（無ければ `--state`、それも無ければ `sakutei-zone.db`） |
| `--zone-soa-serial bump\|keep` | `soa_serial` | 書き換えたときに SOA のシリアルを上げるか（既定 `bump`） |
| `--zone-scope <selector>` | `zone_scope` | List・BulkObserve が ID に付ける固定の scope（Backlog #106） |
| — | `zone_reload` | 書き換えたあとに走らせる再読み込みのコマンド（例 `knotc zone-reload example.com.`。既定は無し） |
| — | `zone_hosts` | 扱う離れたホストの名前（`ns1,ns2`）。`remote_config` と一緒に |
| — | `remote_config` | 対象の一覧のファイル（[../remote.md](../remote.md)） |

## 再読み込み

`zone_reload` は、Apply がゾーンファイルを実際に書き換えたときだけ走る（手元のゾーンファイルでも、離れたホストでも）。空白で区切った語をそのまま引数にし、シェルは通さない。失敗は Apply の失敗として知らせ、次にそのゾーンへ Apply が来たときにもう一度走らせる。

```yaml
providers:
  dnszone:
    zone: /var/lib/knot/example.com.zone
    zone_reload: knotc zone-reload example.com.
```

## 離れたホスト

`zone_hosts` を設定すると、各ホストの上の同じパス（`zone`、絶対パス）のゾーンファイルを ssh 越しに読み書きする（Backlog #130）。設定・鍵とホスト鍵・権限・届かないときの答えは [../remote.md](../remote.md)。

```yaml
providers:
  dnszone:
    zone: /var/lib/knot/example.com.zone
    zone_hosts: ns1,ns2
    remote_config: hosts.yaml
    zone_reload: knotc zone-reload example.com.
```

- ID の先頭にホストの名前が付く（`ns1/www.example.com./A`）。同じレコードを 2 台に置くなら 2 つ宣言する。
- 読み出しは `<zone>.lock` の共有の flock の下で `cat`、Apply はホストの上で `<zone>.lock` の排他の flock を Apply の間ずっと持ち（そのホストで動く sakutei とも排他になる）、書き込みはゾーンと同じディレクトリの一時ファイル → `mv`（原子的）。置き換えるファイルのモードと持ち主は保つ（新しいファイルは 0644）。中身は標準入力・標準出力で運び、引数には載せない。
- ゾーンファイルが無いのは、そのディレクトリを辿れるときだけ「空」とする。ディレクトリが無い・辿れないときは観測できない（unknown）。
- ホストに要るのは `sh`・`cat`・`flock`（util-linux）・`mktemp`・`mv`・`chmod`・`dirname`。
