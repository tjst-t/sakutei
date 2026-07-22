> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第VI部 衛星と外部世界

reconciler（収束コア）の外側には、core が自前で持たないと決めた state・資格・供給・判定を担う三つの衛星（allocator＝台帳 / credential-broker＝関守 / catalog＝図面）と、作らずに委ねる外部世界がある。本部はその境界を定める。貫くのは §3 の第4原則「持たない・委譲する」と、本部で設計保証まで引き上げる次の一文である——**衛星の停止は、変化を止めるが、維持を止めない。**

## §19 allocator（台帳）

**名称の三者関係**：**コンポーネント名 = `allocator`**、その **interface = `AllocationAuthority`**、担う **役割 = 採番権威（allocation authority）**。本節で「authority」と呼ぶのは役割を指し、実装コンポーネントは `allocator`、コード上の型は `AllocationAuthority` である。allocator は v1a では実装せず、v1b で最小実装する（§27）。

### §19.1 払い出しは第3のカテゴリ

VLAN ID / IP / ASN の**プールからの払い出し**は、inventory（事実の記録）でも intent（構成の意図）でもない。**副作用を伴う書き込み**であり、しかも**素朴だと冪等でない**——毎周同じ intent を見て毎回新しい ID を払い出し、プールを食い潰す。これが払い出しを第3のカテゴリとして扱う理由である。

**解決：要求（冪等）と割り当ての事実（state）に割る。**

- **要求**（intent・冪等）：ユーザは「VLAN が1つ必要」と書く。具体 ID は指定しない。

  ```yaml
  resource: virtual_network
  id: app-net
  vlan: { from_pool: tenant-acme-vlans }   # プールから1つ要求（ID 指定なし）
  ```

- **割り当ての事実**（state）：`app-net → VLAN 573` を**外部 authority が永続化**する。次サイクルで「app-net は既に 573」と分かり、**二重払い出しを防ぐ = 冪等性が回復する**。

→ authority が「前回何を払い出したか」の記憶装置になり、収束エンジン自身は state を持たずに済む。値・配置と同じく、state（冪等性の記憶）も自前で持たず外に委ねる（§3 第4原則）。

### §19.2 authority の3役

| 役割 | 読み書き |
|---|---|
| **inventory**（既にある事実） | 主に read |
| **allocation authority**（プールと払い出し記録） | **read-write** |
| **reality**（収束結果の逆流） | 収束エンジンが唯一の書き手 |

**authority を採番の単一権威にする**：払い出しは必ず authority の API 経由とし、収束エンジンは自前プール管理を持たず「1つくれ」と頼む。→ **並行・シャード環境でも競合しない**。分散合意を core に戻さず、採番調停だけ外部に外出しできる（§18）。

### §19.3 要求と割当事実の interface

`AllocationAuthority` は実装非依存の seam であり、5 つの操作からなる。冪等な要求（`Allocate`）と、PR フロー用の二段階払い出し（`Reserve → Confirm`）、非対称な返却（`Release`）、キャッシュ再取得のための `Lookup` を持つ。

```go
type AllocationAuthority interface {
    Allocate(ctx, pool, owner string) (value string, err error)                   // 冪等：同 owner → 同 value
    Reserve(ctx, pool, owner string, ttl time.Duration) (value string, err error) // PR 用の仮予約
    Confirm(ctx, pool, owner string) error                                        // マージ後 render が昇格
    Release(ctx, pool, owner string) error   // ★下流削除を List 確認後のみ。cooldown あり
    Lookup(ctx, pool, owner string) (value string, ok bool, err error)
}
```

`Allocate` は冪等である——同じ `owner` には常に同じ `value` を返す。これが §19.1 の「二重払い出しを防ぐ」を型として担保する。

### §19.4 仮予約（Reserve → Confirm）

platform 層（PR 必須・§7）で PR 生成時に本払い出しすると、**却下時にリークする**。

```
PR 生成時   ： TTL 付き Reserve（仮予約）→ PR diff に具体 ID が出る（何が使われるか人が見える）
マージ時    ： Confirm（本払い出しに昇格）。★Confirm はマージ後 最初の render フェーズ1が実行
却下 / TTL 切れ： 自動的に解放（リークしない）
```

テナント層（即時・§7）は仮予約不要で直接 `Allocate` する。書込経路の非対称（platform=PR / tenant=即時）が採番の払い出し様式にもそのまま現れる。

### §19.5 Release は非対称原則に従う

素朴な「所有者が desired から消えたら返却」は**危険**：render 入力が stale（CMDB 断・理想の一時的な読み損ね・§9）→ owner 消滅と誤認 → Release → **使用中の VLAN/IP が別リソースに再払い出し = 衝突**（ネットワークでは最悪級の事故）。したがって Release は §12 の削除の規律（確信が持てないとき減算はしない）にそのまま従わせる。

- **（a） desired からの消滅では Release を発火させない。下流リソースの削除完了を確認してから**実行する（「理想から消えた」ではなく「**現実から消えた**」を根拠にする）。
- **（b） 返却後に cooldown / 検疫 TTL**（即時再払い出しをしない。DHCP のリース再利用猶予と同型）。
- **（c） `pruneable:false` 等で下位が残存する間は Release 保留**。

