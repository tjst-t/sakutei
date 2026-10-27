# NETCONF の機器の設定を観測する provider（`netconf`）

NETCONF 対応のネットワーク機器の設定の**部分木**を観測し、宣言と比べてずれを知らせる provider（Backlog #141）。
**今は観測だけ**で、設定を機器に当てることはしない。設定を当てるのは、commit confirmed を使う 2 段の Apply を入れてから（下の「後で入れる 2 段の Apply」、設計書 §23.4「CommitConfirm が無ければ Apply 禁止」）。
決まりの正は設計書 §29.1「NETCONF の機器の設定の部分木の観測（観測だけ）」と §5.3・§23.4。ここは使い方の説明で、矛盾したら spec/ が優先する。

- 通信は NETCONF over SSH（RFC 6241・6242）。ssh 越しの実行の層（[docs/remote.md](../remote.md)、#130）の ssh で **netconf サブシステム**（`ssh -s … netconf`）を開く。機器に agent は置かない。メッセージの区切り（1.0 の `]]>]]>`、1.1 の chunked）と XML は Sakutei の中（Go の標準ライブラリ）で扱う。
- **観測だけ**（`Capabilities.ObserveOnly`）。差分は知らせ（`DriftWouldApply`）と plan の `blocked: observe_only` になるだけで、コアはどの経路でも Apply を呼ばない（remediation mode が auto でも同じ）。List・BulkObserve は持たない（prune しない、宣言外の設定は消さない）。
- 機種固有の設定の型（YANG）は持たない。利用者が機器の YANG に沿った XML を書く。この文書に機種名は出さない。

## 設定

宛先と鍵は #130 の対象の一覧（`remote_config`）と同じ形で書く。NETCONF over SSH の既定のポートは 830。

```yaml
# hosts.yaml（対象の一覧。書き方は docs/remote.md）
secret_store: secrets.json
known_hosts: known_hosts
hosts:
  sw1:
    address: 192.0.2.31
    port: 830
    user: sakutei
    private_key: secretref:file/ssh/sakutei
    connect_timeout: 5s
```

観測する部分木の「どこ」（subtree filter）は、部分木の一覧に名前をつけて書く。

```yaml
# subtrees.yaml（部分木の一覧）
subtrees:
  if-uplink:                 # 部分木の名前（ID の後ろ）
    filter: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"><interface><name>uplink</name></interface></interfaces>'
  acl-mgmt:
    filter_file: filters/acl-mgmt.xml   # 長いものはファイルで（このファイルのある所から）
```

```yaml
# providers.yaml
providers:
  netconf:
    remote_config: hosts.yaml
    netconf_hosts: sw1,sw2              # 観測する機器（必須）
    netconf_subtrees: subtrees.yaml     # 部分木の一覧（必須）
```

- `filter` か `filter_file` のどちらか 1 つ。中身は `<filter type="subtree">` の子にそのまま入る（RFC 6241 §6）。最上位の要素はどれも**名前空間を持つこと**（名前空間の無い subtree filter はすべての名前空間に当たるので断る）。
- 「どこ」を intent でなく provider の側に書くのは、観測（Read）が宣言を見ない決まりだから（ADR-0002）。どこを読むかは観測が知り、何であるべきかは宣言が持つ。
- 部分木の名前は英小文字・数字・`_`・`.`・`-`。未知のキーはエラー。
- `remote_config` の `sudo` はサブシステムには付かない。接続は ssh の多重化（`control_persist`）を使わず直につなぐ（多重化を受けない機器の ssh がある）。代わりに Sakutei が NETCONF のセッションをホストごとに 1 本使い回す。

## ID とフィールド

ID は `<ホスト名>/<部分木の名前>`（例 `sw1/if-uplink`）。

