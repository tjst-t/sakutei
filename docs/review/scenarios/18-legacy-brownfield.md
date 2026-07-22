# 対象構成18: ブラウンフィールド——15年物レガシーDC Sakutei 適用可能性レポート

**総合判定:可視化道具として ○ 条件付き成立 / 収束エンジンとして △ 大幅な拡張が必要**

Sakutei の brownfield 戦略(§23:import → alert-only → scope 単位昇格)と Sync Mode(§23.8)は、
まさにこの「3年かけて可視化→整理→移行」プロジェクトの第一段階を狙って書かれており、思想的な適合は高い。
だが本構成の本体は「**Observe すらできない機器**(telnet only / EOL API / Solaris / NetScreen)」であり、
Sakutei の全価値は Observe/List capability の有無に乗っている。したがって可視化道具としては条件付きで成立するが、
収束エンジンとしてはレガシー中核に永久に届かず、道具の役割を明確に限定する必要がある。

---

## 1. 構成の前提整理

- **雑多な機器の堆積**:VMware 5.5(EOL, vCenter含む)+ ESXi 6.7 混在120ホスト、物理80台(RHEL5〜9 / 一部 Solaris)、
  旧 Catalyst(SSH 不可・telnet のみの個体あり)、Brocade FC、NetScreen〜PaloAlto まで FW 世代混在。
- **ドキュメント・知識の欠落**:構成を知る人は退職済み。「触ると壊れる」サーバが複数。IP 台帳は Excel(2年前更新)。
  誰も全容を把握していない。これは Sakutei が最も価値を出せる状況(§23.2「実態を暴くことが最初の価値提供」)。
- **プロジェクトの位置づけ**:Sakutei は第一段階「可視化(何があるか・どこがドリフトしているか)」の道具として検討。
  収束(自動修復)は当初の目的ではない。この分離が判定の核になる。
- **運用体制**:情シス + 複数ベンダの寄り合い。単一の権威的オーナーが存在しない = ownership/git 単一書き込み経路の
  前提が組織的に崩れやすい。

**設計との対応**:本構成は §23 全体・特に §23.2(alert-only 強制)・§23.7(既存中核は最後、永久に alert-only 可)・
§23.8(Sync Mode)・§21.3(Ansible provider の縮退)・§4.3(Capabilities による機械的縮退)の**限界ケースの実地試験**である。

---

## 2. リソースモデリング:「Observeできる範囲」の層別整理

Sakutei の全機能は「現実を機械的に読めるか(Read)」「集合を網羅列挙できるか(List)」に依存する。本構成を
**観測可能性で層別**すると、Provider の階級(§5.4)がそのまま決まる。

| 層 | 機器 | Observe 手段 | 到達可否 | Provider 階級 | prune/Sync |
|---|---|---|---|---|---|
| **L0 API 良** | ESXi 6.7 / vCenter(限定) | govmomi | ○ | 標準〜完全 | 可 |
| **L1 API 難** | VMware 5.5(EOL, API 廃止/非対応) | 旧 SDK・部分的 | △ | 最小 | 不可寄り |
| **L2 SSH/OS** | RHEL 7〜9 | SSH + Ansible `*_info` | ○ | 最小(部分昇格) | 個別(open-world) |
| **L3 レガシー OS** | RHEL5/6 / Solaris | SSH(鍵・暗号方式が古い) | △ | 最小 | open-world のみ |
| **L4 CLI scrape** | Catalyst(SSH 可) | CLI scrape → 構造化 | △ | 最小 | 不可 |
| **L5 観測不能** | Catalyst(telnet only) / NetScreen / 触ると壊れる機器 | — | ✗ | 表現不能 | — |

**最小クラス Provider と Ansible Provider(長尾)の使い分け**:
- L0/L1 の VMware 系は数が多く横断項目(managed fields)が効くので**自前コア provider**(vsphere)寄り。ただし 5.5 は
  API が枯れており最小階級に落ちる。
- L2〜L4 の雑多な OS・旧 NW は典型的なロングテールで **Ansible Provider(§21.1/21.2)**が担当。`Capabilities.TrueObserve=false`
  により Diff・原因タグ A/B・converging 検知・破壊的重み付けが軒並み縮退する(§21.3)。`*_info` 系モジュールで観測できる
  対象は `TrueObserve:true` に**部分昇格**(§4.3)できるが、Solaris・旧 Catalyst では module 自体が乏しく昇格しづらい。

**観測不能機器(L5)の表現方法**——ここが本構成の核心:
- telnet only / EOL API / 触ると壊れる機器は **Read が実装できない**。Sakutei の型上、Read が返せないと Observation を
  出せず、Availability は常に `unknown` になる。§2-3 の非対称原則により **unknown ≠ absent** なので、勝手に削除・
  修復対象にはならない(安全側)。
