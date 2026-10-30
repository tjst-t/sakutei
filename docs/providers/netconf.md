# NETCONF の機器の設定を観測し、当てる provider（`netconf`）

NETCONF 対応のネットワーク機器の設定の**部分木**を観測し、宣言と比べてずれを知らせる provider（Backlog #141）。
**既定は観測だけ**で、設定を機器に当てることはしない。provider の設定で明示したとき（`netconf_apply: "true"`）だけ、commit confirmed を使う **2 段の Apply** で宣言を当てる（下の「2 段の Apply」、Backlog #175、設計書 §23.4「CommitConfirm が無ければ Apply 禁止」）。
決まりの正は設計書 §29.1「NETCONF の機器の設定の部分木の観測（観測だけ）」「NETCONF の 2 段の Apply（commit confirmed）」と §5.3・§23.4。ここは使い方の説明で、矛盾したら spec/ が優先する。

- 通信は NETCONF over SSH（RFC 6241・6242）。ssh 越しの実行の層（[docs/remote.md](../remote.md)、#130）の ssh で **netconf サブシステム**（`ssh -s … netconf`）を開く。機器に agent は置かない。メッセージの区切り（1.0 の `]]>]]>`、1.1 の chunked）と XML は Sakutei の中（Go の標準ライブラリ）で扱う。
- **既定は観測だけ**（`Capabilities.ObserveOnly`）。差分は知らせ（`DriftWouldApply`）と plan の `blocked: observe_only` になるだけで、コアはどの経路でも Apply を呼ばない（remediation mode が auto でも同じ）。`netconf_apply: "true"` のときだけ `CommitConfirm` を宣言して `ObserveOnly` を外し、ずれをふつうの Apply（予算・承認ゲート・remediation mode を通る）として 2 段で当てる。どちらでも List・BulkObserve は持たない（prune しない、宣言外の設定は消さない）。
- 機種固有の設定の型（YANG）は持たない。利用者が機器の YANG に沿った XML を書く。この文書に機種名は出さない。
- 機器の側（NACM、RFC 8341）で**宣言した部分木だけを読み書きできるよう絞ったユーザー**で動く（下の「部分木だけに絞ったユーザー」、Backlog #176）。観測も Apply も部分木の外を読まない・書かない。

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
    netconf_hosts: sw1,sw2              # 観測する機器（intent の providers.netconf.hosts に書かなければ必須）
    netconf_subtrees: subtrees.yaml     # 部分木の一覧（intent の providers.netconf.subtrees に書かなければ必須）
    # 2 段の Apply を許すときだけ（書かなければ観測だけ）
    netconf_apply: "true"
    netconf_confirm_timeout: 2m         # commit confirmed の confirm-timeout（既定 2m、下限 2s、秒に切り上げる）
    netconf_confirm_timeouts: sw2=5m    # 台ごとの confirm-timeout（無い台は上の値。commit_group の上限は一番短いもの、#199）
    netconf_commit_flow: "1.0"          # "1.0"（既定）か "1.1"（:confirmed-commit:1.1 を持つ機器で persist の流れ）
```

- `netconf_apply` は `"true"` か `"false"`（書かないのと同じ）。ほかの値はエラー。`netconf_apply` を `"true"` にしないまま `netconf_confirm_timeout`・`netconf_confirm_timeouts`・`netconf_commit_flow` を書くのもエラー（Apply しないのに Apply の設定があるのは書き間違い）。

- `filter` か `filter_file` のどちらか 1 つ。中身は `<filter type="subtree">` の子にそのまま入る（RFC 6241 §6）。最上位の要素はどれも**名前空間を持つこと**（名前空間の無い subtree filter はすべての名前空間に当たるので断る）。
- 「どこ」は観測（Read）が知るべきこと（Read は宣言の `config` を見ない、ADR-0002）なので、provider を組み立てるときに渡す。部分木の一覧のファイルの代わりに intent の `providers.netconf.subtrees` にも書ける（下の「範囲を intent に書く」、#182）。provider はそれを組み立ての前に受け取り、Read が resources の宣言を見ないことは変わらない。
- 部分木の名前は英小文字・数字・`_`・`.`・`-`。未知のキーはエラー。
- `remote_config` の `sudo` はサブシステムには付かない。接続は ssh の多重化（`control_persist`）を使わず直につなぐ（多重化を受けない機器の ssh がある）。代わりに Sakutei が NETCONF のセッションをホストごとに 1 本使い回す。

## 範囲を intent に書く（`providers:`、Backlog #182）

観測・Apply する機器（`netconf_hosts`）と部分木の「どこ」（`netconf_subtrees` の filter）は管理の範囲なので、intent のトップレベルの
`providers:` に書ける（§1.7・§7.7）。`remote_config` と 2 段の Apply の設定はつなぎ方として provider の設定に残る。

```yaml
# intent/providers.yaml
providers:
  netconf:
    hosts: [sw1, sw2]                     # netconf_hosts と同じ（順は問わない）
    subtrees:
      if-uplink:                          # 部分木の一覧の subtrees.if-uplink と同じ
        filter: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"><interface><name>uplink</name></interface></interfaces>'