**「現実から消えた」を何で確認するか**：`Observation` の三値（`ok`/`unknown`/`converging`・§4）には **absent（消えた）を積極的に表す値が無く**、`Read` が `unknown` を返す状態と「本当に消えた」を区別できない。個別 `Read` に第4値を足すと全 provider に波及するため、**Release の削除完了確認は `Read` ではなく `List`（集合演算）を根拠にする**。

- **削除完了 = 対象 ID が scope の List 結果に現れなくなったこと**（§12 の「消すべき = 現実にある集合 − 理想にある集合」と同じ集合演算）。これは §12 の List 完全性検証（件数急減 halt・ページング完走）をそのまま前提にでき、`unknown` との混同を避けられる。
- したがって **Release は Lister を持つ provider の下流にのみ許可**する（§12 の削除判定3種と接続：離散・被参照 = 参照カウント0、容量プール = 残余充足、単純専有 = List 集合差）。List できない下流の Release は保守側（保留）に倒す。この規律により Release が §4 / §5 / §12 と型レベルで接続する。

### §19.6 QPS 算術——「allocator がスケール天井」という誤解を数値で潰す

早期収束（採番が UX 直結・§8）と claim 分割（全シャードが共有衛星に依存・§17）の両方が allocator の QPS・可用性に合流する。だが allocator への QPS は**フリート規模ではなく変更レート（churn）に比例する**。これは次の観測 state レスとの整合から導かれる。

- **割当事実は Release まで不変**：allocator が `app-net → VLAN 573` を確定させたら、その事実は Release（§19.5）まで変わらない。したがって **`Lookup`（§19.3）の結果はシャードローカルに無期限キャッシュしてよい**——無効化は**自分の Release のときだけ**（他者が自分の割当を変えることはない・§19.7 の 1 プール 1 権威）。
- **観測 state レス原則との整合**：これは観測 state レス（§3）に反しない。割当事実は「**観測**」ではなく「**自分が確定させた事実**」だからである。現実を再観測して得る値（毎周見に行くべき S1・§4）ではなく、自分が払い出しを依頼して得た確定値である。喪失しても allocator に `Lookup` し直すだけで再取得できるので、これは **S2（喪失＝事故）ではなく再取得可能なキャッシュ = S3 扱い**である（§4）。喪失時の縮退は「一度 allocator に問い直す」だけで、事故にならない。

この帰結として、**定常状態の収束ループは衛星コールゼロで回る**。QPS 算術を第一級の設計事実として置く。

- 例：**10,000 リソース・日次 churn 1% = 新規採番 ~100 件/日 ≒ 無負荷**（定常の 9,900 リソースは Lookup キャッシュヒットで allocator に触れない）。フリートが 10 万に増えても、churn が同じなら QPS は 10 倍で済み、「リソース数に比例して allocator が詰まる」は起きない。
- **早期収束のバースト = ポータル申請レート**（§8）：早期収束で新規採番が要るのは「人がポータルで申請した瞬間」だけであり、その頻度は人の操作レート（申請/秒）で上限される。定常収束はバーストを生まない。
- したがって allocator の sizing は「フリート規模」ではなく「**変更レート（churn）＋ ポータル申請ピーク**」で行う。これが §17 の「共有衛星が次の天井」を実務上大きく緩める根拠である。

secret も同型である（§20）：割当事実に対する Lookup キャッシュと、secret に対する last-resolved 保持＋ TTL キャッシュは、どちらも「自分が確定させた／解決した値の局所保持」であり、定常収束を衛星ゼロで回す。

### §19.7 state 喪失と分断（1 プール 1 権威）

authority は state を持つ（§19.1）ゆえに **喪失と分断の設計が要る**。

- **state 喪失時の再構築（逆引き）**：割当記録（§19.9 の SQLite）が失われたとき、**現実から逆引きして再構築する手順**を持つ。`owner → value` は現実側（機器に設定された VLAN/IP）を Observe / List すれば多くが復元でき、復元できない分は cooldown 中の未使用として保守側に倒す。喪失＝即再払い出し衝突を防ぐ。
- **1 プールにつき 1 権威**：地理分散で 2 権威が同一プールから独立払い出しすると復帰時に採番衝突（§19.5 が最悪級と呼ぶ事故）になる。→ **プールをサイトごとに静的分割**（サイト A = VLAN 1–500、B = 501–1000）し、**そもそも 2 権威が同一プールに触れない**ようにする（shared-nothing を採番プールにも適用）。分断中も各サイトは自分のプール範囲内で独立に払い出せる。

### §19.8 HA 最小形とプール分割 by claim

**allocator HA 最小形**：v1b の自作実装は **段0 の VM-HA 上に置き、active/standby + fencing token（§15 の fencing を再利用）** で保護する。allocator は外出しした共有 state の権威（§19.1）であり、その喪失は最も痛い（§19.7）ため、**可用性目標は reconciler 以上**とする。

- **fencing の再利用**：active/standby フェイルオーバーで旧 active が生きていると 2 権威が同一プールから独立払い出しし、復帰時に採番衝突（§19.5 が最悪級と呼ぶ事故）になる。→ 割当記録に **fencing token / 世代番号**を持たせ、standby 昇格時に世代を上げ、古い active の Confirm/Release を fence する（§17 の単一 writer fencing と同型）。
- **quorum ストアへの相乗り可**：割当記録（最も喪失が痛い S2・§4）は、**S2 の永続化先（quorum ストア or 同期レプリカ + fencing・§4）と同じ置き場に相乗りしてよい**。allocator 専用の別ストアを立てる必要はなく、S2 の置き場が決まればそこに載る。
- ただし **§19.6 / §22 の原則により、allocator HA は「変化（新規採番）の可用性」のためであって「維持（定常収束）の可用性」のためではない**——定常収束は allocator が落ちていても Lookup キャッシュで回る。HA は新規払い出しの UX（§8 の早期収束）を守るための投資であり、システム全体の生存条件ではない。