- **unknown のまま「台帳に載せる」価値**:設計は「観測できないものを Sakutei の理想状態に載せる」機構を正式には持たない。
  現実解は、これらを **手動 inventory エントリ(§16.2 の inventory=事実の記録)としてファイルに記述し、Sakutei には
  「到達不能・観測対象外」とマーキングした placeholder Resource として置く**こと。収束はしないが、「何があるかの台帳」に
  存在は残る。Excel 台帳の電子化・正典化(§4 で inventory を git 管理)という意味で価値がある。ただしこれは Sakutei が
  Observe した事実ではなく人間入力であり、**drift 検知は一切効かない**(誰かが telnet で変えても Sakutei は気づけない)。
  → **設計ギャップ**:Sakutei には「観測不能を型として明示的に扱う階級」(Read も List も無い、存在だけ人間が主張する
  リソース)が無い。現状は placeholder + inventory ファイルの運用回避になる(§7 参照)。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities(TrueObserve/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `vsphere_vm`(6.7) | ESXi 6.7 上 VM | ✓/✓/△/✗ | 標準 | vm名+cluster。UUID/current_host は managed/computed | 中 |
| `vsphere_legacy`(5.5) | VMware 5.5 上 VM(EOL) | △/△/✗/✗ | 最小 | 同上。API 制約で State 部分的 | 高(EOL SDK) |
| `ansible_rhel`(7〜9) | RHEL 7〜9 パッケージ/設定 | △(info系のみ)/✗/✗/✗ | 最小 | hostname or トポロジ座標 | 低〜中 |
| `ansible_legacy_os`(RHEL5/6/Solaris) | 旧 OS | ✗/✗/✗/✗ | 最小 | hostname | 中(古い SSH・module 不足) |
| `catalyst_cli`(SSH 可) | 旧 Catalyst config | ✗(scrape)/✗/✗/△ | 最小 | 物理トポロジ座標(§5.5) | 高(CLI scrape の脆さ) |
| `fw_panos`(PaloAlto) | PaloAlto ルール(順序付き) | ✓/✓/△/✓(commit) | 完全 | ルールセット(ordered) | 中 |
| `fw_legacy`(NetScreen/telnet Catalyst) | 観測不能 FW/SW | ✗/✗/✗/✗ | **表現不能**(placeholder) | 人間入力の inventory キー | — |
| `inventory_file` | Excel 由来の台帳(観測不能機器含む) | read only(人間入力) | inventory ソース(§16.2) | 台帳キー | 低(移行工数は別) |

---

## 3. ownership・DesiredSource・sharding設計

- **ownership はほぼ全て `platform`**(§12.1)。テナントセルフサービスは存在せず、共有基盤の集合体だから。
  したがって書き込み経路は **PR 必須・remediation は alert-only 寄り・rate limit は厳格**が既定になる。tenant 層は当面不要。
- **DesiredSource は FileSource(git)一択**(§13.1)。DBSource(即時・API)は tenant 層の道具で本構成には不要。
  ただし「複数ベンダ寄り合い」で **git 単一書き込み経路を組織的に守れるか**が最大の非技術リスク(§7 で詳述)。
- **sharding は当面不要**(§12.2)。規模(120+80+NW/FW = 数百〜せいぜい数千リソース)は単一デーモンの天井内。
  将来サイト/世代別に分けたくなったら scope=site/vendor で水平分割できるが、v1 では過剰。
- **scope 設計が本構成の主戦場**:§23.4 の段階的昇格は scope 単位で進む。ここでは
  `scope: {vendor: vmware67}` / `{os: rhel9}` / `{fw: paloalto}` のように**「観測可能性が揃う束」で scope を切る**のが正解。
  観測不能機器を混ぜた scope は永久に昇格できないため、L5 は最初から別 scope(observe-none)に隔離する。

---

## 4. render(inventory/allocation/secret)の使い方

- **inventory(§7.1 フェーズ1)= 本構成の主役**。Excel 台帳からの移行パスは2経路:
  1. **観測可能機器(L0〜L4)**:`sakutei import`(§23.3)で Observe した現実を YAML に書き出す。Excel を信じず、
     **現実を正典にする**。Terraform import と違い観測 state を持たないので「state と現実のズレ」が原理的に発生しない(§23.3)。
  2. **観測不能機器(L5)**:import できないので、**Excel を人手で inventory ファイル(git)へ移送**し placeholder として置く。
     これは移行工数がかかる人力作業で、Sakutei は「入れ物(正典化された台帳と履歴)」を提供するだけ。
- **allocation**:IP 台帳(Excel)を allocator(§16)へ載せる価値はあるが、**既存の使用中 IP を authority に seed する**
  作業が前提。既存環境では「誰が何番を使っているか」が不確かなので、authority を正にする前に import で現実の IP 使用を
  観測して突合する必要がある。当面は allocator を導入せず inventory として読むだけでも成立する。
- **secret(§14)**:機器の初期パスワードは §14.3「既存・手動登録」に該当。段0 admin が Vault に1回登録し、以後 rotation の
  系に合流させるのが理想だが、**telnet only / EOL 機器は rotation Workflow が動かせない**(自動接続手段が無い)ため、
  層B(静的資格 + 手動 rotate)にすら乗らず、事実上 Break Glass 相当のオフライン保管に近い扱いになる。

---

## 5. 安全機構の適用(「触ると壊れる」環境)

- **import / alert-only の安全性(§23.1/23.2)**:import は Observe(読み取り)のみで **Apply 経路を触らない**。
  alert-only は**爆発半径ゼロ**(§10.2)。したがって「触ると壊れる」環境でも、**観測できる機器なら**読むだけで
  安全に実態を可視化できる。ここは設計が明確に効く。
- **観測不能機器の安全性**:L5 は Read も Apply も走らないので Sakutei が触ることは物理的にない。安全は保たれるが、
  それは「管理できている」からではなく「一切関与できていない」から。**安全と無力は表裏**。
- **alert-only 永続の妥当性(§23.7-4)**:設計は「既存の中核は永久に alert-only のままでも構わない」と明言している。
  本構成のレガシー中核(VMware 5.5・旧 NW・触ると壊れるサーバ)は、まさに**永久 alert-only の正当な適用対象**。
  これは逃げではなく設計思想通りの運用。
- **Sync Mode の限界ケース(§23.8)**:Sync Mode は「現実→git へ片方向反映」で人手運用と共存しながら履歴を貯める強力な
  仕組みだが、**`Capabilities.List` を要求する**(§23.8 末尾)。List できない L4/L5 機器は Sync Mode の対象にできない。
  つまり「継続的に現実を git へ記録し続ける」価値は、List 可能な層(vSphere・PaloAlto・一部 OS)にしか届かない。
  旧 Catalyst の config drift を Sync Mode で追う、はできない。
- **誤って auto にした場合の被害想定**:level-triggered は「速く確実に収束する能力=速く確実に破壊する能力の裏返し」(§10.1)。
  この環境で不完全な理想 × auto × prune を有効にすると、**Excel 由来の不完全な台帳に書かれていない本番 VM/設定を
  「余分」と判定して一斉削除**しうる(§23.1 の最大の地雷)。「触ると壊れる」サーバを Sakutei が理想と違うと判断して
  再構成すれば即障害。**緩和は多層**:(a) 導入初期は prune 全面禁止・`pruneable:false` 既定(§23.1)、(b) rate limit の
  `halt-destructive-only`(§10.2)、(c) グローバル freeze を1コマンドで(§23.6)、(d) 制御面を止めてもデータ面は動く(§19)。
  これらが揃って初めて「事故っても止まる」が成立する。**auto への昇格は §23.4 の N 日間 diff ゼロを厳守する**こと。

---

## 6. 段0ブートストラップの当てはめ(レガシー環境の中に新規の段0)

- 本構成には Sakutei の段0(Proxmox 3ノード + Ceph + Vault + broker + reconciler、§19.2)が**存在しない**。
  レガシー DC の中に**新規で段0を建てる**必要がある。これは §19.1「段0は自分の外側から来る=Ansible で人手構築」の定石通りで、
  レガシー環境だからといって特別な障害はない(むしろ新規の島として綺麗に切れる)。
- **管理NW到達性の最小核(§19.3)**は要注意。レガシー DC の管理 NW は旧 Catalyst(telnet)で構成されている可能性が高く、
  段0 の reconciler ホストがそこに収容されると「段0 が触れない管理 NW に依存する」構図になる。**段0 用の管理到達性は
  可能な限り新規機器で独立させる**か、既存 NW を触らず OOB/コンソールサーバ(§15.6 L2.5)を確保するのが安全。
- **secret の根**:機器初期パスワードは §14.3 の「既存・手動登録」で段0 admin が Vault に投入。ただし §4 で述べた通り
  L5 機器の資格は自動 rotation に乗らず、オフライン金庫寄りの扱いになる。
- 段0 は「作って忘れる」不変基盤として建てられ、レガシー本番(段1相当)を alert-only で観測する。**この構図自体は成立する。**

---

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(最重要)

1. **観測不能機器(L5)は Sakutei の射程外**。telnet only / EOL API / Solaris / 触ると壊れる機器は Read/List が実装できず、
   Sakutei の中核価値(観測 state レスの drift 検知)が**構造的にゼロ**。設計は Ansible 部分昇格(§4.3)で縮退を吸収するが、
   縮退の底は「Apply + changed 真偽」であって、**それすら叩けない機器には縮退の受け皿が無い**。可視化ですら不能。
   → **設計ギャップ**:「観測不能を明示的に型で扱う階級」(Read/List 無し、人間が存在だけ主張する inventory-only リソース)が
   未定義。placeholder + inventory ファイルの運用回避しかない。3年プロジェクトで L5 は**Sakutei ではなく人力棚卸し**の対象。

2. **CLI scrape の脆さ(L4)**。旧 Catalyst の SSH scrape provider は、機種・IOS バージョン・プロンプト差で壊れやすく、
   `TrueObserve` が名目上 true でも実質信頼できない。Diff 品質が低く、原因タグも縮退する。**「観測できた」と
   「正しく観測できた」の乖離**が事故要因になる。ここは Sakutei の保証群がほぼ効かない層。

3. **組織リスク(§23.5 の実務課題)が技術より重い**。複数ベンダ寄り合いで **git 単一書き込み経路・PR レビュー統制**を
   維持する統治が無い。誰かが手で変えれば alert-only は drift を報告するが、「その報告を見て git を更新する/戻す」判断を
   下す責任主体が曖昧だと、alert-only の価値(§23.5)も回らない。**Sakutei が正しく動いても組織が受け入れなければ導入は失敗**
   (§23.5 明言)。これは拡張では解けない、プロジェクト設計の問題。

4. **inventory 起点の信頼性**。Excel 台帳(2年前)は render フェーズ1 の inventory ソースとして腐っている。import で現実を
   正典化する(§23.3)ことで観測可能機器は救えるが、**観測不能機器の台帳は永久に人間入力のまま**で、Sakutei は正しさを
   保証できない。「何があるか誰も知らない」問題のうち、Sakutei が自動で解けるのは L0〜L3 だけ。

**3年プロジェクトの各段階で Sakutei が担える範囲(忖度なし)**:

| 段階 | やること | Sakutei が担える範囲 |
|---|---|---|
| **第1年:可視化** | 何があるか・drift しているか | **L0〜L3 は◎**(import + alert-only で実態レポート)。L4 は△(scrape の信頼性次第)。**L5 は✗**(人力棚卸し) |
| **第2年:整理** | managed fields 切り分け・不要資産の廃棄判断 | Sync Mode で List 可能層の変化を git 履歴化。cutover レビュー(§23.8)で intent を整理。**削除判断は人**(prune は最後) |
| **第3年:新基盤移行** | 新クラスタへ移行 | **新規構築部分は greenfield として最初から auto**(§23.7-3)が最も価値が出る。既存中核は alert-only のまま置き去りにして廃止 |

→ Sakutei の真価は「レガシーを収束管理する」ことではなく、「**レガシーの実態を暴き(第1年)、新基盤を最初から正しく作る
(第3年)**」ことにある。中間のレガシー中核は永久 alert-only か、Sakutei 対象外のまま廃止されるのが現実的な着地。

---

## 8. 総合判定

**可視化道具として:○ 条件付き成立。**
- 条件:(a) 対象を Observe/List 可能な層(L0〜L3、可能なら L4)に限定する、(b) import → alert-only を厳守し当面 Apply しない、
  (c) 観測不能機器(L5)は Sakutei の射程外と割り切り、inventory placeholder + 人力棚卸しで扱う、(d) 複数ベンダ間で
  git 単一書き込み経路と「drift 報告を捌く責任主体」を組織的に確立する。
- 根拠:§23.2/23.3/23.7 が本構成のためにあるかのように整合し、「誰も全容を知らない」環境の**実態暴き**は Sakutei が
  最も価値を出す場面。爆発半径ゼロの alert-only で安全に始められる。

**収束エンジンとして:△ 大幅な拡張が必要(レガシー中核には事実上不成立)。**
- L5 を扱う「観測不能を明示する階級」が未定義であり、CLI scrape 層の信頼性も低い。レガシー中核の自動収束は
  設計思想上も「永久 alert-only 可」とされており、**収束させないことが正しい運用**。収束の価値は第3年の新基盤
  (greenfield 部分)でのみ本格的に発現する。既存中核を auto 化しようとすると §23.1 の全削除事故リスクが跳ね上がる。

**結論**:Sakutei は本構成に対し「第一段階の可視化道具」としては条件付きで成立し、投入する価値がある。ただし
「レガシー全体の収束エンジン」としては成立せず、それを目標に据えるべきでない。3年プロジェクトにおける Sakutei の
正しい役割は、**第1年の可視化と第3年の新基盤 greenfield 化**であり、レガシー中核は観測できる範囲だけ alert-only で
台帳化し、残りは人力棚卸しの上で廃止へ送る——という割り切りが成立条件そのものである。