```

- intent の部分木は `filter` だけ（`filter_file` は部分木の一覧のファイルにだけ書ける）。名前と filter の決まりは上と同じ。
- 部分木は名前ごとに突き合わせる。filter は**正規形**（名前空間は接頭辞でなく URI、空白と兄弟の順を問わない）で比べるので、
  一覧のファイルから写した filter は書き方が違っても同じと見る。intent にだけある部分木は一覧のファイルの部分木に足される。
  部分木が intent にも一覧にも無ければ組み立てで断る。
- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.netconf.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## ID とフィールド

ID は `<ホスト名>/<部分木の名前>`（例 `sw1/if-uplink`）。

| フィールド | 中身 |
| --- | --- |
| `config` | 部分木の中身の XML（**宣言、必須**）。観測の値は機器の running の正規形（下）で、機器の順のまま |
| `ordered` | 順に意味のある列の道（`nacm/rule-list/rule` のように、部分木の頭からの要素の名前を `/` でつなぐ。カンマで複数）。比べ方の宣言で、機器には無い（それだけでは差分にならない）。Apply を許していて `config` が違うときは、Apply が順を並べ直して確かめるのに使うので plan に `+ ordered` と出る |
| `commit_group` | まとめて確定する group の名前（英小文字・数字・`_`・`.`・`-`。下の「commit_group」、#199）。どう確定するかの宣言で、機器には無い（差分にならない）。書かなければ台ごとに確定する |
| `confirm` | 確かめる先の一覧（`self`・`observe:…`・`check:…` を「, 」で区切る。下の「confirm の先」、#200）。どう確かめるかの宣言で、機器には無い（それだけでは差分にならない）。書かなければ `self` だけ |
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
- **知らない欄は読み込みで断る**（Backlog #211）。`fields` に書けるのは上の表の `config`・`ordered`・`commit_group`・`confirm`・`host`・`subtree` だけ。それ以外の欄（宣言を組み立てる道具が足した `name` など）は、`validate`（provider の `ValidateIntent`）・`plan`・`reconcile`・常駐の読み込みで、欄の名前とリソースの ID を挙げて断る（例 `netconf "sw1/if-uplink": unknown field(s) ["name"] (remove them from the fields; …)`）。直し方はその欄を宣言から外すこと。観測だけの `capabilities` を宣言に書いたときも同じく断る。以前は知らない欄が drift になり、Apply が「the operation carries no config」の permanent で断っていて原因が分かりにくかった。機器には何も送らない。
- 宣言に edit-config の属性（`nc:operation`・`yang:insert`）は書けない（どう当てるかは provider が決める）。
- 部分木が機器に無い（`<data>` が空）のは観測の事実（absent）。plan には `CREATE` として出る。観測だけなら当てない。Apply を許していれば、2 段の Apply で作る。
- **中身の無い要素だけの `<data>` も absent**（Backlog #208）。部分木が無いとき、名前空間の無い空の `<configuration/>`（rpc-reply の base:1.0 の名前空間を受け継ぐ）を返す機器がある。子も値も無い最上位の要素は、名前空間の有無も属性も問わず「無い」と見る。同じ理由で、中身の無い要素を `config` に宣言するのは読み込みで断る（宣言しても観測はいつも absent になり、収束しない）。

## 比べ方（正規形）

書き方の違いはずれにしない。

| 揃えるもの | 決まり |
| --- | --- |
| 名前空間 | 接頭辞でなく名前空間の URI で比べる（`xmlns:if="…"` と `xmlns="…"` は同じ） |
| 値の中の接頭辞 | identityref などの `prefix:name` は、その接頭辞がその場で宣言されていれば URI に解決して比べる（機器は自分の接頭辞で返す）。宣言されていない接頭辞の値（時刻・IPv6 など）はそのまま |
| 空白 | 要素の間の空白は捨て、値の前後の空白を落とす。`<a/>` と `<a></a>` は同じ |
| 属性 | 名前空間の宣言は捨て、残りは並べ替えて比べる。**比べるのは宣言に書いた属性だけ**（Backlog #208）：機器の側の属性は、宣言の同じ道（`ordered` と同じ、部分木の頭からの要素の名前を `/` でつないだもの）の要素に同じ名前（名前空間と名前）の属性があるときだけ比べる。機器が付ける印（最上位の要素の commit の時刻・利用者など、読むたびに値が変わるもの）は drift にしない。宣言に書いた属性が機器に無い・値が違うのは drift |
| 兄弟の順 | **既定では順を問わない**。`ordered` で名指しした道の要素だけ、同じ名前の兄弟の間で順を比べる（ordered-by user のリストなど） |
| コメント・処理命令 | 捨てる |

ずれていれば、plan と status に「道 = 値」の行で、機器にだけある行と宣言にだけある行（それぞれ頭から 3 つ）を出す。行が同じで宣言に書いた属性だけが違うときは、「道 @属性 = 値」の行で、どの要素のどの属性が違うかを出す（名前空間つきの属性は `{URI}名前`）:

```
netconf sw1/if-uplink: the attributes of the device's subtree differ from the declaration (1 attribute(s) only on the device, 1 only declared; only the attributes written in the declaration are compared)
  device: interfaces/interface @inactive = no
  declared: interfaces/interface @inactive = inactive
```

行も属性も同じで順だけ違うときは `the order of an ordered list differs`。

2 段の Apply の確かめ（新しい接続で読み直して宣言どおりかを見る）も同じ比べ方をする。読むたびに属性の値が変わる機器でも、中身が宣言どおりなら確定まで通る。

## 機器の側の事実（注意）

機器の NETCONF サーバによって、次のことがある。

- **文字列の非 ASCII が崩れて返るサーバがある**：設定した値の非 ASCII の文字が、文字参照の形（`&#…;` を文字として書いたもの）などに変わって get-config に返り、宣言と合わなくなる（いつまでも drift で、2 段の Apply は確かめに通らない）。`config` の値（description など）は ASCII に限る運用を勧める。
- **NACM を持たないサーバでは、書き込み用の利用者を部分木だけに絞り切れない**：機器の側で部分木の外の書き込みを断れないので、その利用者の資格は機器の設定の全体を書ける。そのため、Sakutei が filter の選ぶ部分木だけを置き換える（外には触れない、上の「部分木の置き換え」）ことが守りの一つになる。部分木の一覧の filter は狭く書き、書き込み用の資格は観測用と分ける（下の「観測用と書き込み用の資格」）。
- **filter の最上位の要素に、サーバ固有の名前空間が要るサーバがある**：標準の YANG の名前空間でなく、サーバの設定の木の根（例：`<configuration xmlns="…">`）を、そのサーバの名前空間で書かないと何も選ばないサーバがある。部分木の一覧の filter と `config` の最上位の要素は、機器が get-config で返す名前空間に合わせて書く。

