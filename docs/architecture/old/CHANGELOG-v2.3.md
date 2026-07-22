# Sakutei 設計書 v2.3 変更一覧(CHANGELOG)

対象:`sakutei-design-v2.2.md` → `sakutei-design-v2.3.md`。改訂日 2026-07-22。
本書はレビュー(`docs/review/`)の指摘を反映した設計変更の一覧である。後続の再評価エージェントが
「どの指摘に、どの章で、どう対処したか」を追えるようにする。v2.2 は歴史として保持し編集しない。

形式:**変更ID / 対応レビュー指摘 / 変更箇所(章)/ 変更内容の要約 / 設計判断の理由**。
末尾に**採用しなかった指摘**を見送り理由つきで列挙する。

凡例:C=01-consistency、S=02-safety、F=03-feasibility、X=review/README 横断発見。

---

## A. 整合性(01-consistency 高6件すべて)

### CHG-A1 — Provider interface の正準を §5.1 に一本化
- **対応**:C H-1、C H-2、C L-1、C総括(1)
- **章**:§4.1(【正準定義の所在】ボックス追加・コードブロック修正)、§5.1(Read 戻りを Observation に)、
  §4.4(Executor.List を []ID に)、§4.6(型表を更新)、§26.1 A-2
- **要約**:v2.2 で §4.1(`Observe`/`Diff` あり)と §5.1(`Read`/Diff なし)が食い違っていた Provider 署名を
  **§5.1 を正準**として統一。呼称は `Read`、Provider は `Diff` を持たない(コアが Schema 駆動で実装)、
  `Read` の戻りは `Observation`(Availability/State/Ready)。未定義だった `ObserveStatus` を廃止し
  `Observation.Availability` が役割を担う。Lister は `[]ID` に統一。
- **理由**:実装者が §4.1 をコピーすると誤った契約(Diff 実装・Ready を返せない署名)を書く。正準を1つに
  決め、もう一方を明示的に概念モデルへ降格するのが「決定事項のみを書く」方針に整合。§6.7 の依存解決が
  署名レベルで成立するよう Ready を通すことを優先した。

### CHG-A2 — allocation / credential-broker の v1 依存矛盾を解消
- **対応**:C H-3、C H-4、F H-5、C L-4
- **章**:§7.1(allocation 解決は v1b〜)、§7.5(非採番=固定値展開のみ v1a)、§3.4 seam 表、§14.5、
  §19.2(broker=v2 の明示)、§24.1(再スライス)、§16 冒頭、§13.4(catalog=v2)
- **要約**:v2.2 は allocation/broker を v2 としながら v1 の render/段0 が依存し実装不能だった。**決定**:
  (a) allocation 解決と採番連携展開は **v1b〜**、v1a のフェーズ2 は**非採番の固定値展開のみ**。
  (b) credential-broker は **v2**、v1 は `SecretRef` 直結(注入点は Executor 実行時 Resolve)。段0 図の
  broker 行を角括弧で v2 と明示。catalog(intent UI)は v2。
- **理由**:「v1 では該当機能を明示的に縮退させる」方を選択(最小形を無理に v1 に置くより、20 構成検討で
  価値実証に効く v1a を薄く保つ方が改善)。seam は §5「seam は今切る、split は後」に従い切っておく。

### CHG-A3 — §26 カテゴリ A に着手前決定を追加(型・スコープ矛盾の反映)
- **対応**:C H-5、C M-8、F M-6、S 通底1、B/D-2
- **章**:§26.1 A(A-1〜A-5 に再編)
- **要約**:A に **Provider 正準形(A-2)/ rendered_revision 算出定義(A-3・B から格上げ)/ S2 永続化先
  (A-4・B から格上げ)/ ready_condition 名前空間(A-5・B から格上げ)** を追加。`Plan`(破壊性フラグ+
  原因タグ必須)・`ID`(複合キー推奨)・`Scope`(4安全機構が乗る)を最優先に明記。
