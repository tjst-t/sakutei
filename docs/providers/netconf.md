# NETCONF の機器の設定を観測し、当てる provider（`netconf`）

NETCONF 対応のネットワーク機器の設定の**部分木**を観測し、宣言と比べてずれを知らせる provider（Backlog #141）。
**既定は観測だけ**で、設定を機器に当てることはしない。provider の設定で明示したとき（`netconf_apply: "true"`）だけ、commit confirmed を使う **2 段の Apply** で宣言を当てる（下の「2 段の Apply」、Backlog #175、設計書 §23.4「CommitConfirm が無ければ Apply 禁止」）。
決まりの正は設計書 §29.1「NETCONF の機器の設定の部分木の観測（観測だけ）」「NETCONF の 2 段の Apply（commit confirmed）」と §5.3・§23.4。ここは使い方の説明で、矛盾したら spec/ が優先する。

- 通信は NETCONF over SSH（RFC 6241・6242）。ssh 越しの実行の層（[docs/remote.md](../remote.md)、#130）の ssh で **netconf サブシステム**（`ssh -s … netconf`）を開く。機器に agent は置かない。メッセージの区切り（1.0 の `]]>]]>`、1.1 の chunked）と XML は Sakutei の中（Go の標準ライブラリ）で扱う。
- **既定は観測だけ**（`Capabilities.ObserveOnly`）。差分は知らせ（`DriftWouldApply`）と plan の `blocked: observe_only` になるだけで、コアはどの経路でも Apply を呼ばない（remediation mode が auto でも同じ）。`netconf_apply: "true"` のときだけ `CommitConfirm` を宣言して `ObserveOnly` を外し、ずれをふつうの Apply（予算・承認ゲート・remediation mode を通る）として 2 段で当てる。どちらでも List・BulkObserve は持たない（prune しない、宣言外の設定は消さない）。
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
    # 2 段の Apply を許すときだけ（書かなければ観測だけ）
    netconf_apply: "true"
    netconf_confirm_timeout: 2m         # commit confirmed の confirm-timeout（既定 2m、下限 2s、秒に切り上げる）
    netconf_commit_flow: "1.0"          # "1.0"（既定）か "1.1"（:confirmed-commit:1.1 を持つ機器で persist の流れ）