## 観測できないとき

| 起きたこと | 答え |
| --- | --- |
| 機器に届かない・認証の失敗 | 観測できない（unknown、TransientError）。absent にしない |
| ホスト鍵の不一致 | 観測できない（unknown、PermanentError） |
| hello が合わない（NETCONF の名前空間でない・`session-id` が無い・base:1.0 も 1.1 も無い） | 観測できない（unknown） |
| 区切りが壊れた・返事が XML でない・途中で切れた | 観測できない（unknown） |
| `rpc-error`（error-severity が error。warning だけなら成功） | 観測できない（unknown、TransientError）。セッションは捨てない |
| `rpc-error` の `access-denied`（ユーザーに get-config の実行か部分木の読みが許されていない） | 観測できない（unknown、**PermanentError**。やり直しても変わらない）。理由に error-path と「get-config の実行と部分木の読みを許す」を書く |
| 使い回したセッションが切れていた（機器が idle で切った） | 1 度だけ開き直す（unknown にしない） |

## hello と区切り

- Sakutei は hello で base:1.0 と base:1.1 を出す。**両方が base:1.1 を出したときだけ**、hello の後を chunked で区切る（RFC 6242 §4.1）。base:1.0 だけの機器とは `]]>]]>` のまま。
- 機器が出した NETCONF の capability は観測の `capabilities` に残る。後で入れる 2 段の Apply の道（`:candidate`・`:confirmed-commit` の版・`:validate`）を選ぶ材料。
- 機器について知ることは hello の capability だけから読む（`<get>` で netconf-state・yang-library を読みに行かない）。
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
| どの段でも `access-denied`（部分木の外を変える宣言・許されていない rpc） | 何も確定しない（edit-config の後なら candidate を discard）。permanent で、error-path と要る権限を理由に書く（下の「部分木だけに絞ったユーザー」） |
| `<discard-changes>` が失敗した | 「捨てた」とは言わず、candidate に変更が残っているかもしれない（次の lock が断られうる）と書く |
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

## commit_group（複数の台の変更をまとめて確定する、Backlog #199）

同じ変更を複数の台に入れる構成（例：同じセグメントを全部の leaf に足す）で、台ごとに確定すると、確かめが一部の台で落ちたときに変更が片方の台にだけ残る。`commit_group` に同じ名前を書いた部分木は、**全部の台で確かめが通ったときだけ確定し、1 台でも落ちればどの台も確定しない**。書かなければ今どおり台ごとに確定する（暗黙にはまとめない）。

```yaml
resources:
  - provider: netconf
    id: leaf1/vlan-100
    fields: {config: '<vlans xmlns="…">…</vlans>', commit_group: vlan-100}
  - provider: netconf
    id: leaf2/vlan-100
    fields: {config: '<vlans xmlns="…">…</vlans>', commit_group: vlan-100}
```

外の realizer（`pkg/cli.WithRealizer`、§7.6）が返すリソースの `Fields` に `commit_group` を入れても同じ。

読み込みの時点で断るもの:

- group の中の依存の辺（`depends_on` が直接でも間接でも group の別の member に届く）。まとめて確定するものの間に順序は付けられない。
- scope をまたぐ group、`update_policy.strategy: task` の member。
- 1 台に 2 つの member（1 台の candidate は 1 つのセッションしか持てない）。

収束ループでの扱い:

- member は**同じ波に並ぶ**（各 member が全 member の依存先を待つ）。差分のある member はまとめて **1 回の Apply** で渡る（差分の無い member は送らない）。
- **1 つでも行けない member**（観測できない・止めている・上流が落ちた・backoff・in-flight・予算で止まった・承認待ち）があれば、**どの member も行かない**（`skip …: commit_group "…": held with … (…)`）。承認ゲートは group 単位で、一部だけの承認はさせない。plan は操作ごとに `commit_group` を出し、止まった member のある group の残りを `blocked_by: commit_group` にする。
- 予算は今どおり台ごとの変更として数える。
- 早期収束パスと一回性の reconcile の観測し直しの回（#177）は group の member を当てない（group の全部を見る周・次の run に任せる）。

流れ（2 段の Apply を台ごとでなく group で回す）:

1. group の全部の台で、それぞれの Apply のセッションを開き、candidate に部分木を当てる（上の 1・2）。**1 台でも送れなければ、どの台にも commit confirmed を送らない**（candidate を捨てる）。
2. 全部の台に、それぞれのセッションで commit confirmed（並べて送る）。
3. 全部の台で、新しい ssh 接続での到達と宣言どおりかを確かめる（並べて）。全部の台で通ったら、全部の台の `confirm` の `observe:`・`check:` を待つ（並べて。下の「confirm の先」、#200）。
4. **全部が通ったときだけ**、各台に確定の `<commit>`（並べて送る）。