- **理由**:これらは「型を書き始めた瞬間に露呈し後から変えると全 provider に波及」する着手前事項でありながら
  v2.2 の A から漏れていた。安全機構の判定根拠(rendered_revision・S2・Plan)が未決に乗る「依存の逆立ち」を
  解消するため A へ格上げした。

### CHG-A4 — Release「下流削除を Observe で確認」を観測 state レス原則に接続
- **対応**:C H-6
- **章**:§16.4(a)
- **要約**:`Observation` の三値に absent が無く「削除完了」を Read で表せなかった問題を、**Release の削除
  完了確認を `Read` ではなく `List`(集合演算)で行う**と決定。Release は Lister を持つ下流にのみ許可し、
  §8.1 の集合差・§8.6 の List 完全性検証・§8.4 の削除判定3種と型レベルで接続。
- **理由**:個別 Read に第4値(absent)を足すと全 provider に波及する。集合演算は §8.1 で既に削除の基盤で
  あり、`unknown` との混同を構造的に避けられる。

### CHG-A5 — 細部の整合(規模・キー名・命名・集約順序・catalog)
- **対応**:C M-1、C M-2、C M-3、C M-4、C M-6、C L-3、C L-4
- **章**:§6.1(depends_on に統一 + Loader 変換注記)、§13.3(キー命名規約)、§7.5(集約順序に stuck/blocked)、
  §14.5(SecretRef 注入点明示)、§16 冒頭(allocator/AllocationAuthority/authority 三者定義)、§13.4(catalog=v2)
- **要約**:設定キーを snake_case(`depends_on`)へ統一し Loader 変換を明記。合成リソース上位ステータス
  集約順序に `stuck`/`blocked` を追加。到達資格 SecretRef の注入点(Executor 実行時 Resolve)を明示。
  allocator の三者関係を定義。
- **理由**:実装者の誤認(別キー・permanent 失敗の子が昇る先が無い等)を潰す文書内整合。

---

## B. 安全性(02-safety 防御不十分シナリオ上位)

### CHG-B1 — rate limit に累積時間窓予算を追加(漸進的全削除を塞ぐ)
- **対応**:S-01(高)、X-5
- **章**:§10.2(destructive_budget・halt-all エスカレーション・異常検知の凍結セマンティクス)
- **要約**:cycle 瞬間量に加え **24h 等の窓での破壊的変更総数予算**と **単一 scope の削除進捗率**を監視し、
  超過で `halt-all`(rendered_revision 全体を凍結し人へ)へエスカレーション。巨大/削除方向 Diff 検知時は
  「超過分保留(漸進継続)」でなく「approved-large-change が付くまで一切の破壊を保留」と明文化。
- **理由**:v2.2 の `halt-destructive-only` は「その周の超過分保留」で、毎周 10 件ずつ 50 周で全消しが通った。
  非対称原則に整合させ、意図の凍結(halt-all)を選べるようにした。累積カウンタは S2 永続化(再起動で穴が
  復活しないため)。

### CHG-B2 — List 完全性検証(ページング完全性の契約化+件数急減 halt)
- **対応**:S-04(高)、X-5
- **章**:§8.6(新設)、§5.3(List 契約に追記)
- **要約**:(a) Provider 契約にページング完走検証を明記(打ち切りは error)。(b) 前回 List 比の件数急減
  (既定 −50%)で prune 保留 + Event。(c) `ListStatus`(complete/partial/unknown)三値を導入し complete
  以外は prune を保守側に倒す。v1a に入れる。
- **理由**:`unknown≠absent` は Read(個別)の防御であり、prune の根拠である List(集合)のサイレント欠損
  (ページング打ち切り・レプリカ遅延)には無力だった。Read の三値と対称の防御を集合層にも張る。