```

- `netconf_apply` は `"true"` か `"false"`（書かないのと同じ）。ほかの値はエラー。`netconf_apply` を `"true"` にしないまま `netconf_confirm_timeout`・`netconf_commit_flow` を書くのもエラー（Apply しないのに Apply の設定があるのは書き間違い）。

- `filter` か `filter_file` のどちらか 1 つ。中身は `<filter type="subtree">` の子にそのまま入る（RFC 6241 §6）。最上位の要素はどれも**名前空間を持つこと**（名前空間の無い subtree filter はすべての名前空間に当たるので断る）。
- 「どこ」を intent でなく provider の側に書くのは、観測（Read）が宣言を見ない決まりだから（ADR-0002）。どこを読むかは観測が知り、何であるべきかは宣言が持つ。
- 部分木の名前は英小文字・数字・`_`・`.`・`-`。未知のキーはエラー。
- `remote_config` の `sudo` はサブシステムには付かない。接続は ssh の多重化（`control_persist`）を使わず直につなぐ（多重化を受けない機器の ssh がある）。代わりに Sakutei が NETCONF のセッションをホストごとに 1 本使い回す。

## ID とフィールド

ID は `<ホスト名>/<部分木の名前>`（例 `sw1/if-uplink`）。

| フィールド | 中身 |
| --- | --- |
| `config` | 部分木の中身の XML（**宣言、必須**）。観測の値は機器の running の正規形（下）で、機器の順のまま |
| `ordered` | 順に意味のある列の道（`nacm/rule-list/rule` のように、部分木の頭からの要素の名前を `/` でつなぐ。カンマで複数）。比べ方の宣言で、機器には無い（それだけでは差分にならない）。Apply を許していて `config` が違うときは、Apply が順を並べ直して確かめるのに使うので plan に `+ ordered` と出る |
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
- 宣言に edit-config の属性（`nc:operation`・`yang:insert`）は書けない（どう当てるかは provider が決める）。
- 部分木が機器に無い（`<data>` が空）のは観測の事実（absent）。plan には `CREATE` として出る。観測だけなら当てない。Apply を許していれば、2 段の Apply で作る。

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
- 観測で機器に送る rpc は `<get-config>`（`running`、subtree filter）と `<close-session>` だけ。2 段の Apply を許したときだけ、Apply のセッションで `<lock>`・`<edit-config>`・`<validate>`・`<commit>`・`<discard-changes>`・`<unlock>` を送る。

## 2 段の Apply（`netconf_apply: "true"` のときだけ）

届かなくなる設定を当てても機器が自分で戻すように、commit confirmed を使う。流れは **`:confirmed-commit:1.0` だけの機器**を主にしている（確定は commit confirmed を送った**同じセッション**からしかできず、そのセッションが切れると取り消される）。1.1 の機器でも既定はこの流れで動く。

1. Apply 用の**新しいセッション**を開き、機器の hello に `:candidate` と `:confirmed-commit`（1.0 か 1.1）があるかを見る。**どちらかが無い機器には何も送らない**（その機器は観測だけのまま。Apply は失敗〈permanent〉として知らせる）。
2. candidate を `<lock>` し、running の部分木を読み、宣言を**部分木の置き換え**（下）として candidate に `<edit-config>`。`:validate` があれば `<validate>`。
3. **同じセッションで** `<commit><confirmed/><confirm-timeout>…</confirm-timeout></commit>`。
4. **新しい ssh 接続で**（観測のセッションとも Apply のセッションとも別）機器に届くこと・running の部分木が宣言どおり（観測と同じ比べ方、`ordered` も）なことを確かめる。届かない・違う間は間隔をおいて読み直し、**confirm-timeout の半分で打ち切る**。
5. 確かめられたら**同じセッションで**確定の `<commit>`、`<unlock>`、`<close-session>`。

うまくいかないとき:

| 起きたこと | 答え |
| --- | --- |
| 確かめられなかった（届かない・宣言どおりでない） | **確定しない**。candidate を `<discard-changes>` し、Apply のセッションを開いたまま confirm-timeout が過ぎるのを待って**機器の自動の巻き戻しに任せる**（閉じる要求が届く保証は無いので、巻き戻しは機器の時計に頼る形にそろえる）。過ぎたら新しい接続で元に戻ったかを見て、失敗（permanent）として知らせる。同じ宣言は変わるまで当て直さない（届かなくなる設定を繰り返さない） |
| commit confirmed・確定の commit の応答の前に切れた | 確定の成否が分からない（unknown）。失敗（transient）として次の周の観測に任せる。1.0 の機器はセッションの終わりで、そうでなくても confirm-timeout で戻す |
| lock が取れない（ほかのセッションが使っている） | transient（次の周にやり直す） |
| 機器が `<edit-config>`・`<validate>` を受けない（rpc-error） | candidate を discard して permanent（宣言を直すまで当てない） |
| 部分木を消す（Delete） | しない（prune も `ensure: absent` も無い） |

- 1 台には 1 度に 1 つの Apply（candidate は機器に 1 つ）。部分木ごとに 2 段で当てる。
- **1.1 の persist の流れ**（`netconf_commit_flow: "1.1"`、機器が `:confirmed-commit:1.1` を持つときだけ）: commit confirmed に `<persist>` を付け、確定は `<persist-id>` で送る。Apply のセッションが確定の前に切れても、新しいセッションから確定する。1.1 を持たない機器では 1.0 の流れ。
- 予算（§13）・承認ゲート・remediation mode（alert-only なら当てない）は他の provider の Apply と同じに通る。

### 部分木の置き換え

YANG を持たないので、どこで置き換えるかは filter の形で決める（RFC 6241 §6）。部分木の外（同じ親の下のほかの要素）には触れない。

- 子が content match（値つきの葉）だけの filter の要素・子の無い filter の要素（selection）を**境目**にする。宣言のその要素を `nc:operation="replace"` で送り、機器にだけあるその要素を `nc:operation="remove"` で消す（突き合わせは content match の値、無ければ機器の要素の最初の葉〈YANG のリストのキーは最初に書かれる〉、葉が無ければ名前）。
- それ以外の filter の要素（containment）は中に入る（content match の葉はどのエントリかを示すために写す。operation は既定の merge）。
- `ordered` の道の境目の要素には `yang:insert="last"` も付け、宣言の順に並べ直す。
- filter が選ばない宣言の要素があれば当てない（送れば filter の外を書く。permanent）。

例: filter `<nacm xmlns="…"><rule-list><name>sakutei-it</name></rule-list></nacm>` なら、rule-list `sakutei-it` を丸ごと置き換える（ほかの rule-list には触れない）。

## 統合テスト

公開の NETCONF サーバ（netopeer2 のコンテナ）を相手に回す（Backlog #141・#175）。build tag `integration_netconf` の付いたファイルにあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-netconf
```