**時間の上限は group の中で一番短い confirm-timeout を基準にする**（台ごとの値は `netconf_confirm_timeouts`）。最初の commit confirmed から、その半分までに 4 に入れなければ確定しない。

1 台でも**送れない・確かめに落ちる・時間の上限の半分に届く**、のどれかなら、どの台も確定しない: candidate を `<discard-changes>` し、**`:confirmed-commit:1.1` を持つ台には `<cancel-commit>` も送ってすぐ戻す**。ほかの台は機器の confirm-timeout の巻き戻しに任せ、その時間までセッションを開いたまま待ち、新しい接続で戻ったかを見る。落ちた台は permanent（同じ宣言は変わるまで当て直さない）、ほかの台は transient（理由に落ちた台の名前）。

結果は台ごとに Event `CommitGroupApplied`（attrs の `group` と `outcome`: `committed`・`not_committed`・`unknown`）で出す。確定しなかった台・分からない台は、その台の Apply の失敗（`ApplyFailed`）にもなる。

### 残る窓（全部か何もしないかにならない場合）

NETCONF には複数の機器をまたぐ確定が無い。4 の確定は台ごとの rpc なので、4 に入ってからは次のことが起こりうる。どちらの場合も、**ほかの台は確定している**。

| 起きたこと | その台 | 答え |
| --- | --- | --- |
| 確定の `<commit>` を送っている途中で切れた（応答が来ない） | **unknown**（確定していれば残り、届いていなければ confirm-timeout で戻る） | 成否が分からないので、次の周の観測に任せる。宣言どおりと見えれば適合、戻っていれば次の周にもう一度 group で当てる |
| 機器が確定の `<commit>` を断った（rpc-error） | not_committed（confirm-timeout で戻る） | 次の周に group で当て直す（確定した台は差分が無いので送らない） |

窓は 4 の rpc を送っている間（並べて送るので、台ごとの 1 往復ほど）に限られる。窓に落ちた group は、台ごとの Event で、どの台が確定し、どの台が分からないかを示す。

## confirm の先：別のリソースの観測（`observe:`）と確かめの関数（`check:`）（Backlog #200）

ある台への変更が効いたかは、相手の台から見ないと分からないことがある（例：相手の台で経路が見えること）。欄 `confirm` に、変えたあと（commit confirmed のあと、確定の前）に確かめる先を「, 」で区切って書く。書かなければ `self` だけ（今までどおり）。`observe:`・`check:` の読み方・繰り返し・時間の上限・知らせ方は nftables の base chain の `confirm`（[nftables.md](nftables.md)、#214）と共通の口（`internal/confirmafter`）。

```yaml
resources:
  - provider: netconf
    id: sw1/if-uplink
    fields:
      config: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">…</interfaces>'
      confirm: 'self, observe:netconf//sw2/if-peer expect:{"config":"<interfaces xmlns=\"urn:ietf:params:xml:ns:yang:ietf-interfaces\">…</interfaces>"}, check:route-seen'
  - provider: netconf
    id: sw2/if-peer          # observe: で読むリソースは intent に宣言する
    fields: {config: '…'}
```

| 先 | 確かめること |
| --- | --- |
| `self` | 機器そのもの（新しい ssh 接続で届くこと・部分木が宣言どおりなこと）。**`self` の無い `confirm` は読み込みで断る**（到達の先が無い。`after:self` も断る） |
| `observe:<provider>/<scope>/<id> [expect:{…}]` | 別のリソースを観測し直し、`expect:`（JSON の object、欄 → 値）に書いた**欄の部分集合**が期待どおりになるまで繰り返し読む。`expect:` を書かなければ、観測できて在ることだけ。ID は `<provider>/<scope>/<id>`（scope が無ければ `netconf//sw2/if-peer`。scope は `site=a,rack=1` の形）。ID の中に空白は書けない |
| `check:<name>` | 名前で登録した確かめの関数を、nil を返すまで繰り返し呼ぶ。何をどう読んで判定するかは関数を渡す側が持つ |

- **`observe:` の読み方**：コアが、そのリソースの provider の `Read` を**観測用の資格**（Apply の中でも書き込みの文脈を外す。#185・#186）で、**ふだんの範囲のまま**呼ぶ。読めるのは **intent が宣言するリソースだけ**（宣言に無い ID・`ensure: absent` は読み込みで断る）で、netconf のリソースなら部分木の filter の `get-config` だけ、機器の NACM の外は読まない（#176）。比べ方はふだんの収束と同じ（そのリソースの宣言に `expect` を重ね、diff と PlanRefiner に通して `expect` の欄の差分だけを見る。netconf の `config` なら正規形と `ordered`）。`expect` に computed・managed・secret の欄は書けない。リソースそのものは書けない（`self` が読む）。
- **`check:` の関数の渡し方**：自前のバイナリ（§28.9）で `cli.Main(cli.WithConfirmCheck("route-seen", func(ctx context.Context, t provider.ConfirmTarget) error { … }))`。network_policy の realizer（`cli.WithRealizer`、§7.6）は `netpolicy.Output` の `Checks`（名前 → 関数）でも渡せる（返すリソースの `Fields` の `confirm` に `check:<名前>` と書く）。同じ名前を 2 度登録すると断る。登録の無い名前は読み込みで断る。関数には変えたリソースの ID（`t.ID`）と宣言の値（`t.Fields`）が渡り、`ctx` は観測の文脈（`provider.Writing(ctx)` は false。書き込み用の資格を使わない）で、期限は時間の上限。
- **いつも変えたあとだけ**：`observe:` と `check:` は commit confirmed と `self` の確かめのあとに確かめる（`after:` の印は書いても書かなくても同じ）。時間の上限は `self` と同じ（commit confirmed から confirm-timeout の半分）。
- **通らなければ確定しない**：`self` が落ちたときと同じ（確定の commit を送らず機器の巻き戻しに任せ、permanent で同じ宣言は変わるまで当て直さない）。知らせには通らなかった先と最後に見た理由を出す（例 `not confirmed: observe:netconf//sw2/if-peer: config not as expected (…) within 5s …`。`expect` の値そのものは出さない）。
- **commit_group** では、group の全部の台が commit confirmed を終え、全部の台の `self` が通ってから、全部の台の `observe:`・`check:` を待つ。1 つでも通らなければ、どの台も確定しない。
- 限界：netconf の観測は `get-config` の部分木なので、`observe:` で機器の operational の状態（経路表など）は読めない（#201）。それを見たいときは `check:` の関数で読む。