**プール分割 by claim**：サイト / シャード固有のプールは、**intent でレンジを disjoint に分割宣言する**（§19.7 の「1 プール 1 権威」をシャード軸に一般化）。

```yaml
# 各サイトのプールレンジを intent で disjoint に宣言（claim と対応）
pools:
  - { name: site-a-vlans, range: "100-199", claim: {site: site-a} }
  - { name: site-b-vlans, range: "200-299", claim: {site: site-b} }
```

- サイト固有プールを disjoint に切ると、**シャードは自分のレンジだけを自 allocator（またはローカルインスタンス）で管理でき**、中央 allocator への依存は「**全シャード共有プール**」（サイトをまたぐ ASN 等）だけに縮む。
- **実行時のレンジ委譲プロトコルは作らない**——分割は **intent の宣言で足りる**（claim-as-intent・§17 と同じ思想：動的な調停機構でなく、静的な宣言で分割境界を固定する）。分断中も各シャードは自分のレンジ内で独立に払い出せる。

### §19.9 waiting-allocation 縮退と実装の切替

**allocator が停止しても定常収束は止まらない**（§19.6・§22）。止まるのは**新規採番が必要なリソースだけ**である。これを専用の縮退状態として定義する。

- **allocator 停止 → 新規採番が必要なリソースだけが `waiting-allocation` 状態**になる。これは `converging`（§11.1）に類似した「一時的に進めない」状態であり、**エラーでも halt でもなく、下流 skip のみ**（§8 の失敗伝播と同じ機構）。既存リソースの収束は Lookup キャッシュ（§19.6）で**継続**する。
- **Release は元々非対称・遅延可**（§19.5）：allocator 停止中に Release を発火できなくても、Release は「下流削除を List 確認してから・cooldown 付き」で元々遅延前提なので、停止中は単に保留され安全側に倒れる（加算はしても減算はしない・§3）。
- **早期収束パスの縮退**：allocator 停止中、新規払い出しが要る早期収束ヒント（§8）は**キューに保持し、定期周期へ縮退**する（§8 の「溢れたヒントは次の定期周期へ」と同じ規律）。allocator 復帰後の周期で自然に払い出され、`waiting-allocation` が解ける。UX としては「申請が即時反映されず、allocator 復帰まで遅延する」として現れる——正しさは損なわれない。

**自作最小実装と外部権威の切替**：NetBox / Infoblox / phpIPAM / 自作を実装として差し替え可能（`AllocationAuthority` seam）。**独立 CMDB 全体は不要**——inventory は当面ファイル、reality 逆流は将来、残るのは**採番だけ**である。**採番は state を持つので自作の価値があり**（intent は git が正だから UI も供給源も薄くてよい＝ state を持つ部分と持たない部分で自作の意味が違う）、最小実装で足りる：

- **SQLite + `BEGIN IMMEDIATE`（アトミック払い出し）＋ フラットプール**。`BEGIN IMMEDIATE` で書込ロックを即時取得し、同一プールへの並行 `Allocate` を直列化して二重払い出しを防ぐ。

**守る2規律**：（1） `AllocationAuthority` interface の裏に隠して差し替え可能に保つ。（2） スコープを**採番に限定**し、CMDB 全体に育てない。

---

## §20 secret と資格

秘密には二つの経路がある。リソースに**注入する設定値**（`SecretRef`）と、エンジン自身がターゲットに**到達するための資格**（`CredentialProvider` / credential-broker）である。どちらも「値は収束しない」原則を共有し、値の永続化・判定・rotation は Vault に委ねる（§3 第4原則・§22）。

### §20.1 値は収束しない——参照・存在・鮮度だけを扱う

理想状態（git）に値は書けない → 収束エンジンは「あるべき値」を知らない／知るべきでない → **値の diff は取れない**。よって**収束が扱うのは値ではなく参照・存在・鮮度だけ**。値は Vault の責務である（DB のデータを収束外にしたのと同じ切り分け）。

- **存在・鮮度は収束**：「このパスに存在すべき」を Observe（**存在だけ・値は見ない**）／ Apply（無ければ Vault に生成させる。値は見ずに委譲）。鮮度（`rotation: every: 90d`）も収束対象である。
- **rotation の実行は Task / Workflow**：「新値生成 → DB に設定 → アプリ設定更新 → 旧を無効化」の順序付き・協調が要る手続き（順序ミスで接続断）。「**いつ（90 日超）= 収束が検知、どう（回す手順）= Workflow**」（§6）。

**秘密の3分類（出自による扱いの分岐）。**「値を収束しない」原則は共通だが、**秘密がどこから来るか**で扱いが分かれる。この分類が、下記 §20.4 の層A/層B・§20.5 の rotation 収束にそのまま接続する。