### CHG-B3 — 段0 コールドスタート順序・Ceph SPOF・自己収束自傷・Vault 断緩和
- **対応**:S-10(高)、S-07(中)、F H-6
- **章**:§19.7(新設)、§19.2(コールドスタート注記)、§19.6(運用負荷の正直な再見積もり)
- **要約**:(1) 起動順序 `Vault unseal → broker healthy → reconciler` を ready_condition 相当で明文化、
  reconciler は依存 unready 時は Observe せず待機。(2) 統合 Ceph の SPOF を明記し障害ドメイン分散(別ラック/
  電源/空調)を人手運用要件へ。(3) reconciler が自分の broker 到達資格・Vault アドレスを収束対象から除外
  (管理NW の「触らない最小核」を段0 サービスへ拡張)。(4) TTL キャッシュ < unseal MTTR の不等号に対し
  段0 限定の Vault auto-unseal または Observe 専用長 TTL 別枠キャッシュを緩和策として提示。
- **理由**:VM-HA はノード障害には効くが、Ceph 全損・起動デッドロック・自己参照設定の自傷は射程外だった。
  段0 は自己運用の生命線であり、これらを確定しないと最初の障害で全盲になる。

### CHG-B4 — じわじわ型部分障害(二値でない障害)への横断言及
- **対応**:S 通底2、S-03
- **章**:§10.7(新設)、§7.6(desired 側 incomplete≠empty)
- **要約**:「瞬間・二値」の障害モデル(cycle 瞬間量・不達/到達・存在/unknown)を、desired 側(§7.6)・
  List 側(§8.6)・削除累積側(§10.2)に**対称に「急減/部分欠損を疑う」検知**を入れて横断的に塞ぐ方針を
  §10.7 に明文化。render 入力の incomplete≠empty(200 OK だが歯抜け → 前回保持側へ倒す)を §7.6 に新設。
- **理由**:現実の障害は「じわじわ・部分的」に起きる。S-01/S-03/S-04 が同じ形の穴を共有しており、対称的な
  「前回比の急変を疑う」検知が最も効く横断対策。

### CHG-B5 — 原因タグの per-resource 帰属・rendered_revision 決定性・version 埋め込み fail-closed
- **対応**:S-02(高)、S-09(中)
- **章**:§10.3(per-resource 帰属・rendered_revision 決定的ハッシュ・version 埋め込み必須契約・last-seen リング)
- **要約**:原因タグを Cycle グローバルでなく **per-resource-diff で確定**(入力ハッシュを intent/外部入力/
  provider version 由来に分解)。`rendered_revision` を decision-relevant な入力のみの決定的ハッシュと規定
  (TTL ヒット/ミスで揺れない)。version 埋め込みを **登録必須契約(fail-closed)**+ソースハッシュ自動算出で
  誤検知/検知漏れを両封じ。**軽量 last-seen リング**(S3・非永続)で「前周は X だった」を Event に添える。
- **理由**:安全機構の中核(rate limit 重み・監査帰属)が原因タグに乗るのに、A/E 混在時の帰属が未定義で
  E 由来の破壊が A に相乗りする穴があった。C は「最も危険」と自認しながら version 付け忘れが危険側に倒れて
  いた。last-seen リングは観測 state レスの看板を守りつつ運用者の「いつからズレた?」に答える(→ F H-4)。

### CHG-B6 — シャード/allocator の fencing・分割規律、悪意への追加ゲート、監査の即時アンカー
- **対応**:S-08(高)、S-11(高)、S-06(高)
- **章**:§12.2(単一 writer/割当表に fencing token)、§16.6(allocator state 逆引き再構築・1プール1権威の
  静的分割)、§10.2(platform 破壊的大量変更は A でも承認必須)、§17.5(監査の即時 push・間隔短縮)、
  §26.2 B(firmware 相の途中 death は人手ゲート)
- **要約**:platform シャード(単一 writer)と割当表に fencing token/世代番号を必須化(「合意は持たないが
  fencing は持つ」)。allocator は state 喪失時の現実逆引き再構築 + 1プール1権威の静的分割。悪意ある A タグ
  対策として platform の破壊的大量変更は承認必須。監査を WORM/外部即時 push で改竄窓を潰す。