## 部分木だけに絞ったユーザー（NACM）

NETCONF のユーザーは、機器の側の NACM（RFC 8341）で**宣言する部分木だけ**を読み書きできるよう絞って運用できる（Backlog #176）。Sakutei が送るものは決まっていて、どれも部分木の外を読み書きしない。

| 使う所 | 送る rpc | 読み書きする所 |
| --- | --- | --- |
| hello | （rpc でない） | 機器の hello の capability を読むだけ。`<get>` で netconf-state・yang-library を読みに行かない |
| 観測 | `<get-config>`（running、部分木の filter）・`<close-session>` | 部分木だけを読む |
| 2 段の Apply | 上に `<lock>`・`<unlock>`（candidate）・`<edit-config>`・`<validate>`・`<commit>`（confirmed・確定・persist-id）・`<discard-changes>` を足す | 部分木だけを置き換える（外には触れない、上の「部分木の置き換え」）。確かめは新しい接続の `<get-config>` |

- `<lock>` は candidate の**全体**を締める（NETCONF の部分木の lock は running の partial-lock〈RFC 5717〉しか無い）。締めるだけで外を読み書きしない。lock の間の candidate は自分の変更しか持たないので、`<discard-changes>`・`<validate>`・`<commit>` も外を変えない。
- 絞ったユーザーに要るのは「**部分木の読み書き**」と「**上の rpc の実行**」だけ。観測だけなら「部分木の読み」と `get-config` の実行だけでよい。`close-session` はいつも許される（RFC 8341 §3.4.4）。

### 絞り方の例

機器の `ietf-interfaces` の `uplink` だけを Sakutei に任せるときの NACM（running の `ietf-netconf-acm` に入れる。入れ方は機器しだい）。部分木の一覧（`subtrees.yaml`）の filter と同じ所を `path` に書く。

```xml
<nacm xmlns="urn:ietf:params:xml:ns:yang:ietf-netconf-acm">
  <groups>
    <group><name>sakutei</name><user-name>sakutei</user-name></group>
  </groups>
  <rule-list>
    <name>sakutei</name>
    <group>sakutei</group>
    <!-- 部分木の読み書き（部分木ごとに 1 つ。読むだけなら access-operations を read に） -->
    <rule>
      <name>subtree-if-uplink</name>
      <path xmlns:if="urn:ietf:params:xml:ns:yang:ietf-interfaces">/if:interfaces/if:interface[if:name='uplink']</path>
      <access-operations>*</access-operations>
      <action>permit</action>
    </rule>
    <!-- Sakutei が送る rpc（観測だけなら get-config だけ） -->
    <rule><name>rpc-get-config</name><module-name>ietf-netconf</module-name><rpc-name>get-config</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-lock</name><module-name>ietf-netconf</module-name><rpc-name>lock</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-unlock</name><module-name>ietf-netconf</module-name><rpc-name>unlock</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-edit-config</name><module-name>ietf-netconf</module-name><rpc-name>edit-config</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-validate</name><module-name>ietf-netconf</module-name><rpc-name>validate</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-commit</name><module-name>ietf-netconf</module-name><rpc-name>commit</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <rule><name>rpc-discard-changes</name><module-name>ietf-netconf</module-name><rpc-name>discard-changes</rpc-name><access-operations>exec</access-operations><action>permit</action></rule>
    <!-- ほかは読み・書き・実行のすべてを断る -->
    <rule><name>deny-rest</name><module-name>*</module-name><access-operations>*</access-operations><action>deny</action></rule>
  </rule-list>
</nacm>
```

- 規則は rule-list の中で頭から当たる（最初に当たった規則で決まる）。`deny-rest` は最後に置く。ユーザーが別の group にも属していれば、その group の rule-list も効く（RFC 8341 §3.4.4）。
- `deny-rest` を書かなくても、NACM の既定（`write-default` は deny）で外は書けない。読みも絞るなら `deny-rest`（か `read-default` を deny）にする。
- NACM 自身（`ietf-netconf-acm`）や鍵などの部分木は既定で拒否（`nacm:default-deny-all`）なので、それを部分木にするときは `path` で明示して許す（統合テストはこの形）。
- ssh の利用者（上の `sakutei`）の作り方・鍵の置き方は機器しだい。Sakutei の側は対象の一覧（`hosts.yaml`）の `user` にその名前を書くだけ。

### 断られたとき