| フィールド | 中身 |
| --- | --- |
| `config` | 部分木の中身の XML（**宣言、必須**）。観測の値は機器の running の正規形（下）で、機器の順のまま |
| `ordered` | 順に意味のある列の道（`nacm/rule-list/rule` のように、部分木の頭からの要素の名前を `/` でつなぐ。カンマで複数）。比べ方の宣言で、機器には無い（それだけでは差分にならない） |
| `host`・`subtree` | ホストの名前と部分木の名前（ID から、identity）。書かなくてよい |
| `capabilities` | 機器の hello の NETCONF の capability（短い名前を空白で区切る。例 `base:1.0 base:1.1 candidate:1.0 confirmed-commit:1.1 validate:1.1 …`）。観測だけ（Computed） |

```yaml
resources:
  - provider: netconf
    id: sw1/if-uplink
    fields:
      config: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces" xmlns:ianaift="urn:ietf:params:xml:ns:yang:iana-if-type"><interface><name>uplink</name><description>to core</description><type>ianaift:ethernetCsmacd</type><enabled>true</enabled></interface></interfaces>'
  - provider: netconf
    id: sw1/acl-mgmt
    fields:
      config: '<acls xmlns="…">…</acls>'
      ordered: acls/acl/aces/ace
```

- 宣言は **filter が選ぶ部分木の全体**。機器の側にだけある葉（明示的に設定された値）もずれになる。既定値の扱いは機器の with-defaults の基本の振る舞いに従う（多くは明示した値だけを返す）。
- 読み込みの時点で、ID の形・ホストと部分木があること・`config` が XML として読めて、最上位の要素が filter の最上位の要素のどれかであること・`ordered` の書式を確かめる（エラーに `config` の中身は出さない）。
- 部分木が機器に無い（`<data>` が空）のは観測の事実（absent）。plan には `CREATE` として出るが、これも当てない。

## 比べ方（正規形）

書き方の違いはずれにしない。

| 揃えるもの | 決まり |
| --- | --- |
| 名前空間 | 接頭辞でなく名前空間の URI で比べる（`xmlns:if="…"` と `xmlns="…"` は同じ） |
| 値の中の接頭辞 | identityref などの `prefix:name` は、その接頭辞がその場で宣言されていれば URI に解決して比べる（機器は自分の接頭辞で返す）。宣言されていない接頭辞の値（時刻・IPv6 など）はそのまま |
| 空白 | 要素の間の空白は捨て、値の前後の空白を落とす。`<a/>` と `<a></a>` は同じ |
| 属性 | 名前空間の宣言は捨て、残りは並べ替えて比べる |
| 兄弟の順 | **既定では順を問わない**。`ordered` で名指しした道の要素だけ、同じ名前の兄弟の間で順を比べる（ordered-by user のリストなど） |
| コメント・処理命令 | 捨てる |

ずれていれば、plan と status に「道 = 値」の行で、機器にだけある行と宣言にだけある行（それぞれ頭から 3 つ）を出す。行が同じで順だけ違うときは `the order of an ordered list differs`。

## 観測できないとき

| 起きたこと | 答え |
| --- | --- |
| 機器に届かない・認証の失敗 | 観測できない（unknown、TransientError）。absent にしない |
| ホスト鍵の不一致 | 観測できない（unknown、PermanentError） |
| hello が合わない（NETCONF の名前空間でない・`session-id` が無い・base:1.0 も 1.1 も無い） | 観測できない（unknown） |
| 区切りが壊れた・返事が XML でない・途中で切れた | 観測できない（unknown） |
| `rpc-error`（error-severity が error。warning だけなら成功） | 観測できない（unknown）。セッションは捨てない |
| 使い回したセッションが切れていた（機器が idle で切った） | 1 度だけ開き直す（unknown にしない） |

## hello と区切り

