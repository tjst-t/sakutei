# provider の設定の一覧：つなぎ方（設定に残す）と範囲（intent に書く）

provider の設定は**つなぎ方**（宛先・鍵・時間・どう話すか）だけにし、**管理の範囲**（何を管理するか）は intent のトップレベルの
`providers:` に書く（設計書 §1.7・§7.7、Backlog #181・#182）。除外（触らないもの）は `external_owners:`（§7.3（c）、#158）に書く。
この文書は組み込みの provider の設定のキーを、どちらに置くかで分けた一覧である。決まりの正は設計書 §7.7。矛盾したら spec/ が優先する。

## 分け方

- **範囲**：provider のインスタンスが「どれを管理するか」を決めるもの。
  - provider が ID・List に付ける **scope**（prune の集合差・`--claim` の境界になる）。
  - ID の頭になる**対象の集合**（ホスト・装置。この集合に無い ID は読み込みで断られる）。
  - 管理の**木の根と条件**（LDAP の検索の根と filter、S3 のバケットの接頭辞、nftables の管理する表、NETCONF の部分木）。
  - 観測の**目印**（guest-os の導入の目印。何を読むかを宣言の側で決める）。
- **つなぎ方**：どこに・どの鍵で・どれだけ待って・どう話すか。宛先（アドレス・URI・endpoint・ファイルのパス）、資格と secret store、
  時間、コマンドのパス、機種の定義（どのコマンドで観測し変えるか）、実装の能力（S3 の endpoint が持つ API）、ホストの上の置き場、
  書き込みを許すか（`netconf_apply`）。観測用と書き込み用の資格の分け（対象の一覧のホストの `observe:`・`write:`、§20.7、#185）も
  つなぎ方。この回が書くか（`access`）は本体が命令と mode から決め、設定にも intent にも書かない。
- **対象の集合**と**配る先**は分ける。host-file・systemd-unit・cli-device・netconf・guest-os のホストは ID の頭で、どの ID を管理するかを
  決めるので範囲。dnszone の `zone_hosts`・nftables の `ruleset_hosts` は同じ 1 つのゾーン・ruleset を写す先（ID に現れない）で、
  つなぎ方として設定に残す。

## 移し方と食い違い（どの provider も同じ）

- intent にだけ書けば、設定に書いたのと同じに動く（範囲だけを intent に書き、設定はつなぎ方だけにできる）。
- 設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が「is only in the provider config: write it in the intent
  too (providers.<名前>.<キー>)」と警告する（常駐では一度だけ）。
- 両方にあれば、設定は安全の網として残る。**正規形で比べて違えば読み込みで断る**（弱い側に寄せない）。常駐は起動で断り、動いている
  あいだに intent が食い違うように変われば、その周を飛ばす。
- 知らないキー・値の書き違い・設定のファイルに無いもの（サーバ・endpoint・クラスタ）を指す範囲は読み込みで断る。
- 設定から範囲を外す（設定に書けなくする）のは次のメジャー版。

## 一覧

intent の列は `providers.<provider の名前>.` の下のキー。「—」は範囲が無い（全部がつなぎ方）。