| 分類 | 生成方法 | 対象例 | 人手 |
|---|---|---|---|
| **既存・手動登録** | 段0 admin が現地作業後、Vault に1回だけ登録 | 機器の初期パスワード、既存 API キー | 初回のみ要 |
| **新規・自動生成** | render フェーズ1 で Vault（または dynamic secret）が生成 | サービスの root password、cluster join token | 不要 |
| **rotation** | Workflow が定期・自動実行 | 上記いずれも、鮮度ポリシーに従う | 不要 |

**設計上の含意**：「既存・手動登録」は初回の1点だけ人手が要るが、**その後は自動生成・rotation の系に合流させる**のが正しい形である。**初期資格情報のまま運用し続けない**——bootstrap Task が初回接触時に自動で新しい値を生成・適用し、以後は rotation Workflow（§6・§20.5）に載せる。これは §20.4 の「層B（静的資格＋定期 rotate）」の具体的な運用パターンである。

### §20.2 SecretRef——JIT 解決と自動マスク

**早すぎる展開は禁**。読み込み時に全展開して理想状態に実値を埋めると、ログ・Event・ダンプに漏れる。展開が早いほど漏洩面が広がる。

- 理想状態は最後まで**参照のまま**保持し、実値化は **Apply 直前・provider に渡す瞬間だけ**。使用後にメモリから消す（`Zero()`）。diff は**参照同士で取る**（値を知らなくても diff できる）。
- **`SecretRef` 専用型**（文字列にしない）：`String()` が `***` を返し**自動マスク**、実値取得は明示 `.Resolve(ctx)` 経由のみ。「うっかり平文化」を型で防ぐ。
- **世代フィールド**：ユーザが書く参照には現れない。**render フェーズ1が Vault（KV v2）metadata から解決して `SecretRef` に注入する**（ピン留めではない）。値を隠したまま rotate を検知でき、**git への書き戻しループが生じない**。

```
理想状態（ピン無し）： password: ${secret:vault:/db/prod#password}
   ↓ render フェーズ1 が metadata から現世代を読む（値は読まない）
世代が下流リソースの入力ハッシュに合成される → rotate すれば次周 render で diff が出る
```

これが無いと「Vault で rotate したのに使う側が古いまま = 接続断」になる。世代は `intent_revision` と `rendered_revision` の差（原因タグ E＝外部入力変化・§10）として検出される。

### §20.3 Vault 断の縮退（TTL キャッシュと alert-only）

Vault は段0 の VM-HA 保護対象だが、sealed-on-restart（ノード障害・保守再起動のたびに sealed に戻り、unseal はオペレータ起動）ゆえに **secret 断が起こりうる**。断中の挙動を非対称原則で定める。

- **secret を持つ Apply のみ止まる**：Vault 断中、secret 依存リソースは **alert-only へ縮退**する（absent と断じず、`unknown` 扱い・§9 の render 入力障害時の非対称と同型）。
- **Observe は継続**：資格キャッシュ（§20.5）で Observe を続け、secret を持たない Apply も回る。
- **last-resolved + TTL キャッシュ**：直近に解決済みの世代を TTL 内で保持し、Vault 断中もキャッシュ済み世代で Apply できる。これは §19.6 の Lookup キャッシュと同型の「自分が解決させた値の局所保持」であり、定常収束を衛星ゼロで回す。

### §20.4 CredentialProvider と credential-broker（関守）

- **`SecretRef`** = リソースに**注入する設定値**（アプリに渡す DB パスワード、機器に設定する SNMP community）。**desired の一部**。
- **`CredentialProvider`** = **engine 自身がターゲットに到達するための資格**（スイッチにログインする短命アカウント、vCenter API トークン）。**desired の一部ではなく実行時の資格**。credential-broker が発行する。

```go
type CredentialProvider interface {
    Acquire(ctx, target Target, identity Identity) (Credential, error)
    Release(ctx, Credential) error
}
```

→ provider が機器にログインする資格は `CredentialProvider` 経路。機器に設定する値は `SecretRef` 経路。

**credential-broker（関守）を core から分離する理由**：

1. **ライフサイクルが違う**：core = 常時収束ループ、broker = 要求応答型。
2. **可用性が逆立ちしている**：broker は core の**依存先（下位）**。**core が死んでも認証が生きていないと、緊急時に人が機器に入れない**。core 内に認証を置くと致命的な循環依存になる。さらに **broker 断は Vault 断より重い**——Vault 断は「secret 依存の Apply が止まる」だけだが、**broker 断は認証が要る全ターゲットの Observe が止まる = システム全盲**（§22 の停止影響表）。
3. **セキュリティ境界**：認証は最大の攻撃対象。広い攻撃面（provider 群 / Ansible 連携）を持つ core と同居させず、小さく監査可能な別プロセスに隔離する。

**核心原則：静的資格情報を配るな、動的に発行しろ。** 長命な資格情報を保存・配布しないこと。使う瞬間に短命資格を発行し、使用後に失効させる（Vault 動的シークレット / JIT）。**保存しないものは漏れない。rotation が不要になる。発行ログがそのまま監査になる。** 現実には2層：

- **層A（動的発行対応）**：SSH 署名証明書 / PKI / 一部 API。短命資格を JIT 発行。**保存ゼロ**。
- **層B（レガシー機器＝静的のみ）**：古い BMC / NW 機器。Vault 保存（`SecretRef`）＋ 定期 rotate で緩和。統一 interface の裏で自動振り分けする。