- Sakutei は hello で base:1.0 と base:1.1 を出す。**両方が base:1.1 を出したときだけ**、hello の後を chunked で区切る（RFC 6242 §4.1）。base:1.0 だけの機器とは `]]>]]>` のまま。
- 機器が出した NETCONF の capability は観測の `capabilities` に残る。後で入れる 2 段の Apply の道（`:candidate`・`:confirmed-commit` の版・`:validate`）を選ぶ材料。
- 機器に送る rpc は `<get-config>`（`running`、subtree filter）と `<close-session>` だけ。provider には edit-config・commit を組み立てる道が無い。

## 後で入れる 2 段の Apply（方針、この版には無い）

設定を当てるときは、届かなくなる設定を当てても機器が自分で戻すように、commit confirmed を使う。

1. `:candidate` に `<edit-config>` で宣言を当てる（`:validate` があれば `<validate>`）。
2. `<commit><confirmed/><confirm-timeout>…</confirm-timeout></commit>`（commit confirmed）。
3. **新しい接続で**機器に届くこと・running が宣言どおりなことを確かめる。
4. 確かめられたら確定の `<commit>`。確かめられなければ確定しない（timeout で機器が戻す）。

確定の送り方は機器の `:confirmed-commit` の版で変わる。

- **`:confirmed-commit:1.0` だけの機器**: 確定は commit confirmed を送った**同じセッション**からしかできず、そのセッションが切れると取り消される。だから **同じセッションで commit confirmed → そのセッションを開いたまま新しい接続で確かめる → 同じセッションで確定** の順にする。
- `:confirmed-commit:1.1` の機器: `persist` を付ければセッションが切れても残り、別のセッションから `persist-id` で確定できる。

入れたら provider は `CommitConfirm` を宣言し、`ObserveOnly` を外す（§23.4）。それまでは型で Apply させない。

## 統合テスト

公開の NETCONF サーバ（netopeer2 のコンテナ）を相手に回す（Backlog #141）。build tag `integration_netconf` の付いたファイルにあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-netconf
```

要るもの: docker（パスワードなしの `sudo -n`）、`ssh`・`ssh-keygen`。イメージは `sysrepo/sysrepo-netopeer2:latest@sha256:36cc1d841c97…`（tag と digest で固定。`SAKUTEI_IT_NETOPEER2_IMAGE` で差し替えられる）。

- 機器を 1 台（`sakutei-ncit-sw1`）、127.0.0.1 の空いたポートに立てる。Sakutei は `netconf` ユーザで鍵で入る（テストが作った鍵を authorized_keys に置き、秘密鍵は file SecretBackend から）。ホスト鍵はコンテナの中から読んで known_hosts に書く（初回だけ信じることはしない）。
- 観測する部分木は ietf-netconf-acm の 2 つ: `sw1/acl-rules`（rule-list。rule は ordered-by user なので `ordered: nacm/rule-list/rule`）と `sw1/group`（group の user-name。宣言は機器と違う順で書く）。どの利用者も属さない group なのでアクセスの制御には効かない。
- 「手で変える」は、テストが ssh で直に開く**別の NETCONF セッション**の `<edit-config>`（Sakutei の外。base:1.0 だけを出すので `]]>]]>` の区切り）。netconf ユーザに書き込みを許す NACM の規則は、準備のときにコンテナの中の sysrepocfg で入れる。Sakutei 自身は base:1.1（chunked）で話す。
- 終われば（落ちても）コンテナを消す。Makefile も残ったコンテナを消す。

確かめること:

| | 内容 |
| --- | --- |
| 宣言どおり | status で 2 つとも conformant、plan は変更なし、reconcile を 3 回回しても何も出ない |
| 手で変える | rule の順を入れ替え、group に利用者を足す → status で 2 つとも drift、plan の 2 つの操作が `blocked: observe_only`（順のずれと、機器にだけある `user-name = carol` の説明つき）、reconcile を 3 回回しても毎回「never applied」で、機器は手で変えたまま |
| 手で戻す | 何周しても conformant（ずれが出ない） |
| 止める | `docker stop` → status で 2 つとも unobservable（unreachable）、drift は 0、reconcile は 0 以外で終わる（観測できないものがある） |

### 結果（2026-10-27）

開発機（docker 29.1.3・OpenSSH 9.6p1）で netopeer2-server 2.1.49 を相手に回した結果（PASS）:

```
--- PASS: TestNetconfIntegration (3.64s)
    --- PASS: TestNetconfIntegration/conformant (0.62s)
    --- PASS: TestNetconfIntegration/changed_by_hand:_drift,_never_applied (0.82s)
    --- PASS: TestNetconfIntegration/restored_by_hand:_conformant,_no_drift_in_any_cycle (0.41s)
    --- PASS: TestNetconfIntegration/stopped:_unknown (1.67s)