要るもの: docker（パスワードなしの `sudo -n`）、`ssh`・`ssh-keygen`。イメージは `sysrepo/sysrepo-netopeer2:latest@sha256:36cc1d841c97…`（tag と digest で固定。`SAKUTEI_IT_NETOPEER2_IMAGE` で差し替えられる）。

- 機器を 1 台（`sakutei-ncit-sw1`）、127.0.0.1 の空いたポートに立てる。Sakutei は `netconf` ユーザで鍵で入る（テストが作った鍵を authorized_keys に置き、秘密鍵は file SecretBackend から）。ホスト鍵はコンテナの中から読んで known_hosts に書く（初回だけ信じることはしない）。
- 観測する部分木は ietf-netconf-acm の 2 つ: `sw1/acl-rules`（rule-list。rule は ordered-by user なので `ordered: nacm/rule-list/rule`）と `sw1/group`（group の user-name。宣言は機器と違う順で書く）。どの利用者も属さない group なのでアクセスの制御には効かない。
- 「手で変える」は、テストが ssh で直に開く**別の NETCONF セッション**の `<edit-config>`（Sakutei の外。base:1.0 だけを出すので `]]>]]>` の区切り）。netconf ユーザに書き込みを許す NACM の規則（group `sakutei-it-admin` と rule-list `sakutei-it-admin` の `permit-all`）は、準備のときにコンテナの中の sysrepocfg で入れる。Sakutei 自身は base:1.1（chunked）で話す。
- 2 段の Apply は、`netconf_apply: "true"`・`netconf_confirm_timeout: 10s` の provider の設定で回す（1.0 の流れと、`netconf_commit_flow: "1.1"` の persist の流れ）。netopeer2 の hello は `:candidate`・`:confirmed-commit:1.1`・`:validate:1.1` を出す（1.0 の流れは 1.1 の機器でも同じに動く）。
- 確かめの失敗は「Sakutei が自分を締め出す宣言」で起こす: 部分木 `sw1/admin-rules`（rule-list `sakutei-it-admin`）の頭に、ietf-netconf-acm を読むのを断る規則 `deny-read-nacm` を足す。当てると、新しい接続からは NACM の部分木が読めなくなる（`<data/>` が返り、宣言どおりと確かめられない）。書き込みは許したままにする。
- 終われば（落ちても）コンテナを消す。Makefile も残ったコンテナを消す。

確かめること:

| | 内容 |
| --- | --- |
| 宣言どおり | status で 2 つとも conformant、plan は変更なし、reconcile を 3 回回しても何も出ない |
| 手で変える | rule の順を入れ替え、group に利用者を足す → status で 2 つとも drift、plan の 2 つの操作が `blocked: observe_only`（順のずれと、機器にだけある `user-name = carol` の説明つき）、reconcile を 3 回回しても毎回「never applied」で、機器は手で変えたまま |
| 手で戻す | 何周しても conformant（ずれが出ない） |
| 2 段の Apply（1.0） | 手で変えた 2 つ（rule の順・`carol`）を、Apply を許した設定の reconcile が当てる → status で 2 つとも conformant、機器は宣言どおり（r1 が r2 の前、`carol` が無い）。confirm-timeout（10 秒）を過ぎても宣言どおりのまま（確定された）。もう一度の reconcile は `Converged` |
| 2 段の Apply（1.1 の persist） | 同じく手で変えたものを persist の流れで当てる → conformant、confirm-timeout の後も宣言どおり |
| 確かめの失敗 | `sw1/admin-rules` に `deny-read-nacm` を足す宣言の reconcile → 新しい接続で宣言どおりと確かめられず（confirm-timeout の半分の 5 秒で打ち切る）、確定の commit を送らず、confirm-timeout まで待つ（reconcile は 10 秒以上かかる）。機器が巻き戻し、`deny-read-nacm` は無く NACM がまた読める。Apply は失敗として `not confirmed … the confirming commit was not sent; the device rolled back at the timeout` を知らせる |
| 止める | `docker stop` → status で 2 つとも unobservable（unreachable）、drift は 0、reconcile は 0 以外で終わる（観測できないものがある） |