**identity は2入口、機器側アカウントと分離**：マシン（収束デーモンが Vault に自分を証明。段0 のトークン注入が起点、AppRole / TPM 等で強化）と、ヒューマン（IdP＝SSO で個人認証 → その身元で短命資格を発行。bastion 経由で経路を1本に絞りセッションを記録）。**機器には個人アカウントを永続作成しない**——機器側は使い捨て、身元は IdP / Vault が保持し、**発行ログが「本当は誰が」を記録する**（機器側は共通/使い捨てでも監査は個人単位）。

**可用性はエンジン以上・段0 で鶏卵解決**：broker は段0 の VM-HA 保護対象（Vault・reconciler と同列）で、可用性要件は最上位（§22）。段0 のトークン注入が起点となり、core が broker に依存し broker が段0 に依存する鶏卵は、**段0 が素の起動を保証する**ことで断つ（§23）。broker 断の緩和は Vault と対称：（a） 発行済み短命資格を core 側に TTL 内キャッシュ、（b） broker 不達中は該当リソースを `unknown` 扱い（absent と断じない）、（c） broker は段0 の VM-HA 保護対象。

**v1 における暫定**：credential-broker は v2 以降のため（§27）、v1（v1a/v1b）では provider のターゲット認証（DNS API トークン、vCenter 資格等）を **`SecretRef`（Vault 保存の静的資格）** で供給する。これは層B（静的資格＋定期 rotate）相当の暫定であり、broker 導入時に `CredentialProvider` 経路へ移行する。到達資格の `SecretRef` は **desired 本体には載せない**——**Executor が Apply/Read 実行時に `.Resolve()` する**（§20.2 の「Apply 直前のみ実値化」と同じ規律）。desired に載るのは「どの Vault パスの資格を使うか」という参照だけで、実値は実行時にのみメモリに出て使用後に消える。

**導入は段階的で、人 → デーモンの移行は remediation mode の移行に内包される。** broker は独立した導入イベントではなく、`remediation mode`（§14.1）の段階移行に自然に乗る——**段階1**は人が主で、デーモンは Observe ＋ drift 検知（`alert-only`）に留まり、人が短命資格で機器に入って直す。**段階3**はデーモンが全 Apply（`auto`）を担い、人は緊急時のみ。**仕組みは不変で、`CredentialProvider` の発行先分布が人 → デーモンへ移るだけ**である。したがって **まず作るべきは人向けの短命発行（bastion ＋ JIT）**——段階1で即座に実利があり（個人監査の要求がその時点で満たされる）、デーモン向け発行は同じ interface の発行先を変えるだけで後から乗る。この段階性は §24 のブラウンフィールド昇格（read-only → alert-only → auto）と同じ構造である。

### §20.5 資格局所性（WAN 断中の自律収束と爆発半径の両立）

エッジ / 店舗の claim（§17）が **WAN 断中も自律収束を回す**には、その店舗の機器へ到達する **write 資格が店舗ローカルに常駐**する必要がある。だが資格を常駐させると漏洩時の破壊力が問題になる——このトレードオフを**局所化で決着させる**。

- **店舗ローカル file `SecretBackend` に、その店舗の機器分の資格だけを常駐させる**（§2 seam 一覧の `file` 実装・§23 段0-minimal と同じ `SecretBackend` seam）。**中央の全店舗資格を配らない**——ある店舗のデーモンが持つのは当該店舗の機器資格のみで、**爆発半径は当該店舗に閉じる**（1 店舗の資格漏洩は 1 店舗にしか及ばない）。
- **WAN 断中も自律収束が回る**：店舗デーモンは局所資格で自店舗の機器へ Apply/Observe でき、中央 Vault・中央 broker への到達を要しない（§17 の S2-free シャードと組み合わせ、店舗は「pull した intent + 局所資格」だけで完結する）。
- **再接続時に中央からローテーション収束**：WAN 復帰時、中央が rotation Workflow（§20.1）で店舗ローカル資格を収束（rotate）する。常駐資格の鮮度は「常駐しているから古いまま」ではなく、**中央からの定期 rotate 収束で維持**される。
- **トレードオフの決着**：「資格常駐の破壊力（漏洩面）」vs「自己修復（WAN 断耐性）」は、資格を店舗単位に局所化することで両立する——常駐はするが**当該店舗にしか及ばない**局所常駐であり、鮮度は中央 rotate が維持する。これは §19.6 / §22 の「維持は衛星ゼロで回る」を資格面に適用した形である。broker（v2）導入後は、この局所 file `SecretBackend` が層B（静的資格＋定期 rotate）相当の暫定であり、broker の JIT 発行へ移行できる。

### §20.6 Break Glass（真の緊急時）

通常認証（Vault / IdP / 短命発行）が**全滅した状況**でも機器に入れる最終手段。ここには矛盾がある——Break Glass は認証基盤に依存できないため、消した「静的な共通資格情報」を意図的に1つ残す**必要悪**であり、ゼロにはできない。

**封じ込め原則（「使えるが、こっそりは使えない」）**：

- **オフライン物理保管**（Vault にもデーモンにも置かない）。**段0 の unseal key と同じ金庫・同じ性質 = 最後の人間の鍵**。
- **使用が必ず検知される**（封筒の改竄シール / 取り出しアラート）。**使い捨て・使用後 rotate**。最重要のものは**分割管理**（Shamir。Vault unseal と同型。単独犯行 / 単独ミスを防ぐ）。
- **事前チェック最小・事後監査最大**（緊急時は速度優先、抑止は確実な発覚で担保）。