| 起きたこと | 答え |
| --- | --- |
| 観測の `<get-config>` が `access-denied` | 観測できない（unknown、PermanentError）。理由に error-path と「get-config の実行と部分木の読みを許す」 |
| Apply の `<lock>`・`<edit-config>`・`<commit>` などが `access-denied`（部分木の外を変える宣言・許されていない rpc） | 何も確定しない（edit-config の後なら candidate を discard し、unlock する）。Apply は permanent で、error-path と要る権限（部分木の読み書き・上の rpc の実行）を理由に書く。同じ宣言は変わるまで当て直さない |
| **読むのを許されていない部分木** | NACM は読めないデータを**黙って省く**（RFC 8341 §3.4.5）ので、機器に無い（absent）と**見分けられない**（既知の限界）。plan には `CREATE` と出る。観測だけなら当てない。Apply を許していれば、機器が `access-denied` で断る（上の行。何も書かない）。部分木の一覧には、ユーザーに読むのを許した部分木だけを書く |

## 観測用と書き込み用の資格（Backlog #185）

対象の一覧の機器に観測用（`observe:`）と書き込み用（`write:`）の資格を分けて書ける（[../remote.md の「観測用と書き込み用の資格」](../remote.md#観測用と書き込み用の資格)）。観測（`get-config`）は観測用のユーザーのセッションで、2 段の Apply（lock・edit-config・validate・commit、新しい接続での確かめの読みと確定）は書き込み用のユーザーの新しいセッションで行う。

- 観測用のユーザーには、NACM で宣言した部分木の読みと `get-config` だけを許す（上の「部分木だけに絞ったユーザー」の読みの規則）。部分木の外は読まないので、読むだけでは観測できない項目は無い。
- `netconf_apply` を `true` にしない（観測だけの）構成では、書き込み用の資格は使われない。

## 統合テスト

公開の NETCONF サーバ（netopeer2 のコンテナ）を相手に回す（Backlog #141・#175・#176・#199・#200）。build tag `integration_netconf` の付いたファイルにあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-netconf
```

要るもの: docker（パスワードなしの `sudo -n`）、`ssh`・`ssh-keygen`。イメージは `sysrepo/sysrepo-netopeer2:latest@sha256:36cc1d841c97…`（tag と digest で固定。`SAKUTEI_IT_NETOPEER2_IMAGE` で差し替えられる）。

- 機器を 1 台（`sakutei-ncit-sw1`）、127.0.0.1 の空いたポートに立てる。Sakutei は `netconf` ユーザで鍵で入る（テストが作った鍵を authorized_keys に置き、秘密鍵は file SecretBackend から）。ホスト鍵はコンテナの中から読んで known_hosts に書く（初回だけ信じることはしない）。
- 観測する部分木は ietf-netconf-acm の 2 つ: `sw1/acl-rules`（rule-list。rule は ordered-by user なので `ordered: nacm/rule-list/rule`）と `sw1/group`（group の user-name。宣言は機器と違う順で書く）。どの利用者も属さない group なのでアクセスの制御には効かない。
- 「手で変える」は、テストが ssh で直に開く**別の NETCONF セッション**の `<edit-config>`（Sakutei の外。base:1.0 だけを出すので `]]>]]>` の区切り）。netconf ユーザに書き込みを許す NACM の規則（group `sakutei-it-admin` と rule-list `sakutei-it-admin` の `permit-all`）は、準備のときにコンテナの中の sysrepocfg で入れる。Sakutei 自身は base:1.1（chunked）で話す。
- 2 段の Apply は、`netconf_apply: "true"`・`netconf_confirm_timeout: 10s` の provider の設定で回す（1.0 の流れと、`netconf_commit_flow: "1.1"` の persist の流れ）。netopeer2 の hello は `:candidate`・`:confirmed-commit:1.1`・`:validate:1.1` を出す（1.0 の流れは 1.1 の機器でも同じに動く）。
- 確かめの失敗は「Sakutei が自分を締め出す宣言」で起こす: 部分木 `sw1/admin-rules`（rule-list `sakutei-it-admin`）の頭に、ietf-netconf-acm を読むのを断る規則 `deny-read-nacm` を足す。当てると、新しい接続からは NACM の部分木が読めなくなる（`<data/>` が返り、宣言どおりと確かめられない）。書き込みは許したままにする。
- **部分木だけに絞ったユーザー**（#176）: コンテナに 2 人目の利用者 `sakutei` を作り（同じ鍵）、準備のときに sysrepocfg で NACM の group `sakutei-scoped` と rule-list `sakutei-scoped` を入れる。許すのは `sw1/acl-rules`・`sw1/group` の 2 つの部分木の読み書き（`path` の規則）と、`get-config`・`lock`・`unlock`・`edit-config`・`validate`・`commit`・`discard-changes` の実行だけで、最後の `deny-rest` でほかの読み・書き・実行をすべて断る（上の「絞り方の例」と同じ形）。このユーザーで Apply を許した設定（1.0 の流れ）を回す。
- **commit_group**（#199）: 別に 3 台（`sakutei-ncit-g1`〜`g3`）を立て、3 台の部分木 `acl-rules` を `commit_group: leafs` にまとめる。confirm-timeout は g1・g3 が 10 秒、g2 が 20 秒（`netconf_confirm_timeouts: g2=20s`）で、時間の上限は一番短い 10 秒の半分。確かめの失敗は、g3 だけを Sakutei 自身が NACM を読めなくなる宣言（`g3/admin-rules` に `deny-read-nacm`）にして起こす。
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
| 絞ったユーザー（NACM） | 手で変えた 2 つを、絞ったユーザーの設定の status が drift と見て、reconcile が 2 段で当てる（`2 change(s) applied`）→ conformant、機器は宣言どおりで confirm-timeout の後も残る。その間の netopeer2 のログに断った rpc（`Operation not authorized`・`User callback failed`・`denied`）が無い（外を触る rpc を送っていない）。部分木の外（`sw1/admin-rules`）を変える宣言は、観測では absent に見え（NACM が黙って省く）、Apply が `edit-config on the candidate: … access-denied at /ietf-netconf-acm:nacm/rule-list[name='sakutei-it-admin']/…` で断られて permanent、`the candidate was discarded, nothing was committed`。機器の NACM は変わらない。そのあとも絞ったユーザーで部分木の中は当てられる（lock と candidate が残っていない） |
| commit_group：全部が通る | 3 台の rule の順を手で入れ替え、group の reconcile → plan の 3 つの操作に `commit_group: leafs`、`3 change(s) applied`、台ごとに `CommitGroupApplied` の committed。3 台とも宣言どおりで、confirm-timeout の後も戻らない。次の reconcile は Converged |
| commit_group：1 台の確かめが落ちる | g1・g2 の rule の順と g3 の `deny-read-nacm` を同じ group で reconcile → g3 は新しい接続で宣言どおりと確かめられず（5 秒で打ち切る）、どの台も確定しない（3 台とも not committed）。netopeer2 は `:confirmed-commit:1.1` を持つので、3 台に `<cancel-commit>` を送ってすぐ戻す。g1・g2 は手で変えたまま、g3 に `deny-read-nacm` は無い |
| confirm の observe:：相手が期待どおり（#200） | g1 の rule の順を手で入れ替え、g2 は r1, r2 の順にして、g1 の `acl-rules` を `confirm: self, observe:netconf//g2/acl-rules expect:{"config": <r1, r2 の順>}` で reconcile → `1 change(s) applied`、g1 は宣言どおりで confirm-timeout の後も戻らない |
| confirm の observe:：相手が期待と違う（#200） | g1・g2 とも逆の順にして（g2 の宣言は今の running どおりなので変えない）同じ confirm で reconcile → `not confirmed: observe:netconf//g2/acl-rules: config not as expected … the confirming commit was not sent; the device rolled back at the timeout`。g1 は手で変えたまま、g2 は変わらない（読むだけ） |
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