- **理由**:shared-nothing + 「分散合意しない」は平常時は綺麗だが分断/侵害時の単一性保証を欠いていた。
  fencing は合意より軽く両立する。安全機構が「誤操作」前提で悪意を想定していなかった点を最小限補強。

---

## C. 実装可能性(03-feasibility 高6件)

### CHG-C1 — v1 スコープの再スライス(v1a / v1b / v1.5)
- **対応**:F H-1、F M-1、X-1、X-README 優先1
- **章**:§24.1(全面再編)、§24.2、§24.4、§9 冒頭(プリミティブ表にバージョン列)
- **要約**:v1 を **v1a(コア実証 ≈10 人月:コア型+収束ループ+安全機構+最小 SDK+Provider2〜3個+devenv)/
  v1b(import・Sync Mode・allocator 最小・採番連携・SDK フル・ready_condition・capacity gate)/
  v1.5(単純 Task + 最小 Workflow〈直列+不変条件+手動承認ゲート〉+ remediation schedule)** に分割。
  多相 Task 状態機械・自動段階ロールアウトは v2 のまま。
- **設計判断の理由**:20 構成の 12 が「事業の核」を Task/Workflow に依存させており、Task/Workflow の v2 送りが
  最頻の△理由だった。一方 v1 全部は 22.5 人月で MVP として過大。**「v1 を薄くしつつ手続きの最小形を前倒す」**
  が 20 構成検討の判定を最も改善する方向。基準は「後付けで全 provider/scope に波及=v1a」「無いと使い始め
  られない導入形態がある=v1b/v1.5」。多相・自動ロールアウトは重く需要が偏るため v2 に残した。

### CHG-C2 — スケール算術を実機レイテンシで書き直し+tier 別間引き最小形の前倒し
- **対応**:F H-2、X-6、F L-3
- **章**:§12.2(実機レイテンシ表・階級別天井・tier 別 Observe 頻度)
- **要約**:「200ms で数千〜1万」を撤回し、vCenter 0.8s / NETCONF 1.5s / CLI 4s / Ansible 6s の実機表と
  30s 間隔での上限台数(~200/~107/~40/~27)を明記。天井を provider 階級別(完全/標準=数千〜1万、
  Ansible/CLI=数百〜千)に読み替え。**tier 別 Observe 頻度の最小形を v1b の Observe スケジューラ seam** として
  前倒し(完全な間引き/watch は v2)。
- **理由**:泥臭い層(実装の8割)を主対象と宣言する以上、間引きは「破綻してから」では遅い。§6.2 の
  「毎周全部」は既定として維持し、頻度を tier で上書き可能にすることで型に余地を残す。

### CHG-C3 — Schema 設計支援の強化(lint 強制・golden path・訴求の正直化)
- **対応**:F H-3
- **章**:§5.2/§5.5(Schema lint をロード拒否レベルで強制)、§24.1a(最小 SDK で lint 強制)、
  §24.1b(golden path テンプレート+契約テスト)
- **要約**:Schema lint を「あると良い」から**ロード拒否レベルの強制**(content-address に identity 不可・
  List:true で identity 未指定を拒否・secret 平文 String 拒否・version 未宣言拒否)に格上げ。golden path
  雛形(REST CRUD / Ansible ラップ)を SDK 同梱。「4関数書くだけ」→「4関数+Schema、Schema が肝」に正直化。
- **理由**:Schema 誤りの爆発半径が大きい(別 VM を消す/資格が平文化)ことは設計書自身が §5.5 で認めて
  おり、普及の律速。過小訴求で入った作者が事故を起こす方がエコシステムに有害。

### CHG-C4 — cause tag B(drift)の「なぜ」追跡強化(last-seen + status デバッグ体験)
- **対応**:F H-4、F M-2、F M-5
- **章**:§10.3(軽量 last-seen リング)、§24.1a(`sakutei status` のデバッグ体験を v1a に明示)
- **要約**:直近 N 周の actual スナップショットを S3(非永続)にリング保持し drift 検知時に「前周は X だった」を
  Event に添える。`sakutei status` に原因タグ+直近スナップ差分+下流波及 D+provider 階級表示を1画面で。