| provider | 範囲（intent の `providers:` に書く） | 元の設定 | 正規形（比べ方） | つなぎ方（設定に残す） |
| --- | --- | --- | --- | --- |
| dnszone（#181） | `scope` | `zone_scope`（`--zone-scope`） | scope の文字列 | `zone`（`--zone`・`--state`）・`soa_serial`・`zone_reload`・`zone_hosts`・`remote_config` |
| nftables（#181） | `scope`・`tables`（live だけ） | `ruleset_scope`・`ruleset_tables` | scope・並べ替えた表の組 | `ruleset`・`ruleset_target`・`ruleset_nft`・`ruleset_hosts`・`remote_config`・`ruleset_rollback_timeout`・`ruleset_confirm_address` |
| proxmox-vm（#181） | `clusters.<名前>.scope` | `proxmox_config` の `clusters.<名前>.scope` | scope | `proxmox_config` のほか（endpoint・`api_base_path`・token・node・CA）。触らない VM は `external_owners:` |
| netconf（#182） | `hosts`・`subtrees.<名前>.filter` | `netconf_hosts`・`netconf_subtrees` のファイルの `subtrees.<名前>` | 並べ替えたホストの組・filter の XML の正規形（名前空間は URI で、空白・兄弟の順を問わない） | `remote_config`・`netconf_apply`・`netconf_confirm_timeout`・`netconf_commit_flow`。長い filter の `filter_file` は部分木の一覧のファイルにだけ書ける |
| ldap（#182） | `scope`・`servers.<名前>.base_dn`・`servers.<名前>.filter`・`servers.<名前>.scope` | `ldap_config` のファイルの同じキー | scope・DN の正規形（大文字小文字を問わない）・filter はそのまま | `ldap_config` のほか（`ldapsearch`・`ldapmodify`・`secret_store`・`uri`・`starttls`・`ca_cert`・`bind_dn`・`bind_password`・`timeout`） |
| cli-device（#182） | `hosts` | `cli_hosts` | 並べ替えたホストの組 | `remote_config`・`cli_definition`・`cli_definitions`・`secret_store`。管理する項目（`<装置>/<項目>`）は前から intent の `resources:`（prune しない） |
| guest-os（#182） | `hosts`・`markers.<名前>`（絶対パス） | `guest_hosts`・`guest_markers` | 並べ替えたホストの組・パス | `remote_config` |
| host-file（#182） | `hosts` | `file_hosts` | 並べ替えたホストの組 | `remote_config`・`stamp_dir`・`secret_store`。管理するファイルは前から intent の `resources:`（prune しない） |
| systemd-unit（#182） | `hosts` | `unit_hosts` | 並べ替えたホストの組 | `remote_config`・`stamp_dir`・`unit_dir`・`quadlet_dir`（ホストの上の置き場）。管理するユニットは前から intent の `resources:` |
| postgresql（#182） | `scope`・`servers.<名前>.scope` | `postgresql_config` のファイルの同じキー | scope | `postgresql_config` のほか（`psql`・`secret_store`・host・port・dbname・user・password・sslmode・sslrootcert・connect_timeout・verify_passwords） |
| s3-bucket（#182） | `scope`・`endpoints.<名前>.bucket_prefix` | `s3_config` のファイルの同じキー | scope・接頭辞はそのまま | `s3_config` のほか（`secret_store`・endpoint・`api_base_path`・region・addressing・鍵・CA・timeout・`fields`（実装が持つ API）・`storage_class_aliases`） |
| redfish-system・redfish-virtual-media・redfish-bios・redfish-firmware | — | | | `redfish_config`（BMC の一覧） |
| sqlmigrate | — | | | `sqlmigrate_config`（データベースとマイグレーションの置き場。何を当てるかは intent の `resources:`） |
| ansible | — | | | `ansible_config` |
| template-file | — | | | `template_dir`・`output_dir` |
| metrics-probe | — | | | `metrics_probe_config`（endpoint と probe） |
| fake | — | | | `state` |

- scope は intent ではラベルの map（`site: tokyo`）で書く（resources の scope と同じ形）。設定の側の `"site=tokyo"` と同じ値として比べる。
- ホストの組の順は問わない（`[web2, web1]` と `web1,web2` は同じ）。同じホストを 2 回書けば断る。
- 範囲を intent から受け取らない provider（表の「—」）の名前を `providers:` に書けば断る。

## 例

```yaml
# intent/providers.yaml
providers:
  netconf:
    hosts: [sw1, sw2]
    subtrees:
      if-uplink:
        filter: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"><interface><name>uplink</name></interface></interfaces>'
  ldap:
    servers:
      dir1:                         # ldap_config にあるサーバ（つなぎ方はそちら）
        base_dn: dc=example,dc=com
        filter: (!(ou=outside))
  cli-device:
    hosts: [sw1, sw2]
  guest-os:
    hosts: [node1, node2]
    markers:
      installed: /etc/sakutei-installed
  host-file:
    hosts: [web1, local]
  systemd-unit:
    hosts: [web1]
  postgresql:
    scope:
      site: tokyo
  s3-bucket:
    endpoints:
      minio:
        bucket_prefix: app-
```

```yaml
# providers.yaml（--provider-config）：つなぎ方だけ
providers:
  netconf:
    remote_config: hosts.yaml
  ldap:
    ldap_config: ldap.yaml          # servers.dir1 に uri・bind_dn・bind_password（base_dn・filter は書かない）
  cli-device:
    remote_config: hosts.yaml
    cli_definition: example-switch.yaml
  guest-os:
    remote_config: hosts.yaml
  host-file:
    remote_config: hosts.yaml
  systemd-unit:
    remote_config: hosts.yaml
```

外の provider も同じ口で範囲を intent から受け取れる（`pkg/registry` の `RegisterIntentRanges`、§28.9）。