**階層**：

| レベル | 何が死んだ | 手段 |
|---|---|---|
| L1 | reconciler のみ | 通常の bastion + 短命発行（Vault/IdP 生存 = Break Glass ではない） |
| L2 | Vault | オフライン保管の機器資格（rotate 済み最新を金庫に退避） |
| **L2.5** | **管理NW の到達性** | **OOB / コンソールサーバ（シリアル）経由**。ネットワークが死んでも機器に入れる = **ネットワーク版 Break Glass** |
| L3 | 段0 全滅 | 完全オフライン Break Glass + **段0 再構築**（Ansible playbook + unseal key） |

**L2.5 の注意**：管理NW が死ぬと credential-broker も機器に到達できない（`reconciler → credential-broker → 管理NW → 機器` の経路が切れる）= §20.4 の「broker 断 = 全盲」と同じ状態になる。したがって管理NW障害時は broker 断の緩和（TTL 内キャッシュ / `unknown` 扱い / alert-only 縮退）がそのまま効く。OOB は**この状態から人が復旧するための唯一の経路**であり、Sakutei の管理対象外に置く。**L3 は段0 のブートストラップと同一**（段0 を人手で再建する能力が、そのまま最終 Break Glass・§23・§25）。

**システム化**：

- **検出** = 取り出し / 開封イベントを既存の **Event → sink**（§22）に `BreakGlassUsed`（最高 severity）で流す。**使用は必ず検出される**。
- **rotate** = **自動 rotate は危険**（緊急対応中に資格が変わると対応者を締め出す）。**事態収束後に人が明示キック**する。中身は既存の rotation Workflow で、トリガが「手動」なだけ。
- **分担**：保管はオフライン（システム外）、更新 / rotate はシステム。

---

## §21 DesiredSource と catalog（図面）

理想状態（desired）はどこから来るか。git（人がレビューして書く platform 層）か、DB（テナントが即時に書く tenant 層）か、外部 CMDB との合成か。この供給源の違いを `DesiredSource` seam に隠し、収束ループは供給源を区別しない。catalog（図面）はその上に載る薄い UI である。

### §21.1 DesiredSource seam（選択制）

```go
type DesiredSource interface {
    Fetch(ctx) ([]Resource, intentRevision, error)
}
```

| 実装 | 保存 | 履歴 / 監査 | レビュー | 書込頻度 | 用途（ownership） | 版 |
|---|---|---|---|---|---|---|
| **FileSource（git）** | ファイル | git commit 連鎖 | PR | 低（人） | **platform 層** | v1 既定 |
| **DBSource** | DB | append-only ログ | 無（即時） | 高（API） | **tenant 層**・払い出し | v2 |
| **RenderedSource** | 外部 CMDB + intent | 元ソース依存 | render → PR | 中 | inventory 合成 | — |

「ファイルか DB か」は**保存形式の選択ではなく、必要な "正の性質"（履歴・レビュー・書込頻度）の選択**であり、それは `ownership`（§7）層と一致する。**ファイルには2役あった**：（1） 保存、（2） **git がくれる正の性質**（履歴・PR レビュー・`git diff`・単一書き込み経路）。DB にすると （1） は自明に代替できるが、（2） は**別途組む**必要がある（原因タグ A・レビューゲート・監査・爆発半径の土台が （2） に依存するため）。→ DBSource は **append-only 変更ログ**（監査）と **revision**（commit hash の代わりにトランザクション ID）を自前で持つ。

**書込経路と ownership の対応（platform=PR / tenant=即時）**：platform 層は PR 必須・厳格、tenant 層は即時書込・緩い（§7）。**混在の境界**：同一リソースを2つの供給源が持たない（**ownership が供給源の所属も決める**）。engine から見れば全部マージされた1つの理想状態で、収束ループは供給源を区別しない。

**RenderedSource の規律**：render の生成物は**手編集禁止**（`generated/` 配下に置き、冒頭に自動生成ヘッダを付す。生成物への直接コミットは弾く）。人が触るのは intent ファイルと外部 CMDB だけであり、これにより「生成物と intent の二重管理」が構造的に起こらない。

**上位テナント基盤は DesiredSource の一実装**：上位テナントプラットフォーム（ポータル・quota・課金）は `DesiredSource` の一実装として intent を投入する。**認可・quota は上位で適用済み**であり、Sakutei は認可を持たない（§3 第4原則）——投入された intent を収束するだけである。

### §21.2 Watch capability（早期収束のヒント供給・任意）

`DesiredSource` の任意 capability として `Watch` を置き、**早期収束パス（§8）の発火源**にする。`Provider` に対する `Lister`（prune の前提）と同じく**宣言制**であり、実装しない供給源では定期周期だけが動く（正しさは不変・遅延のみ）。

```go
// 任意 capability（実装は任意。Provider に対する Lister と同じ宣言制）
type Watcher interface {
    Watch(ctx) (<-chan ChangeHint, error)   // 早期収束のヒントを流す。真実は運ばない
}

type ChangeHint struct {
    IntentRevision string   // どの intent 版か（合流時に最新を読むための目印）
    Scopes         []Scope  // 触られた scope 群（これ "だけ" を運ぶ。値・状態は運ばない）
}
```