- **理由**:B は「消去法(否定形)」でしか判定できず「いつからズレた?」に答えられなかった。永続化しない
  限り観測 state レスの看板は守れる(喪失しても数周鈍るだけ=S3 の定義通り安全側)。CHG-B5 と一体。

### CHG-C5 — render 2フェーズの決定性を着手前決定へ+段0 運用負荷の正直化+管理NW の v1 縮退
- **対応**:F H-5、F H-6、F M-4
- **章**:§7.5(カスタム関数の決定性を rendered_revision 定義へ)、§19.6(運用負荷の再見積もり)、
  §19.3(v1 は管理NWコアを alert-only 限定・commit-confirm 未実装は Apply 不可を Capabilities で強制)
- **要約**:テンプレートのカスタム関数の決定性担保を §26.1 A-3 に紐づけ(動かしながら決める項目ではない)。
  段0 運用を「年数回〜月1回は楽観的」と正直化。commit-confirm は v2 なので v1 は管理NWコアを収束対象に
  しない(観測のみ)。
- **理由**:決定性は原因タグ E の判定基盤。管理NWコアの自傷ロックアウトを commit-confirm 無しの v1 で
  起こさないため、型(Capabilities)でロックアウト経路を塞ぐ。

---

## D. 20 構成の横断的発見(review/README 横断発見 1〜7)

### CHG-D1 — 段0-minimal(1〜2 台構成向け縮退形)
- **対応**:X-2、構成 01/02
- **章**:§19.8(新設)、§3.4 seam 表(SecretBackend file)
- **要約**:単一ノード・file ベース SecretBackend・Vault なし・broker なしの **段0-minimal** を第一級で定義。
  可用性トレードオフ(単一障害点を許容するが観測 state レス+level-triggered で復帰後に自然再収束)を明示。
  v1a で成立。
- **理由**:3ノード+Ceph+Vault-HA 前提の段0 が 1〜2 台構成と物理的に衝突し不成立だった帯を開く。可視化・
  drift 検出器としての価値は 1〜2 台でも出る。

### CHG-D2 — 外部オーケストレータ境界の第一級化
- **対応**:X-3、構成 02/04/08/09/10/15/19
- **章**:§11.4(新設)
- **要約**:(a) `external_owner` による**外部所有 scope の除外宣言**(Observe はするが Apply しない。scope/
  種別単位の除外)。(b) 協調の seam=外部の収束結果を `ready_condition` として Observe で待つ一方向連携
  (Sakutei から外部へ指示は出さない)。
- **理由**:converging 3層+managed fields で「縁切り」はできるが、除外の明示概念と協調 seam が第一級で
  無く、二重管理事故の主因になりうる。協調を「観測して待つ」に閉じることで外部の内部ループに介入しない。

### CHG-D3 — remediation schedule(時間帯連動の mode 自動切替)
- **対応**:X-4、構成 03/15/17
- **章**:§10.8(新設)、§24.1.5(v1.5)
- **要約**:`remediation.schedule` で**保守窓のみ auto、他は alert-only** を宣言的に表現。freeze(今すぐ)と
  schedule(時間帯で張る)は重ねられる(freeze 最優先)。v1.5。
- **理由**:v2.2 は手動 freeze/unfreeze しかなく SLA 環境でヒューマンエラー面が残った。level-triggered なので
  窓が開いた周に自然修復される。

### CHG-D4 — 観測不能資産の「階級」の型定義
- **対応**:X-7、構成 14/18、C 18(観測不能機器の階級が型として未定義)
- **章**:§23.9(新設)
- **要約**:`observability: none` + `ledger_only: true` + `last_verified` で、Observe すら不可の資産を台帳上
  `unknown` のまま第一級で扱う。Read/Apply 対象外、closed-world List 集合演算から除外(prune 巻き添え防止)、
  時間ベースの人手検証を health 述語に。v1a で型、運用は v1b。