PASS
```

手で変えたあとの `sakutei plan`（抜粋）:

```
Blocked: 2 of 2 change(s) would not be applied by a reconcile now

Plan: 2 change(s)

~ UPDATE netconf//sw1/acl-rules
  reason: netconf sw1/acl-rules: the device's subtree differs from the declaration (0 line(s) only on the device, 0 only declared): the order of an ordered list differs
  cause:  B
  ~ config                "<nacm xmlns=…><rule-list>…<rule><name>r2</name>…</rule><rule><name>r1</name>…" → "…"
  blocked: observe_only — the provider observes only (Capabilities.ObserveOnly, §23.4) — reported as drift, never applied

~ UPDATE netconf//sw1/group
  reason: netconf sw1/group: the device's subtree differs from the declaration (1 line(s) only on the device, 0 only declared)
  device: nacm/groups/group/user-name = carol
  cause:  B
  blocked: observe_only — the provider observes only (Capabilities.ObserveOnly, §23.4) — reported as drift, never applied
```

`sakutei reconcile`:

```
Observe-only provider(s): 2 drifted resource(s) reported — never applied (§23.4)
  DRIFT EVENT: would be UPDATE netconf//sw1/acl-rules
  DRIFT EVENT: would be UPDATE netconf//sw1/group
Reconcile: no changes applied; 2 resource(s) skipped
sakutei: skip netconf//sw1/acl-rules: held: its provider observes only (§23.4)
sakutei: skip netconf//sw1/group: held: its provider observes only (§23.4)
```

止めたあとの `sakutei status`（抜粋）:

```
観測できない（drift とも適合とも数えない）: 2 リソース
  UNOBSERVABLE netconf//sw1/acl-rules
    理由: observe error: transient: netconf: Read "sw1/acl-rules": remote sw1: unreachable: ssh: connect to host 127.0.0.1 port 45825: Connection refused
```

## 通常のテスト

`make check` に入るもの（ネットワークにも docker にも出ない）:

- `internal/netconf`: 区切り（1.0・1.1、RFC 6242 の例、壊れた区切り）・正規形・ordered と、Go の中で動く偽の NETCONF サーバ（`internal/netconf/netconffake`。base:1.0 だけのものと 1.1 のもの、subtree filter を当てる、受けた rpc を残す）を相手にした観測・ずれ・absent・unknown・開き直し・読み込みの検査（[AC-141-1-*]・[AC-141-2-*]）。
- `internal/remote`: ssh のサブシステム（偽の ssh で、`-s` で開き双方向に流れる・多重化しない・sudo を付けない・鍵の一時ファイルが残らない・ssh の失敗が `*ConnError`、[AC-141-3-*]）。
- `tests/acceptance/netconf_test.go`: デーモンの周（ずれを毎周知らせ、Apply は 0、ほかの provider は同じ周で収束する、止めると unknown、GET /plan の `blocked_by: observe_only`）、ObserveOnly を型で止める（executor・起動時の検査）、CLI の plan・reconcile・status（偽の ssh の先でテストのバイナリが偽のサーバになる）、registry（[AC-141-4-*]）。