| 供給源 | `Watch` の実体 | 版 |
|---|---|---|
| **FileSource（git）** | git webhook / merge フック（PR マージ = ヒント発火） | v1a |
| **DBSource** | DB の書込通知（tenant セルフサービス API の即時反映） | v2（DBSource 自体が v2） |
| RenderedSource | 元ソースの変更通知（実装可能なら） | 元ソース依存 |

- **`Watch` が流すのは `ChangeHint`（intent_revision ＋ 触られた scope 群）だけ**。変更後の値・状態は運ばない——**ヒントは真実を運ばない**。コアはヒントを受けて**その scope を新鮮に render → Observe → Diff → Apply** する（観測 state レス無傷・§3）。
- **合流（coalesce）と最新読み**：同一 scope への複数ヒントは1回の scoped run に合流し、実行時に最新の desired を読む（§8）。これにより「古いヒントで古い状態を適用する」経路を型レベルで塞ぐ。
- **`Watch` seam は v1a で切る**（FileSource の webhook / merge フック）。DBSource 連動は v2。

### §21.3 Loader（設定形式）

**v1 は YAML / JSON 両対応**。同じ木を吐く別シリアライザとして `Loader` interface の下に置く（JSON = API の機械経路、YAML = 人の手編集）。**round-trip 保持は YAML のみ配慮**する。

- **スキーマ検証は JSON Schema を外付け**し、ロード時に検証（`pruneable` / `scope` / enum の必須化、循環検出）。YAML の型曖昧性（Norway 問題等）はスキーマで封じる（「version は文字列必須」等）。
- 参照（暗黙依存）が主役になったら **CUE** へ Loader を差し替える余地を残す（CUE は検証＋参照＋導出を言語内に持つ）。**Starlark は round-trip 不可**で API write と衝突するため採らない。
- **キー命名規約**：設定ファイル上のキーは **snake_case** を正とし、Loader が Go フィールド名（`DependsOn` 等）へ変換する。`depends_on` / `dependsOn` の混在は Loader が吸収する。

**理想状態の保持（platform 層 / ファイル正）**：正はファイル。起動時に読んでインメモリ保持し、通常運転は全部メモリ上で回す。API が書く場合はインメモリ更新＋ファイルへ write-through。**DB プロセスゼロ、単一バイナリを維持**でき、`git diff` で理想状態の変遷が読める。並行書き込みやクエリ性が実問題になった時点で embedded KV（bbolt / SQLite）へ `DesiredStore` interface 経由で移行する。**etcd（分散合意）は要らない**——etcd は「多書き手の合意」のためであって「永続化」のためではない。

### §21.4 catalog（図面）——薄い UI

**catalog（intent UI）は v2**（§27）。intent は既に YAML（git）で表現済みなので、**UI に独自のデータモデル / DB を持たせない**。UI は薄いフロントエンドであり、**git が真実の源**である。

- **書き込み経路は `ownership` に従う**：テナントのカタログ操作（「仮想ネットワーク追加」）→ 即時反映。管理者の基盤設定変更 → **UI → PR 起票 → 承認**。同じ UI でも触るリソースの層で裏の経路が変わる（§7）。
- 難所は UI ではなく **round-trip**（UI とテキスト手編集の2経路調停・コメント / 整形保持）。platform 層を **UI → PR 生成**にすることで、**git を単一の書き込み経路**に保ち往復問題を回避する。
- **カタログ UI の各商品 = 合成リソースのテンプレート**（§9）。UI → 合成リソース → allocation → git / 収束 が一本に繋がり、**UI が薄くできるのは下の仕組みが厚いから**である。

---

## §22 委譲と停止影響

作らないと決めたものと、衛星が止まったときに何が止まるか。この二つを表として確定させる。委譲の一覧は「なぜ作らないか」を、停止影響表は原則「衛星の停止は変化を止めるが維持を止めない」の具体化を担う。

### §22.1 外部委譲の一覧（作らないもの）

| 委譲先 | 委ねたもの | core の関与 | なぜ作らないか |
|---|---|---|---|
| **Vault** | シークレットの値・動的発行・rotation | `SecretRef`（§20）で参照。値を持たない | 値の diff は取れず、値を持てば漏洩面が広がる。値は Vault の責務 |
| **IdP（SSO）** | ヒューマン ID・認証 | broker 経由（§20.4）。core は認可を持たない | 人の認証は既存の企業 IdP が正。認可 / quota を自前に持たない（§3 第4原則） |
| **Prometheus / Alertmanager** | 時系列監視・通知ルーティング・沈黙 | **監視 "設定" を Resource として収束**（§22.2）。判定と通知は委譲 | 監視は成熟した専用系。judgement と routing を再発明しない |
| **DRS / ハイパーバイザー** | 配置最適化・ビンパッキング・migrate | cluster への intent と粗い容量充足のみ | **scheduler を再発明しない**。配置最適化は自前に持たない |
| **上位テナント基盤** | 認可・quota・課金・ポータル | `DesiredSource` の一実装（§21.1）として理想状態を投入 | 認可・quota は上位で適用済み。Sakutei は投入された intent を収束するだけ |
| **git（GitHub/GitLab）** | intent の履歴・レビュー・改竄耐性アンカー | `FileSource`（§21.1）の実体。監査の半分を担う | 履歴・PR レビュー・単一書込経路を git が既に完備。再実装しない |
| **k8s 等** | アプリ層のデプロイ・スケール | core は土台（インフラ）を供給 | アプリ層は k8s が正。Sakutei は k8s の下の土台に閉じる |
| **Ansible（モジュール）** | ロングテール provider の実行実体 | `Executor` の一実装。**モジュールのみ借り、実行モデルは借りない** | provider を全部自作できない。モジュール資産だけ借り、push 型実行モデルは借りない |