netopeer2 2.1.49 で見たこと（試験の組み立ての理由）: netconf ユーザを NACM の group から外す宣言で締め出すと、confirm-timeout の巻き戻しで running は元に戻るのに、そのあとも netconf ユーザの読み書きが断られ続けた（サーバを立て直すまで）。このため確かめの失敗は「読むのだけを断る規則を足す」宣言で起こしている。また ietf-netconf-server の `local-address` を変えても待ち受けは変わらず、締め出しにならなかった。

### 結果（2026-10-28）

開発機（docker 29.1.3・OpenSSH 9.6p1）で netopeer2-server 2.1.49 を相手に回した結果（PASS）:

```
--- PASS: TestNetconfIntegration (40.63s)
    --- PASS: TestNetconfIntegration/conformant (0.67s)
    --- PASS: TestNetconfIntegration/changed_by_hand:_drift,_never_applied (0.84s)
    --- PASS: TestNetconfIntegration/restored_by_hand:_conformant,_no_drift_in_any_cycle (0.42s)
    --- PASS: TestNetconfIntegration/apply_1.0:_commit_confirmed,_checked,_confirmed (13.07s)
    --- PASS: TestNetconfIntegration/apply_1.1:_persist (12.84s)
    --- PASS: TestNetconfIntegration/apply:_the_check_fails,_the_device_rolls_back_at_the_timeout (10.49s)
    --- PASS: TestNetconfIntegration/stopped:_unknown (2.21s)
PASS
```

（1.0・1.1 の段の時間の大半は、confirm-timeout を過ぎても宣言どおりかを見るための待ち。）

Apply を許した設定の `sakutei reconcile`（1.0 の流れ、抜粋）:

```
~ UPDATE netconf//sw1/acl-rules
  reason: netconf sw1/acl-rules: the device's subtree differs from the declaration (0 line(s) only on the device, 0 only declared): the order of an ordered list differs
  cause:  B
  ~ config                "<nacm xmlns=…><rule-list>…<rule><name>r2</name>…" → "…"
  + ordered               (new) → "nacm/rule-list/rule"

~ UPDATE netconf//sw1/group
  reason: netconf sw1/group: the device's subtree differs from the declaration (1 line(s) only on the device, 0 only declared)
  device: nacm/groups/group/user-name = carol
  cause:  B

Reconcile: 2 change(s) applied
```

確かめの失敗の `sakutei reconcile`（10 秒）:

```
Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)
sakutei: apply netconf//sw1/admin-rules: permanent: netconf: Apply "sw1/admin-rules": not confirmed: the running subtree is not as declared within 5s (half of the confirm-timeout 10s); the confirming commit was not sent; the device rolled back at the timeout (observed over a new connection)
```

一回性の `reconcile` の終了コードは、Apply の失敗では変わらない（0。`docs/exit-codes.md`）。失敗は標準エラーと status に出る。
手で変えたあとの観測だけの設定での `sakutei plan`（抜粋）:

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
- `internal/netconf`（2 段の Apply、#175）: 部分木の置き換えの edit-config の中身（[AC-175-1-*]）と、偽のサーバの candidate・lock・edit-config・validate・commit confirmed 1.0／1.1（persist・persist-id）・confirm-timeout とセッションの終わりの巻き戻しを相手にした、1.0 の流れの確定（同じセッションの 2 つの commit の間に新しい接続で確かめる）・確かめの失敗の時間切れの巻き戻し・半分での打ち切り・成否不明・persist・機器が持たないとき・lock の取り合い・機器が受けない宣言（[AC-175-2-*]）。
- `internal/remote`: ssh のサブシステム（偽の ssh で、`-s` で開き双方向に流れる・多重化しない・sudo を付けない・鍵の一時ファイルが残らない・ssh の失敗が `*ConnError`、[AC-141-3-*]）。
- `tests/acceptance/netconf_apply_test.go`: Apply を許したデーモンの周（ずれを 2 段で直し、次の周は適合・確かめの失敗は確定せず時間切れで戻り、同じ宣言を当て直さない・alert-only の周は当てない）と registry（`netconf_apply` の有無と誤った設定、[AC-175-3-*]）。
- `tests/acceptance/netconf_test.go`: デーモンの周（ずれを毎周知らせ、Apply は 0、ほかの provider は同じ周で収束する、止めると unknown、GET /plan の `blocked_by: observe_only`）、ObserveOnly を型で止める（executor・起動時の検査）、CLI の plan・reconcile・status（偽の ssh の先でテストのバイナリが偽のサーバになる）、registry（[AC-141-4-*]）。