絞ったユーザーの段を足したあと（#176、同じ開発機・同じ版。3 回回して 3 回とも PASS）:

```
--- PASS: TestNetconfIntegration (54.92s)
    --- PASS: TestNetconfIntegration/conformant (1.03s)
    --- PASS: TestNetconfIntegration/changed_by_hand:_drift,_never_applied (1.02s)
    --- PASS: TestNetconfIntegration/restored_by_hand:_conformant,_no_drift_in_any_cycle (0.33s)
    --- PASS: TestNetconfIntegration/apply_1.0:_commit_confirmed,_checked,_confirmed (13.04s)
    --- PASS: TestNetconfIntegration/apply_1.1:_persist (12.89s)
    --- PASS: TestNetconfIntegration/scoped_user_(NACM):_observed_and_applied_inside,_refused_outside (14.14s)
    --- PASS: TestNetconfIntegration/apply:_the_check_fails,_the_device_rolls_back_at_the_timeout (10.62s)
    --- PASS: TestNetconfIntegration/stopped:_unknown (1.64s)
PASS
```

絞ったユーザーで部分木の外を変える宣言の `sakutei reconcile`:

```
Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)
sakutei: apply netconf//sw1/admin-rules: permanent: netconf: Apply "sw1/admin-rules": edit-config on the candidate: netconf: rpc-error: protocol access-denied (error) at /ietf-netconf-acm:nacm/rule-list[name='sakutei-it-admin']/rule[name='outside']: Access to the data model "ietf-netconf-acm" is denied because "sakutei" NACM authorization failed.; the device refused it for the user (access-denied): the user may not write this part, or may not execute the rpc. Sakutei writes only inside the subtree; it needs the user to read and write the subtree and to execute lock, unlock, get-config, edit-config, validate, commit and discard-changes (NACM, RFC 8341). A subtree the user may not read is observed as absent; the candidate was discarded, nothing was committed
```

commit_group の段を足したあと（#199、2026-10-29、同じ開発機・同じ版。機器を 4 台に）:

```
--- PASS: TestNetconfCommitGroupIntegration (22.01s)
    --- PASS: TestNetconfCommitGroupIntegration/every_check_passes:_all_three_confirmed (15.24s)
    --- PASS: TestNetconfCommitGroupIntegration/g3's_check_fails:_no_device_confirmed (6.77s)
--- PASS: TestNetconfIntegration (53.50s)
    --- PASS: TestNetconfIntegration/conformant (0.50s)
    --- PASS: TestNetconfIntegration/changed_by_hand:_drift,_never_applied (0.67s)
    --- PASS: TestNetconfIntegration/restored_by_hand:_conformant,_no_drift_in_any_cycle (0.33s)
    --- PASS: TestNetconfIntegration/apply_1.0:_commit_confirmed,_checked,_confirmed (13.03s)
    --- PASS: TestNetconfIntegration/apply_1.1:_persist (12.85s)
    --- PASS: TestNetconfIntegration/scoped_user_(NACM):_observed_and_applied_inside,_refused_outside (14.00s)
    --- PASS: TestNetconfIntegration/apply:_the_check_fails,_the_device_rolls_back_at_the_timeout (10.44s)
    --- PASS: TestNetconfIntegration/stopped:_unknown (1.60s)
PASS
```

g3 の確かめが落ちたときの `sakutei reconcile`（抜粋）:

```
Reconcile: not converged: 3 resource(s) failed to apply (no changes applied)
sakutei: commit_group "leafs": netconf//g1/acl-rules: not committed: transient: netconf: Apply "g1/acl-rules": not committed: commit group "leafs": g3/admin-rules was not confirmed; no member was confirmed; cancel-commit was sent; the device rolled back at once (observed over a new connection)
sakutei: commit_group "leafs": netconf//g2/acl-rules: not committed: transient: … (g1 と同じ)
sakutei: commit_group "leafs": netconf//g3/admin-rules: not committed: permanent: netconf: Apply "g3/admin-rules": not confirmed: the running subtree is not as declared within 5s (half of the shortest confirm-timeout 10s of commit group "leafs"); no member was confirmed; cancel-commit was sent; the device rolled back at once (observed over a new connection)
```

netopeer2 2.1.49 で見たこと（#176）: `get-config` は rpc の規則で断っても通った（読みはデータの規則で絞られる）。断った rpc はログに `Operation not authorized`、断った書き込みは `User callback failed` と出る。読みで省いたデータはログに出ない（外を読んでいないことは、偽のサーバの単体テストで確かめる）。

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

### 結果（2026-10-30、confirm の observe:、#200）

`make integration-netconf` は全部の段が通った（`TestNetconfConfirmObserveIntegration` の 2 段、`TestNetconfCommitGroupIntegration`、`TestNetconfIntegration`）。g2 が期待と違うときの `sakutei reconcile`（抜粋）:

```
Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)
sakutei: apply netconf//g1/acl-rules: permanent: netconf: Apply "g1/acl-rules": not confirmed: observe:netconf//g2/acl-rules: config not as expected (netconf g2/acl-rules: the device's subtree differs from the declaration (0 line(s) only on the device, 0 only declared): the order of an ordered list differs) within 5s (half of the confirm-timeout 10s); the confirming commit was not sent; the device rolled back at the timeout (observed over a new connection)
```

## 通常のテスト

`make check` に入るもの（ネットワークにも docker にも出ない）:

- `internal/netconf`: 区切り（1.0・1.1、RFC 6242 の例、壊れた区切り）・正規形・ordered と、Go の中で動く偽の NETCONF サーバ（`internal/netconf/netconffake`。base:1.0 だけのものと 1.1 のもの、subtree filter を当てる、受けた rpc を残す）を相手にした観測・ずれ・absent・unknown・開き直し・読み込みの検査（[AC-141-1-*]・[AC-141-2-*]）。
- `internal/netconf`（2 段の Apply、#175）: 部分木の置き換えの edit-config の中身（[AC-175-1-*]）と、偽のサーバの candidate・lock・edit-config・validate・commit confirmed 1.0／1.1（persist・persist-id）・confirm-timeout とセッションの終わりの巻き戻しを相手にした、1.0 の流れの確定（同じセッションの 2 つの commit の間に新しい接続で確かめる）・確かめの失敗の時間切れの巻き戻し・半分での打ち切り・成否不明・persist・機器が持たないとき・lock の取り合い・機器が受けない宣言（[AC-175-2-*]）。
- `internal/remote`: ssh のサブシステム（偽の ssh で、`-s` で開き双方向に流れる・多重化しない・sudo を付けない・鍵の一時ファイルが残らない・ssh の失敗が `*ConnError`、[AC-141-3-*]）。
- `internal/netconf`（絞ったユーザー、#176）: 偽のサーバの `Scope`（読み書きできる所を subtree filter の形で）・`ScopeOps`（許す rpc）で NACM の近似をし、観測と 2 段の Apply（1.0・1.1）が通って外を読む・書く・ほかの rpc を送るが 1 つも無いこと、許されていない部分木は absent に見えて Apply が access-denied で permanent（何も確定せず、candidate を捨て lock を放す）、get-config・lock の access-denied、discard-changes の失敗の知らせ（[AC-176-1-*]）。
- `tests/acceptance/netconf_apply_test.go`: Apply を許したデーモンの周（ずれを 2 段で直し、次の周は適合・確かめの失敗は確定せず時間切れで戻り、同じ宣言を当て直さない・alert-only の周は当てない）と registry（`netconf_apply` の有無と誤った設定、[AC-175-3-*]）。
- `tests/acceptance/netconf_attrs_test.go`（#208）: 偽のサーバの `TopAttrs`（get-config の返りの最上位の要素に読むたびに値の変わる属性を付ける）・`EmptyData`（部分木が無いときに名前空間の無い空の `<configuration/>` を返す）で、属性だけの違いは drift にならない・2 段の Apply が確定まで通り次の周は適合・部分木が無いときの plan が `CREATE`・宣言に書いた属性が違えばその要素と属性を名指しした理由で drift（[AC-208-1-*]）。
- `tests/acceptance/netconf_confirm_test.go`（#200）: confirm の `observe:` の先が期待どおりなら確定し、違えば確定せず元に戻る・`observe:` は観測用の資格（書き込みの文脈でない）と範囲（部分木の filter の get-config だけ、NACM の外を読まない）で読み、宣言に無いリソースは読み込みで断る・外の realizer の `Output.Checks` の `check:` を nil になるまで繰り返し呼ぶ（nil にならなければ確定しない）・commit_group では全部の台が commit confirmed を終えてから `check:` を待つ・`self` の無い confirm や登録の無い `check:` を読み込みで断る（[AC-200-1]〜[AC-200-6]）。`internal/netconf` の confirm の書式、`internal/reconcile` の `provider.Confirmer` の実装も。
- `tests/acceptance/netconf_test.go`: デーモンの周（ずれを毎周知らせ、Apply は 0、ほかの provider は同じ周で収束する、止めると unknown、GET /plan の `blocked_by: observe_only`）、ObserveOnly を型で止める（executor・起動時の検査）、CLI の plan・reconcile・status（偽の ssh の先でテストのバイナリが偽のサーバになる）、registry（[AC-141-4-*]）。