### §22.2 observability-as-Resource（監視設定を収束し、判定 / 通知は委譲）

「在る ≠ 動いている」。VM が「存在し電源 ON」でも中のアプリは応答しないかもしれない。この health は「差分ゼロ」なので収束（conformance）では捕まらない。だが prober loop を自作しない——**observability as Resource**：監視 "設定" を通常の Resource として収束する。

```yaml
vm:
  id: web-1
  observability:
    - blackbox: { probe: http, path: /healthz, port: 8080 }
    - alert: { name: web-down, for: 2m, severity: page }
```

これを通常の Resource として収束させ、**Prometheus の scrape target / blackbox / Alertmanager ルールを生成・維持**する。**判定と通知は Prometheus / Alertmanager に委譲**する（§22.1）。**リソース削除時は監視設定も prune される**ので、孤児 monitor が残らない（§12）。

### §22.3 Event 単一型と sink、収束失敗は level で報告

収束失敗 / Workflow 失敗 / drift / canary 失敗 / 振動を**個別に通知実装しない**。**単一の `Event` 型に一本化**し、通知は Event の consumer（sink）として分離する。**本システムはイベントの発生源であり、ルーティング・エスカレーション・沈黙は Alertmanager 等に委譲する**（§22.1）。

- **収束失敗は level（継続状態）として報告**する。level-triggered だからこそ通知も **firing / resolved** にでき、失敗継続 = 更新、回復 = resolved。**毎周同じアラートを撒く破綻を回避**できる。
- **Since / 連続失敗回数から transient ↔ stuck を自動昇格**（level-triggered が過去を毎周持っていることの無料の副産物。edge 型監視には出せない情報）。分類：**transient**（自己回復見込み）/ **stuck**（権限・欠損・容量 = 即 critical）/ **oscillating**（綱引き）/ **blocked**（依存未達 = 上流 Event に集約）。
- **Workflow / canary 失敗は edge（事件）**：停止位置・到達済み step・invariant 状態を1回通知。canary の cleanup 反復失敗のみ level に昇格する。

Break Glass の使用（`BreakGlassUsed`・§20.6）や監査アンカーの push（§26）も、この単一 Event → sink に流れる。発生源と sink を分けることで、通知系の交換が収束コアに波及しない。

### §22.4 衛星停止影響表（何を止め、何を止めないか）

原則「衛星の停止は変化を止めるが維持を止めない」を、各衛星について「**何を止め、何を止めないか**」の表として明示する。衛星の可用性要件の非対称（どれが最上位か）がこの表から直接読める。

> **衛星の停止は、変化を止めるが、維持を止めない。**
> 定常状態の収束ループは、衛星コールゼロで回る——これを設計保証にする。

| 衛星 | 停止が **止めるもの** | 停止が **止めないもの** | 縮退の接続先 |
|---|---|---|---|
| **allocator** | **新規採番が要るリソースの作成のみ**（`waiting-allocation`・§19.9） | 既存リソースの収束（Lookup キャッシュ・§19.6）/ Release（元々遅延可・§19.5） | §19.9 の waiting-allocation・早期収束ヒントのキュー保持 |
| **Vault** | **secret を持つ Apply のみ**（alert-only 縮退・§20.3） | Observe は継続（資格キャッシュ・§20.3）/ secret を持たない Apply | §20.3 の Vault 断緩和・last-resolved + TTL キャッシュ |
| **credential-broker（v2）** | **認証が要る全ターゲットの Observe = 全盲**（§20.4） | （broker は v2。v1 は SecretRef 直結で broker 依存なし・§20.4） | だから**可用性要件が最上位**（§20.4）。TTL 内キャッシュで一時緩和 |
| **catalog** | intent の新規投入 UI（人の書込 UX のみ） | 収束ループ全体（catalog は git の薄い UI・§21.4） | git 直接編集 / API へフォールバック |

- **非対称の要点**：allocator / Vault の停止は**部分的**（新規採番だけ / secret Apply だけ）に止まり、定常収束は回り続ける。catalog の停止は UI だけで、正である git は生き続ける。一方 **broker（v2）の停止だけは全盲**（Observe が全滅）であり、これが「broker 断 > Vault 断」という可用性要件の順位の根拠である（§20.4）。
- **v1（v1a/v1b）では broker が無い**（§27）ため、v1 の衛星停止影響は allocator と Vault の 2 行だけであり、どちらも**定常収束を止めない**。すなわち **v1 では「衛星がすべて落ちても、既存インフラの維持収束は回り続ける」**——これが本原則の v1 における具体的帰結である。
- この非対称は §19.6 の QPS 算術（維持は衛星ゼロで回る）・§20.3 / §20.5 の secret / 資格の局所キャッシュと同根であり、**衛星は「変化」の可用性のためにだけ HA を積む**（維持の可用性は観測 state レスと局所キャッシュが担保する）という投資判断を貫く。