- **理由**:`unknown≠absent` の台帳版。ブラウンフィールドに必ず存在する観測不能資産を型で持たないと prune
  巻き添えや unknown 運用負荷の積み上がりになる。

---

## 採用しなかった / 全面採用しなかった指摘(見送り理由つき)

- **S-11 提案1「broker 側で reconciler を半信頼扱い(per-target 最小権限・要求レート異常検知)」**:
  broker は v2 のため、broker 内部設計は v2 に属する。v2.3 では reconciler 側の追加ゲート(platform 破壊的
  大量変更の承認必須・§10.2)と監査の即時アンカー(§17.5)のみ先取り。**見送り理由**:v1 に broker が
  無く、実装対象外の内部設計を先に固めても検証できない(§5「split は後」)。§26.4 D の再検討トリガに委ねる。

- **S-05(converging Tier2 の適応的 settle 窓)**:レビュー自身が「防御は概ね効く(非カウント)」と判定。
  **見送り理由**:Tier3 の auto alert-only 隔離が最終的に受け止めるため致命化しない。settle 窓の値は
  §26.3 C(運用パラメータ・動かしながら決める)に留める。推奨(Tier1 信号優先・適応延長)は本文に
  反映済みだが必須化しない。

- **F M-7(System Initiative との差別化強化)**:**見送り理由**:これは製品ポジショニングの助言であり
  設計変更ではない。差別化の一本目を「観測 state レス×オンプレ物理まで」に絞る、で足りる(§1.2 が既に
  この立ち位置を述べている)。「なぜ変わるか」で SI と正面勝負しない方針は CHG-C4 の last-seen で必要十分。

- **F L-1(`apply → reconcile` エイリアス)/ F L-2(image ハッシュ更新フロー)/ F L-4(twin を v1 層A/B に
  明示)**:低重大度の運用親切機能。**見送り理由**:L-2/L-4 は v2 の運用フロー(image 自動反映・twin フル
  機能は §22.5 で既に v2)に属し、本改訂の焦点(高重大度)から外れる。L-1 のエイリアスは実装時の小事で
  設計判断を要さない。twin の v1 縮退は §24.2 の「twin フル機能=v2」で既に含意される。

- **C L-2(firmware target_version の例示値差 24.2R1 vs v2.1)**:レビュー自身が「対応不要・無害」と判定。
  **見送り理由**:参照実装側の具体値であり §1.5 の方針(具体値は本書から抽象化)に沿う。

- **C L-5(段0 図の §3.1/§19.2 相互参照)/ 旧稿 v1 の archive 退避(C 総括)**:文書運用の助言。
  **見送り理由**:§26.5 E(本書と参照実装文書の分割・旧稿退避)に既に運営課題として計上済み。設計本文の
  変更ではなくリポジトリ運用で対処する。

- **F M-3(昇格基準を managed 除外後の diff ゼロに)**:**部分採用**。managed 候補の自動サジェストと昇格
  基準の緩和は §23.3 に反映済み。全面的な Sync Mode との commit 間引き共通化の実装詳細は v1b の実装事項と
  し、設計本文では方針提示に留めた。

---

## 波及確認(v1 スコープ変更が各章の「v1では〜」記述に及ぶ点)

- §7.1/§7.5(render 採番=v1b)、§14.5(broker=v2・SecretRef 直結)、§16 冒頭/§24.1b(allocator=v1b)、
  §19.2(broker=v2 の段0 図)、§19.3(管理NW=v1 alert-only)、§9 冒頭(プリミティブのバージョン列)、
  §12.2(間引き最小形=v1b)、§24.4(最初の一歩=v1a のゴール)を相互に整合させた。
- §26.1 A の格上げ(rendered_revision・S2・ready_condition・Provider 正準)と §24.1a の「第0スプリントで
  A を確定」を接続。
