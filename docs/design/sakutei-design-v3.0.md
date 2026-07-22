# Sakutei（作庭）設計書 v3.0

**改訂日:2026-07-22**

## 本書の位置づけ

- 本書は Sakutei の**設計の正（source of truth）**である。設計に関する記述は本書を正とし、旧版・サンプルとの矛盾は本書に寄せる。
- 本書は **v2.6 と内容（設計判断）が等価**の**全面再構成**である。新しい設計判断は導入していない——v2.2 から v2.6 に至るまでのパッチ的改訂（逐次追記）で見通しが崩れかけていた構成を、確定した八部構成へ織り込み直した再編集である。
- したがって v2.2〜v2.6 の各改訂で「何を・なぜ変えたか」の履歴は、本書ではなく各 `CHANGELOG-v2.x.md` と旧版ファイル（`sakutei-design-v2.2.md`〜`sakutei-design-v2.6.md`）を参照する。v2.6 → v3.0 の再構成で行った軽微な調整（重複の圧縮・クロス参照の修正・体裁統一）は `CHANGELOG-v3.0.md` に一覧する。
- 旧版は歴史として保持し、編集しない。

---

## 全体目次

**第I部 思想と境界**
- §1 課題認識と位置づけ
  - §1.1 何が問題か / §1.2 立ち位置 / §1.3 level-triggered + 観測 state レスの定義と、それが構造的に排除するもの / §1.4 名の由来 / §1.5 CLI が体現する位置づけ / §1.6 対象領域
- §2 作るもの・委譲するもの・捨てるもの
  - §2.1 システム全体像 / §2.2 外部委譲（作らないもの） / §2.3 k8s から捨てたもの / §2.4 seam（拡張と分離の全接点）
- §3 八つの原則と適用境界
  - §3.1 level-triggered + 観測 state レス / §3.2 いつ = 収束 / どう = 手続き / §3.3 確信が持てないとき、加算はしても減算はしない / §3.4 持たない・委譲する / §3.5 seam は今切る、split は後 / §3.6 ownership がすべてを貫く / §3.7 シャード間協調は現実を介して行う / §3.8 衛星の停止は変化を止めるが維持を止めない / §3.9 適用境界

**第II部 中核モデル**
- §4 正準型と状態分類
  - §4.1 ID / §4.2 Scope / §4.3 State / §4.4 Plan / §4.5 Observation / §4.6 ChangeHint / §4.7 Event / §4.8 SecretRef / §4.9 状態分類 S1〜S4
- §5 Provider 抽象
  - §5.1 Provider interface 正準形 / §5.2 Schema 属性 / §5.3 Capabilities / §5.4 Lister と各関数の契約 / §5.5 エラー分類 / §5.6 プロバイダの階級 / §5.7 BulkObserve / §5.8 観測不能階級 / §5.9 Executor seam
- §6 3プリミティブ
  - §6.1 Resource / §6.2 Task / §6.3 Workflow / §6.4 承認ゲート / §6.5 RequiresTaskEvent
- §7 所有と境界
  - §7.1 ownership 2層が貫く7つの関心事 / §7.2 managed fields / §7.3 external_owner / §7.4 scope + managed による境界宣言 / §7.5 合成リソースの子への ownership 伝播

**第III部 収束機構**
- §8 収束ループ
  - §8.1 周期ループの全景 / §8.2 モデルA:毎周・全 DAG の採用 / §8.3 wave は Cycle 内の並列単位 / §8.4 非同期 Apply と in-flight 集合 / §8.5 per-resource backoff / §8.6 早期収束パス（トリガ収束）
- §9 render パイプライン
  - §9.1 二フェーズ / §9.2 外部権威到達不能時は前回解決値を保持 / §9.3 TTL キャッシュと平常時の頻度規律 / §9.4 intent_revision と rendered_revision / §9.5 合成リソースは実子リソースに展開する / §9.6 generated/ 規約 / §9.7 ステータス集約（最悪優先）
- §10 差分と原因
  - §10.1 Observation と ConvergenceStatus の型分離 / §10.2 変更の原因区別（A〜E） / §10.3 原因タグは per-resource で帰属する / §10.4 provider_version の diff 埋め込みと fail-closed / §10.5 last-seen リング / §10.6 content-address 参照
- §11 多周期収束
  - §11.1 converging の三層検知 / §11.2 ready_condition / §11.3 oscillation 検出 / §11.4 capacity gate

**第IV部 安全機構**
- §12 削除の規律
  - §12.1 削除は集合演算であり、境界の宣言を要する / §12.2 DeletionPolicy / §12.3 pruneable / §12.4 List 完全性の検証 / §12.5 Release の非対称 / §12.6 final_backup と削除 Workflow 推奨形
- §13 変更予算
  - §13.1 rate limit / §13.2 累積時間窓予算 / §13.3 planned_change 宣言 / §13.4 capacity gate
- §14 修復の統制
  - §14.1 remediation mode / §14.2 remediation schedule / §14.3 優先規則と freeze / §14.4 承認ゲート / §14.5 運用プロファイル / §14.6 ブラウンフィールドの alert-only 常用
- §15 隔離と防御
  - §15.1 blast radius 3軸 / §15.2 fencing token / §15.3 oscillation の自動隔離 / §15.4 シャード = 爆発半径境界 / §15.5 デフォルト halt、明示的意図で進む

**第V部 スケールと分散**
- §16 単一デーモンの天井
  - §16.1 天井は Observe 時間で決まる / §16.2 実機レイテンシ階級表と上限台数の算術 / §16.3 BulkObserve による書き換え / §16.4 バルク非対応層の非対称 / §16.5 tier 別 Observe 頻度 / §16.6 watch 併用ハイブリッド / §16.7 スケール段階表
- §17 claim シャーディング
  - §17.1 claim-as-intent / §17.2 shared-nothing / §17.3 ownership 境界 = シャード物理境界 / §17.4 git 配布規約 / §17.5 S2-free シャード / §17.6 再シャード / §17.7 所有権ハンドオフ / §17.8 スライス
- §18 分散の原則と将来
  - §18.1 原則:シャード間協調は現実を介して行う / §18.2 放棄するもの / §18.3 共有物は衛星に外出し済み / §18.4 v2 に残るもの / §18.5 メタ収束器を作らない

**第VI部 衛星と外部世界**
- §19 allocator（台帳）
  - §19.1 払い出しは第3のカテゴリ / §19.2 authority の3役 / §19.3 要求と割当事実の interface / §19.4 仮予約 / §19.5 Release は非対称原則に従う / §19.6 QPS 算術 / §19.7 state 喪失と分断 / §19.8 HA 最小形とプール分割 by claim / §19.9 waiting-allocation 縮退と実装の切替
- §20 secret と資格
  - §20.1 値は収束しない / §20.2 SecretRef / §20.3 Vault 断の縮退 / §20.4 CredentialProvider と credential-broker / §20.5 資格局所性 / §20.6 Break Glass
- §21 DesiredSource と catalog（図面）
  - §21.1 DesiredSource seam / §21.2 Watch capability / §21.3 Loader / §21.4 catalog（図面）
- §22 委譲と停止影響
  - §22.1 外部委譲の一覧 / §22.2 observability-as-Resource / §22.3 Event 単一型と sink / §22.4 衛星停止影響表

**第VII部 運用ライフサイクル**
- §23 ブートストラップ
  - §23.1 自己を立ち上げる何かは自分の外側から来る / §23.2 段0の構成 / §23.3 段0の設計判断 / §23.4 管理ネットワークの要件 / §23.5 コールドスタート順序と段0 の SPOF / §23.6 段0-minimal / §23.7 devenv は Sakutei 自身で構築する
- §24 ブラウンフィールド
  - §24.1 最大の地雷:不完全な理想 × prune / §24.2 信頼獲得の順序 / §24.3 人の変更を早々に巻き戻さない / §24.4 import / §24.5 Sync Mode / §24.6 観測不能資産の階級 / §24.7 導入順序と escape hatch
- §25 災害と復旧
  - §25.1 DR モード / §25.2 git 外部化とローカルミラー / §25.3 S2 喪失は事故 / §25.4 サイト分断時の挙動と DR 判定
- §26 検証と監査
  - §26.1 検証の4段 / §26.2 devenv（坪庭）と twin（縮景） / §26.3 監査（S4） / §26.4 適合レポート（status） / §26.5 合成検証（canary） / §26.6 CLI の全 verb

**第VIII部 実装**
- §27 実装スライス
  - §27.1 スライスの基準 / §27.2 v1a / §27.3 v1b / §27.4 v1.5 / §27.5 v2 以降 / §27.6 作らないもの / §27.7 スライスの相対規模
- §28 Provider SDK
  - §28.1 なぜ SDK が採用を決めるか / §28.2 作者が書くもの / §28.3 SDK が担うもの / §28.4 最難関は Schema 設計 / §28.5 プロバイダの階級 / §28.6 golden path と事例集 / §28.7 2層プロバイダ戦略 / §28.8 【将来検討】抽象リソースと実装の分離
- §29 残る課題と既知の限界
  - §29.1 実装中に決める事項 / §29.2 意図的な先送りと再検討トリガ / §29.3 既知の限界 / §29.4 非技術の着手条件 / §29.5 着手前決定を再オープンさせない運用

---

# 第I部 思想と境界

本部は Sakutei の存在理由・境界・貫く原則を定める。個々の実装判断（第II部以降）は、本部が定めた原則の適用として導かれる。

---

## §1 課題認識と位置づけ

### §1.1 何が問題か

オンプレミス基盤の宣言的管理には、既存ツールがそれぞれ構造的な限界を持つ。

- **Terraform の state ドリフト**：Terraform は「宣言」と「現実」の間に **state**（現在状態のキャッシュ）を挟む。宣言・state・現実の三項が食い違う **three-way diff** が原理的に生じ、state と現実がずれる drift 地獄と、state を正とみなすことで生じる誤削除が構造的に付きまとう。
- **Ansible の push 一回性**：Ansible は state を持たず冪等だが、**一回 push して終わる**（収束しない）。人手や外部要因で現実がずれても、次に人が playbook を叩くまで放置される。drift の検知も自動修復もない。
- **k8s の実装機構の重さ**：k8s は level-triggered な収束ループで回り続けるが、その代償として apiserver・etcd の分散合意・scheduler・admission・CRD 足場という重い実装機構を抱える。これはアプリ層の大規模スケジューリングには見合うが、オンプレ基盤の収束にはオーバースペックである。

Sakutei は k8s の**収束原理（level-triggered な収束ループ）という動作原理だけ**を借り、実装機構は持たない。位置づけは一言でいえば **「Ansible の冪等性 + k8s のループ」**——宣言的・冪等でありながら、回り続ける。

### §1.2 立ち位置

| | 理想状態 | 観測 state | 駆動 |
|---|---|---|---|
| Ansible | 外部宣言 | 持たない | 一回 push（収束しない） |
| Terraform | 外部宣言 | **持つ**（state drift 地獄） | 一回 push |
| k8s | 内部 etcd | **持つ**（status subresource / informer cache） | ループ |
| **Sakutei** | 外部宣言 | **持たない**（観測 state レス） | **ループ** |

理想状態を外部宣言に置く点は Ansible / Terraform と同じだが、**観測 state を持たず、かつ回り続ける**点が Sakutei に固有である。

### §1.3 level-triggered + 観測 state レスの定義と、それが構造的に排除するもの

Sakutei の中核は次の二つの性質の組み合わせにある。

- **level-triggered**：毎周期、宣言された理想状態（desired）と現実（actual）を突き合わせ、差分を埋めにいく。イベントを1回処理して終わる edge-triggered ではなく、**状態そのものを見て毎周判定する**。人手や外部要因で現実がずれても、次の周期で必ず検知・修復される（drift 検知漏れがゼロ）。
- **観測 state レス**：現実の「現在状態」をキャッシュとして保持しない。毎周期、現実をリアルに再観測して desired と比較する。前回の観測結果を正とみなすことをしない。

この組み合わせが構造的に排除するもの：

- **Terraform の three-way diff / state ドリフト**が消える。宣言と現実の二項しか存在せず、両者の間にずれる中間物（state）が無いため、state と現実の乖離という問題自体が発生しない。
- **drift 検知漏れが消える**。毎周・全リソースをリアル再観測するため、「state 上は収束しているが現実はずれている」という状態が原理的に起きない。
- **dry-run が原理的に正しくなる**。現実を見る Observe と、desired と比較する Diff が型で分離されているため、Diff で止めれば「何が変わるか」を正しく示せる。

k8s は status を etcd に永続化するが、Sakutei はそれすら持たず毎周観測する。観測 state を持たないことは制約ではなく、上記の問題群を構造的に排除するための積極的な設計選択である。

なお「state レス」は正確には **観測 state レス**であって、state を一切持たないわけではない。Task/Workflow の進行など「喪失が事故になる」state は限定的に持ち、永続化する。state の分類学は §4 で定める。

### §1.4 名の由来

プロジェクト名は **Sakutei（作庭）**。平安期の『作庭記』は世界最古の「庭の宣言的仕様書」であり、**理想の姿を宣言し、絶えず観察し、剪定（prune）し、手を入れ続ける**という日本庭園の営みが、本システムの動作原理そのものと一致する。庭は放置すれば drift する。

命名は非対称に扱う。**固有名詞はプロジェクト名 `Sakutei` のみ**とし、コンポーネントは機能名（英語）で呼ぶ。プロジェクト名は文書や会話に一度しか現れないため固有名詞の記憶コストが問題にならないのに対し、コンポーネント名はコード・ログ・日常会話で高頻度に使われるため、「意味の紐付いていない音を記憶するコスト」が継続的に発生するからである。庭園メタファは別名として保持するが（関守／台帳／図面／坪庭／縮景）、正式なコンポーネント名は機能的な英語名を正とする。

### §1.5 CLI が体現する位置づけ

CLI の動詞も業界標準を用いる（既存知識が転用でき、学習コストが生じない）。バイナリは `sakutei` 単一とし、サブコマンドで機能を分岐する。

```
sakutei                            # 常駐して回り続ける（本来の姿）
sakutei plan                       # 何が変わるか（= Diff で止める。dry-run）
sakutei reconcile                  # 一回だけ収束させる（手動キック）
                                   #   --expect-revision <rev> で理想側の版のみ照合
sakutei status                     # drifted 一覧（適合レポート）
sakutei import                     # 既存の現実を intent として書き出す（brownfield 導入）
                                   #   --scope <範囲> --out intent/
sakutei --claim <scope-selector>   # 自分が収束を担当する scope を宣言して常駐（静的シャーディング）
```

`apply` を主動詞に置かないのは、**Sakutei が「一回叩くツール」ではなく「回り続けるもの」**であることを名前で表明するためである（level-triggered の思想を CLI が体現する）。承認ゲートは API（`POST /workflows/{id}/gates/{gate}/approve`）で扱う。

### §1.6 対象領域

対象は**オンプレミス基盤の全域**——物理電源／BMC からゲスト OS・コンテナまでの全レイヤである。

```
物理（電源/BMC）→ ネットワーク機器/FW → SDN → ストレージ
  → ハイパーバイザ/クラスタ → VM → ゲスト OS → コンテナ
  ＋ 基盤サービス（DNS/DHCP/PXE/NTP）
```

手作業が多く、誰も正確な実態を把握していないことが常態のオンプレでこそ、drift 自動修復と観測 state レスの価値が最大化する。

本書は**汎用エンジンの仕様**であり、特定案件の構築手順ではない。本文中に現れる具体プロダクト名（Proxmox / Ceph / Vault 等）は参照実装での採用例であり、仕様そのものは interface と seam の言葉で記述する。具体スキーマ・ベンダー評価・構築手順は参照実装文書へ分離する（混在させると、コア仕様の変更提案と案件固有の設計変更がレビューで混ざるため）。

---

## §2 作るもの・委譲するもの・捨てるもの

### §2.1 システム全体像

Sakutei は **reconciler（収束コア）** と、その周辺の**小さな衛星群**からなる。coreは単一バイナリ・単一デーモンとして小さく保ち、衛星はライフサイクル差・可用性の逆転・攻撃面の分離という明確な理由でのみ分離する。衛星が生えているのは機能膨張ではなく、責務境界を正しく引いた結果である。

```
                        ┌────────────── 人間 ───────────────┐
                        │ 金庫: unseal key / Break Glass     │ ← ブートストラップの底
                        │ Ansible（段0の構築・unseal）        │
                        └──────────────┬────────────────────┘
                                       │ 構築（稀・意図的・pull 型）
                                       ▼
┌──────── 段0（人手管理・"作って忘れる" 不変基盤）─────────┐
│  管理NW到達性の最小核 + OOB / コンソールサーバ            │
│  Proxmox 3ノード + 統合 Ceph + VM-HA                      │
│    ├ Vault（シークレットの根）                            │
│    ├ [credential-broker]（reconciler の依存先）           │
│    └ reconciler（収束コア）                               │
└──────────────────────────────┬───────────────────────────┘
                               │ 自律収束（常時・level-triggered）
                               ▼
┌──────────────── 段1以降: 本番インフラ全体 ───────────────┐
│ 物理 → NW/FW → ハイパーバイザ → VM → guest OS → コンテナ │
└──────────────────────────────────────────────────────────┘
```

**A. reconciler（収束コア）** — 単一バイナリ・単一デーモン。収束ループ（§8）、DAG/wave 依存解決、render パイプライン（§9）、3プリミティブ（§6）、安全機構（第IV部）、Event bus（§26）を内包する。

**B. 衛星群**（core から分離した独立ソフト）

| 衛星 | 別名 | 役割 | 分離理由 | core との関係 |
|---|---|---|---|---|
| **credential-broker** | 関守 | 機器認証の統一（JIT 短命発行 / bastion / Break Glass） | ライフサイクル差（要求応答 vs 常時収束）／ **可用性の逆転**（core の依存先）／ 攻撃面の分離 | core の依存先（下位）かつ core の管理対象 |
| **allocator** | 台帳 | プール採番（VLAN/IP/ASN）の権威 | **state（冪等性の記憶）を持つ**／ 並行・シャード環境の採番調停を一元化 | core は「1つくれ」と要求するだけ |
| **catalog** | 図面 | intent 用の薄い UI | UI に独自のデータモデル／DB を持たせない（正は git 側） | 書込経路は ownership に従う |

分離理由のうち **credential-broker の「可用性の逆転」** は決定的である。broker は core の依存先（下位）であり、core が死んでも認証が生きていないと緊急時に人が機器に入れない。core 内に認証を置くと致命的な循環依存になる。さらに broker 断は Vault 断より重い——Vault 断は「シークレット依存の Apply が止まる」だけだが、broker 断は認証が要る全ターゲットの Observe が止まる = システム全盲になる。allocator が独立する理由は state を持つこと——core を観測 state レスに保つため、採番の記憶は外部の権威に外出しする。catalog（図面）が独立する理由はその逆で、UI に独自のデータモデルを持たせないこと（intent は git が正なので UI は薄いフロントエンドでよい）。

**依存の向き（鶏卵の構造）**：core は broker/Vault を**管理する**（いずれも Resource である）が、実行時には**依存する**。この循環は **段0が素の起動を保証する**ことで断ち切る。最後は人間の物理保管（金庫の unseal key）に着地する原則が底であり、これがブートストラップの無限後退の正しい終わり方である（詳細は §23）。

### §2.2 外部委譲（作らないもの）

**持たない・委譲する**ことは Sakutei の強みである（漏洩面が縮み、再発明を避け、core が小さく保たれる）。以下を外部に委ねる。詳細な委譲一覧と停止影響は §22 を正とする。

| 委譲先 | 委ねたもの | core の関与 |
|---|---|---|
| Vault | シークレットの値・動的発行・rotation | `SecretRef` で参照。値を持たない |
| IdP / SSO | 人の ID・認証 | broker 経由。core は認可を持たない |
| Prometheus / Alertmanager | 時系列監視・通知ルーティング・沈黙 | **監視"設定"を Resource として収束**。判定と通知は委譲 |
| DRS / ハイパーバイザ | 配置最適化・ビンパッキング・migrate | cluster への intent と粗い容量充足のみ。**scheduler を再発明しない** |
| 上位テナント基盤 | 認可・quota・課金・ポータル | `DesiredSource` の一実装として理想状態を投入 |
| git（GitHub/GitLab） | intent の履歴・レビュー・改竄耐性アンカー | `FileSource` の実体。監査の半分を担う |
| k8s 等 | アプリ層のデプロイ・スケール | core は土台（インフラ）を供給 |
| Ansible（モジュール） | ロングテール provider の実行実体 | `Executor` の一実装。**モジュールのみ借り、実行モデルは借りない** |

### §2.3 k8s から捨てたもの

Sakutei は k8s の**収束原理だけ**を借り、実装機構は意図的に捨てている。

- **apiserver** — 採らない（中央 API サーバを再発明しない。理想状態は git / DBSource から pull する）。
- **etcd（分散合意）** — 採らない（多書き手の合意は不要。理想状態はファイル + インメモリで保持し、必要になれば embedded KV へ移す。分散合意を中枢に戻さない）。
- **scheduler** — 採らない（配置最適化は DRS へ委譲。ビンパッキングを解かない）。
- **admission** — 採らない（検証はロード時の JSON Schema 検証と循環検出で足りる）。
- **CRD 足場** — 採らない（型は Go の interface として直接定義し、動的スキーマ機構を持たない）。

k8s の**偶有的複雑さ（実装機構）は削り、本質的複雑さ（運用の泥臭さ：手続き・物理・テナント・DR・自己運用）は引き受ける**——方向が逆の二つの動きにより、「芯は小さく、適用面は広い」を両立させる。

### §2.4 seam（拡張と分離の全接点）

Sakutei の実質的なモジュール境界は seam interface である。**seam は今切り、split は後**という原則（§3.6）に従い、将来の分離点を先に interface として切っておく。次表を seam の正とし、各 interface の定義先を対応させる。

| seam | 差し替えるもの | 定義先 |
|---|---|---|
| `Provider` / `Lister` / `BulkObserver` | 個別操作 / 集合列挙（prune の前提）/ バルク Observe | §5 |
| `Executor` | 実行境界（in-process / プロセス隔離 / リモート） | §5, §17 |
| `Loader` | 設定形式（YAML/JSON、将来 CUE） | §9, §21 |
| `DesiredSource` / `Watcher` | 理想状態の供給源 / 早期収束ヒント供給 | §11, §21 |
| `DesiredStore` | 理想状態の保存（ファイル / bbolt / SQLite） | §21 |
| `SecretBackend` | シークレット取得（Vault / file） | §20 |
| `AllocationAuthority` | 採番払い出し（自作 / NetBox / Infoblox） | §19 |
| `CredentialProvider` | ターゲット到達資格（SecretRef 直結 / credential-broker） | §20 |

---

## §3 八つの原則と適用境界

全コンポーネントに共通する。個々の設計判断はこの八原則の適用として導かれる。前半の六原則は Sakutei を貫く基本原則、後半の二原則はスケール・分散・衛星の設計で結晶した原則である。

### §3.1 level-triggered + 観測 state レス

毎周期、desired と actual を突き合わせ、差分を埋める。観測 state は持たず、毎周リアルを再観測する。持つ state は分類し、**喪失が事故になるものだけを永続化**する（§4）。この原則が効くのは、drift 検知漏れをゼロにしたい場面と、three-way diff / state ドリフトを構造的に排除したい場面である。他の全原則の土台であり、後続の原則の多くはこの原則を破らないための規律として導かれる。

### §3.2 いつ = 収束 / どう = 手続き

「いつ・どの順で変えるか」は収束が判定し、「どう変えるか」は Task/Workflow が実行する。経路依存・非冪等・中断が致命的な手続き（migration / firmware / ローリング / DR）は収束ループに載せず、Task/Workflow に閉じ込める。収束はトリガ・メンテ窓ゲート・依存順序・drift 検知・完了確認だけを担い、危険な手順そのものは Task/Workflow に閉じ込める。実行中の対象は `converging` 扱いにして収束の介入を止める。この原則が効くのは、冪等な状態収束では表現できない一回性・順序依存の危険な手順を扱う場面である——例えば理想状態に `column: new_name` とだけ書くと収束は rename を drop+add に化けさせてデータを消しかねず、こうした操作は Task に閉じねばならない。

### §3.3 確信が持てないとき、加算はしても減算はしない（非対称原則）

最も広く貫く原則。`unknown ≠ absent`（到達不能を「消えた」と断じない）／ `pruneable:false` 既定 ／ 分断時の破壊保留 ／ 爆発半径 halt ／ freeze ／ allocation の Release 遅延 ／ render 入力の `incomplete ≠ empty` まで、すべてこの一つの規律の適用である。この原則が効くのは、観測や理想が不確かなときに「消す」判断が誤ると回復不能になる場面——すなわち削除・破壊のすべてである。破壊は確信を要求し、加算は許す。

### §3.4 持たない・委譲する

値・配置・予測・認可は自前で持たず外部に委ねる。シークレット値 → Vault、配置最適化 → DRS、時系列予測 → Prometheus、認可・課金 → 上位テナント基盤。**持たないことが強み**であり、漏洩面が縮み、再発明を避け、core が小さく保たれる。この原則が効くのは、「作れるが作るべきでない」機能の誘惑が生じる場面である。判断基準は、その責務が state を持つか・別事業として成立するか・攻撃面を広げるかである。

### §3.5 seam は今切る、split は後

将来の分離点（分散実行・外部 CMDB・DB 供給源・認証・採番）を interface として先に切り、実際の分割は必要になってから行う。これによりコアを無変更のまま拡張を吸収できる。この原則が効くのは、いま単一プロセス・単一実装で足りるが、将来分割・差し替えが確実に来ると分かっている場面である。seam を切っておけば、フェイク Provider を挿すだけで実インフラなしに収束エンジン全体をテストできる（seam 設計の副産物）。

### §3.6 ownership がすべてを貫く

ただ一つの区別——`platform`（共有基盤・PR 必須・厳格）か `tenant`（専有・即時書込・緩い）か——が、**所有・書込ポリシー・修復モード・レート制限・爆発半径・供給源の選択・シャード分割境界**を横断的に決める。個別に導いた結論がこの一軸に乗ったことは、分解が正しい証拠である。この原則が効くのは、書込経路・remediation・rate limit・blast-radius をリソースごとに設計しなければならないと思える場面で、実際にはすべて ownership 一軸で決まる。

| 層 | 書込経路 | remediation | rate limit | 爆発半径 | 供給源 |
|---|---|---|---|---|---|
| tenant | 即時（レビュー無） | auto でよい | 緩め（warn） | 小（砂場内） | DBSource |
| platform | PR 必須（人が承認） | alert-only 寄り | 厳（halt） | 大（全体） | FileSource（git） |

テナントがレビュー不要なのは、scope で砂場境界を切ってあり他テナントも基盤も壊せないからである（テナント内でのレビューの要否はテナントの自己責任）。テナントが基盤に触れる操作（採番消費・容量消費）は、authority（採番）と quota（容量）という事前に区切られた枠を通すことで即時のまま安全に保たれ、枠を超える要求だけが platform 層の承認に上がる。**所有権の論理境界がそのままシャードの物理境界になる**（§3.7）ことも、この一軸が横断的である証左である。

### §3.7 シャード間協調は現実を介して行う

スケールアウトはシャーディング（tenant / site / cluster での水平分割）で行い、各シャードは shared-nothing で独立に収束する。このとき**シャード同士は互いに問い合わせない**。相手が作った**現実を Observe して gate する**（「観測 ready 待ち」による結果整合）。シャード間 RPC / 状態交換は正しさに一切関与しない——交換した状態を信じることは、観測 state を持つのと同じ罠だからである。これは観測 state レス思想（§3.1）のスケール版である。共有物はすべて衛星に外出し済み（採番 = allocator・秘密 = Vault・intent = git）であり、**この分解がそのままスケールアウトの分解になる**。この原則が効くのは、単一デーモンの天井を超えて複数デーモンへ分割する場面である。

### §3.8 衛星の停止は変化を止めるが維持を止めない

**定常状態の収束ループは、衛星コールゼロで回る**——これを設計保証とする。allocator が確定させた割当事実は Release まで不変なので、`Lookup` の結果はシャードローカルに無期限キャッシュしてよい（これは「観測」ではなく「自分が確定させた事実」だから観測 state レスに反しない）。secret も last-resolved 保持 + TTL キャッシュで Vault 断中も Apply できる。したがって衛星が停止しても止まるのは**変化（新規採番を伴う作成）だけ**であり、**維持（既存リソースの定常収束）は止まらない**。衛星への QPS はフリート規模ではなく**変更レート（churn）に比例する**。この原則が効くのは、「シャード数を増やすと共有衛星が詰まる」「衛星が可用性の天井になる」という懸念が生じる場面で、実際には定常収束は衛星に触れない。

### §3.9 適用境界

Sakutei は万能ではない。スイートスポットと、設計では到達できない領域を正直に宣言する。

**スイートスポット**（Sakutei の価値が最大化する環境）：

- **規模**：数十〜1万リソース。単一デーモンで毎周全 Observe が回り、バルク API を持つ層なら1万規模まで届く（数万を超えるとシャーディングへ）。
- **所有形態**：**platform ownership 中心**。共有基盤を PR + 承認で厳格に統治し、drift を自動修復したい環境。
- **構成様式**：合成リソースとしてテンプレート化できる、繰り返しの多い構成（IaaS カタログ・クラスタ・テナント）。
- **組織文化**：git / PR によるレビューと監査を受容する環境。理想状態を宣言し、変更を PR で承認する文化が根付いている（あるいは根付かせられる）こと。

**設計では到達できない領域**（正直な限界の宣言）：

- **管理対象機器の API が貧しい環境は観測止まり**。Observe すら不可能な旧機器・ベンダーロックの箱は、台帳上 `unknown` のまま第一級で扱う（§24）が、収束（Apply）はできない。これは設計の穴ではなく**対象側の制約**であり、機器 API の貧しい構成（例：SOHO 級ネットワーク／ストレージ機器）は、可視化・drift 検知までが到達上限として確定している。
- **上位テナント基盤は別事業**。認可・quota・課金・セルフサービスポータルは、それ自体が一つの事業本体であり、Sakutei の責務ではない。Sakutei は `DesiredSource` の一実装として、認可・quota 適用済みの理想状態を受け取るだけである。パブリック IaaS のような構成で残る差分は、この委譲先の実装工数であって Sakutei 側の技術ギャップではない。
- **ゼロタッチ物理は責務外**。電源も入っていない・配線もされていない物理の「段0 の底」を人手・OOB なしに立ち上げる能力は持たない。ブートストラップは人手（Ansible + 金庫）から始まる（§23）——これは妥協ではなく定石であり、自己を立ち上げる何かは自分の外側から来るしかない。エッジ多拠点のような構成で残る本質的な限界は、この「ゼロタッチ物理段」であり、Sakutei の責務境界の外にある。

この境界は検証を経て確定したものである。20 構成の適用可能性検討では、最終的に到達不能として残った領域はいずれも上記三種——対象の制約・別事業・責務外——に純化しており、**設計で解ける課題はすべて解けた（残るは実装順序と対象側の制約のみ）**という収束状態に至っている。

**レイヤ別の適合と主な地雷。** オンプレ基盤を電源/BMC からコンテナまで縦断したとき、各レイヤの Observe 手段・適合度・踏むと事故る地雷は次のように分かれる。◎＝宣言的で素直 / ○＝収束できるが地雷あり / ✕＝収束外（Task へ）である。

| レイヤ | Observe 手段 | 適合 | 主な地雷 |
|---|---|---|---|
| NW 機器 | NETCONF / gNMI / RESTCONF、旧機種は CLI scrape | ○ | 管理断ロックアウト / 順序付きリスト |
| LB / FW | F5 iControl / PANOS / Forti API | ○ | FW ルールは順序付きリスト |
| SDN | NSX-T policy / ACI / OVSDB | ◎ | **二重ループの綱引き**（相手も収束器・§11） |
| ストレージ | Pure / ONTAP / PowerStore REST | ◎ | volume 削除 = データ消失（破壊的・§12） |
| ハイパーバイザ | vCenter（govmomi）/ libvirt | ◎ | **DRS / HA が動かすフィールドを触るな**（managed fields・§7） |
| VM 内 OS / ソフト | SSH / WinRM or 常駐 agent | ○ | Observe 手段の確保 |
| VM 内 DB の中身 | — | ✕ | **収束外**（Task 行き・§6） |
| コンテナ | podman / docker API | ◎ | k8s 上なら二重ループ無意味（アプリ層は k8s に委譲・§22） |

**「薄いオーケストレータ」で済むのは上位が宣言的な部分（SDN / VCF / アレイ）だけである。生の NW 機器・FW・guest OS が "泥臭い" 層で、実装の8割はここに費やされる**（この結論は provider 戦略・§28.7 に直結する）。◎の層だけを見て「宣言的で簡単」と訴求するのは不正直であり、地雷（順序付き・managed fields・二重ループ・converging）は各層に散在する。


# 第II部 中核モデル

本部は Sakutei の中核データモデルを定義する。§4 で全プロバイダに波及するコア型と状態分類を確定し、§5 でプロバイダが実装する抽象を、§6 で3つのプリミティブを、§7 で所有と境界の宣言を定義する。ここで固定した型と契約が、第III部以降（収束機構・安全機構・スケール）の全機構が読む共通の語彙になる。

---

## §4 正準型と状態分類

コア型は「後から変えると全プロバイダに波及する」ため、構造（何を持つか）をここで正準形として固定する。細部の命名・タグ体系は実装中に調整可能だが、構造は変えない。

### §4.1 ID —— （provider, scope, id） 複合

リソースの同一性は3要素の複合で表す。

```go
// ID — リソースの正準識別子。global-unique な単一文字列ではなく複合キー
type ID struct {
    Provider string  // どの provider が扱うか
    Scope    Scope   // ルーティングキー(§4.2)。claim シャーディングの境界(§17)
    Local    string  // scope 内でのローカル ID(provider が発行・観測する id 文字列)
}
```

**確定理由:`scope` がルーティングキーである。** claim シャーディング（§17）は「どの scope を自分が収束するか」で担当を分ける。ID に scope を含めないと、再シャード時にリソースの帰属が壊れる。global-unique な単一文字列 ID は「再シャードで壊れる」ため棄却する。`Local` は provider が観測・発行する id 文字列であって、その解釈は provider に閉じる（コアは不透明に扱う）。

### §4.2 Scope —— ラベル集合 + パス形式

`Scope` は provider 非依存の管理境界を表す。zone / cluster / rack を1型で表し、provider 追加のたびにコア型を変えずに済ませる。

```go
// Scope — provider 非依存のラベル集合 + パス形式
type Scope struct {
    Labels map[string]string  // 例: {site: tokyo, cluster: prod}。provider ごとの意味を1型で吸収
}

func (s Scope) Path() string  // labels → intent/<scope>/... のパス。rendered_revision 算出単位でもある(§9)
```

`Scope` は4つの用途で同一語彙を共有する。この一致が設計の要である。

- **claim の scope-selector**（§17）:`--claim {site: site-a}` が担当境界を切る。
- **prune 境界**（§12）:closed-world の集合演算は scope 内に閉じる。
- **`intent/<scope>/` パス規約**（§17）:claim ↔ パスが機械的に対応し、sparse checkout が「自 claim 分 + 共有 platform 分」だけを引ける。
- **rendered_revision 算出単位**（§9）:`Path()` が revision 計算の粒度になる。

型付き（zone 型・cluster 型と分ける）にすると provider 追加のたびにコア変更になるため、**ラベル集合という1型**に統一する。

### §4.3 State —— Schema 付き map

観測された現実の状態を表す。型情報は `Schema`（§5）が持つので `State` 自体は map で足りるが、各値は Schema の宣言型に従う。

```go
// State — Schema 付き map(key=Schema のフィールド名、value=Schema 宣言型に沿った値)
type State map[string]TypedValue
type TypedValue struct {
    Kind  FieldKind  // Schema の type と対応
    Value any
}
```

`State` は現実側の状態のみを表し、desired（あるべき姿）は持たない。この分離が dry-run の正しさの根拠になる（§4.6, §8）。

### §4.4 Plan —— 操作列 + 破壊性 + 原因タグ + 対象 + 診断

`Plan` は「何をすべきか」を表す。Apply の契約（§5.4）・rate limit の重み付け（§13）・破壊予算（§13）・per-resource 原因タグ（§10）が全部これを読むため、必要な情報をすべてここに載せる。

```go
// Plan — 操作列 + 破壊性 + 原因タグ + 対象 ID + 人間可読の差分診断
type Plan struct {
    Ops []Op  // Create / Update / Delete の操作列
}

type Op struct {
    Kind        OpKind      // Create | Update | Delete
    Target      ID          // 対象(§4.1 の (provider, scope, id) 複合)
    Destructive bool        // ★破壊性フラグ。rate limit・破壊予算・halt-all 凍結がこれで駆動(§13)
    Cause       CauseTag    // ★原因タグ A/B/C/D/E(per-resource 帰属・§10)
    Diagnostic  string      // 差分の人間可読診断(sakutei status / plan 表示用)
    Fields      []FieldDiff // 変わるフィールド(Update 時)。表示粒度はここで展開(§12)
}
```

- **`Destructive`** が安全機構の駆動軸である。rate limit・破壊予算・全体凍結がこのフラグで動く。不明時（`TrueObserve:false`）は破壊的側に倒す（§5）。
- **`Cause`** は差分1件ごとに原因タグを帰属させる（per-resource）。原因の分類定義は §10 を正とする。

### §4.5 Observation —— Availability + State + Ready

`Read`（§5）の戻り値。`drifted` / `converged`（desired との比較結果）は Observe では判定できず、これらは **Diff の出力**である。混ぜると Observe が desired を知る必要が生じ、dry-run の根拠（Observe/Diff 分離）が崩れる。

```go
// Observation — 現実を見た結果。desired を知らない
type Observation struct {
    Availability Availability     // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State            // Availability == ok のときのみ有効
    Ready        map[string]bool  // 名前付き ready condition の充足状況(§8.3, §11.2)
    ObservedAt   time.Time        // この観測を取った時刻。bulk/個別いずれも刻む(§5)。周期越え再利用の検知に使う
}

type Availability int  // Ok | Unknown | Converging
```

**三値の意味を絶対に混同しない**（非対称原則の起点・§3）。

- **`ok`** … 存在する。`State` が有効。
- **`unknown`** … 到達不能。**存在しないことと unknown を絶対に混同しない**（`unknown ≠ absent`）。
- **`converging`** … 相手が収束作業中（相手も収束ループを持つ）。drift と誤認して再 Apply すると綱引きになる（§11）。

`error` は到達失敗そのもの（接続不能等）を表し、到達したが「相手が状態を返せない」場合は `Availability` で表現する。収束ステータス（報告用）はコントローラが**導出**する:`unknown`/`converging` は Observation から、`converged`/`drifted` は Diff 結果（Plan が空か否か）から、`oscillating` は履歴から（§11.3）。

**`ObservedAt` を各 Observation に刻む**（バルクでも個別でも）。バルク結果は当該周期でのみ使用し、**周期をまたいだ再利用は禁止**する——古い観測を「現在状態」として次周期に持ち越すことは、排除したはずの観測 state の再導入である（§8）。`ObservedAt` はこの周期越え再利用を検知・拒否する目印である。

### §4.6 ChangeHint —— revision + scopes のみ

早期収束（トリガ収束・§8）の入口が流すヒント。**トリガは「いつ収束するか」のヒントであり、「何が真実か」を一切運ばない。**

```go
// ChangeHint — 早期収束のトリガ。ペイロード(変更後の値)は運ばない
type ChangeHint struct {
    IntentRevision string  // intent_revision(触られたことの目印)
    Scopes         []Scope // 触られた scope 群(この scope を今見に行け、としか言わない)
}
```

ヒントに含まれる情報を「現在状態」として信じることは一切しない。トリガを取りこぼしても次の定期周期が必ず拾う（遅延のみ・正しさは無傷）。詳細は §8 を正とする。

### §4.7 Event —— 単一型 + kind

イベントは単一型に kind enum を持たせる。発生源と sink を分離するための共通型で、severity・level か edge か・Since・原因タグをそのまま入れる。

```go
// Event — 単一型 + kind enum + scope/resource 参照 + revision 参照
type Event struct {
    Kind        EventKind  // enum(DriftDetected / ProviderUpgradeDiff / StageAwaitingApproval / BreakGlassUsed …)
    Severity    Severity   // info | warning | critical
    LevelOrEdge LevelKind  // level(継続状態・firing/resolved)か edge(事件・一回)か
    Scope       Scope      // どの scope で起きたか
    Resource    *ID        // 個別リソース参照(あれば)
    Revision    string     // 関連する rendered_revision / intent_revision
    Since       time.Time  // level イベントの継続開始(transient↔stuck 自動昇格)
    Cause       *CauseTag  // 原因タグ(監査帰属)
}
```

### §4.8 SecretRef —— 自動マスクされる専用型

秘密は文字列にしない。`SecretRef` 専用型を用い、`String()` が `***` を返すことで**自動マスク**し、実値取得は明示 `.Resolve(ctx)` 経由のみとする。「うっかり平文化」を型で防ぐ。

```go
// SecretRef — 秘密の参照。値は保持せず、Apply 直前に JIT 解決する
type SecretRef struct {
    Path string  // 例: vault:/db/prod#password
    Gen  string  // 世代(render フェーズ1 が Vault metadata から注入。ピン留めではない)
}

func (r SecretRef) String() string          // 常に "***" を返す(ログ・Event・ダンプで自動マスク)
func (r SecretRef) Resolve(ctx) (string, error)  // Apply 直前・provider に渡す瞬間だけ実値化
```

diff は**参照同士で取る**（値を知らなくても diff できる）。git への実値書き戻しループは生じない。JIT 解決の詳細・`CredentialProvider` との境界（注入する設定値か、engine 自身の到達資格か）は §20 を正とする。

### §4.9 状態分類 S1〜S4

「state レス」は正確には**観測 state レス**である。他の state は限定的に持つ。判定基準は**「喪失が事故になるのは S2 だけ → そこだけ永続化」**。

| クラス | 中身 | 置き場 | 喪失時 |
|---|---|---|---|
| **S1 観測 state** | リソースの current キャッシュ | **持たない** | —（そもそも保持しない） |
| **S2 進行 state** | Task/Workflow フェーズ・承認待ち・シャードルーターの割当表 | **外部永続化必須** | 事故（割当表喪失=誤配送） |
| **S3 履歴 state** | 振動検知・ベースライン・backoff・Since・in-flight 集合・allocation Lookup キャッシュ | インメモリのみ | 検知が数周鈍るだけ（安全側）。Lookup キャッシュ喪失時は allocator に問い直すだけ |
| **S4 監査 state** | append-only ログ | 別ストア（収束経路から不可視） | 収束に影響なし |

これにより全クラスの再起動セマンティクスが定義済みになり、「再起動安全・DB 不要」という運用上の推論が正しくなる。永続化が必要なのは S2 だけであり、その永続化先（quorum ストア + fencing）は §17 を正とする。

**S2-free シャード階級。** エッジ / 店舗の claim は **Resource 収束のみ**を担い、**ローカル S2 を持たない**階級を第一級で定義する。「何も覚えないから、何も失わない」——デーモンホストが全損しても失うものがゼロで、再イメージ → pull → 収束で完全復旧する。Task/Workflow が要する S2 は中央 claim 側に置くか、完了マーカーを現実側に置いて Observe で問う（§6.2 単純型と同型）。詳細な運用（git 配布規約・claim ハンドオフ）は §17 を正とする。

---

## §5 Provider 抽象

**プロバイダの書きやすさが Sakutei の普及を直接決める**。設計の肝は、プロバイダ作者が書く最小を極限まで削り、面倒はコア側の共通ライブラリ（SDK）が引き受けることにある。作者が書くのは約4関数（+ prune 用 `List`）、SDK が diff・秘匿マスク・managed 除外・version 埋込・エラー分類・テストハーネスを担う。

### §5.1 Provider interface 正準形

削りに削ると、作者が書くべきは **4つの関数**。prune を許すなら `List` を1つ足す。

```go
type Provider interface {
    // メタデータ
    Schema() Schema              // フィールド定義(型・必須・secret・managed・ordered・identity・computed)
    Capabilities() Capabilities  // TrueObserve / List / Converging / CommitConfirm / BulkObserve

    // 現実を読む(desired を知らない)。戻りは Observation(Availability / State / Ready)(§4.5)
    Read(ctx, id string) (Observation, error)

    // 現実を変える(create / update / delete を1関数に統合)
    Apply(ctx, plan Plan) error
}

// prune を許すなら追加(任意 capability)
type Lister interface {
    List(ctx, scope Scope) ([]ID, error)
}
```

**Diff は Provider が実装しない。** 差分計算はコアが `Schema` 駆動で汎用実装する。作者は「読む（Read）」と「あるべき姿にする（Apply）」だけ書けばよい。設計上の3つの負担軽減:

- **（1） Diff を書かせない**（最大の軽減）。フィールド比較は車輪の再発明かつバグの温床。順序付きコレクションのような特殊 diff も **Schema で `ordered` と宣言すればコアが適用する**——作者は宣言するだけ、ロジックはコア。
- **（2） create/update/delete を Apply 1本に統合**。`Plan` に「何をすべきか」が入っているので Apply 1本で受ける（中の分岐は自由）。
- **（3） Schema がすべての鍵**。フィールドの型・必須・secret・managed・ordered・identity・computed を宣言すれば、コアが diff・検証・マスキング・managed 除外を全自動化できる。

diff は二段である。**集合 diff**（desired 集合 vs List 結果 → create/delete 判定・コントローラの汎用ロジック）と、**フィールド diff**（両方に在るものの update 判定・Schema 駆動でコアが汎用実装）。

### §5.2 Schema 属性

`Schema` はフィールドごとに意味を宣言する。**Schema を正しく書くことが実装の重さの本体**であり、間違えると設計書が積み上げた安全性が無効化される。

| 属性 | 意味 | 誤ると |
|---|---|---|
| **type** | フィールドの型（FieldKind） | diff / 検証が壊れる |
| **required** | 必須か | 不完全な desired を検知できない |
| **secret** | 秘密値 | 資格情報が平文でログに出る（§4.8, §20） |
| **managed** | 外部システムが動かす欄 | 外部と永久に喧嘩（§7） |
| **ordered** | 順序が意味を持つコレクション | FW ルールの順序が壊れる（§12） |
| **identity** | 削除・参照のキー | 別リソースを消す |
| **computed** | 観測のみ（intent に書かない導出値） | 書けない値を書こうとして事故る |

**identity は「運用上の変更頻度」で選ぶ。** 技術的な一意性だけで決めてはならない。変更されやすい属性（シリアル番号・MAC アドレス）を identity にすると、その属性が変わるたびに Resource の再生成と理想状態の commit が必要になり、運用コストが跳ね上がる。物理サーバの identity をシリアル番号にすると故障交換のたびに intent の書き換えが要るが、**物理トポロジ座標**（「leaf03 スイッチの 12 番ポート」= `leaf03-p12`）を identity にすれば筐体交換時に理想状態の変更が一切不要になる。シリアルや型番は `managed: true` + `computed` として観測のみ行い、identity にはしない。SDK の **Schema lint** が「secret 宣言し忘れ」「identity 未指定」等を静的に検出する。実装は「まず Schema、次に Read/Apply」の順で書く。

### §5.3 Capabilities —— 機械的縮退

provider の能力差を散文の注意書きにせず、機械可読なメタデータにする。

```go
type Capabilities struct {
    TrueObserve   bool  // current を "状態" として返せる(Ansible check mode = false)
    List          bool  // → prune 可否(§5.4 の Lister と連動)
    Converging    bool  // 相手の作業中を検知できる
    CommitConfirm bool  // candidate/commit で自動ロールバック可(NW 機器)
    BulkObserve   bool  // scope 内を1コールでまとめて Read できる(§5.7)
}
```

**縮退規則（コントローラに固定）**:

- `TrueObserve:false` → actual 比較ができないので原因タグ A/B を区別不能。単一タグ `changed` に統合し、rate limit の重み付けは**破壊的側に倒す**（不明は危険扱い=非対称原則）。
- `List:false` → その型への prune 宣言はロード時に拒否する。
- Ansible でも `*_info` 系モジュールを Observe に使える対象は `TrueObserve:true` に**部分昇格**でき、縮退は全面でなく部分回復可能。

### §5.4 Lister と各関数の契約

**prune には Lister が要る。** 削除は `現実にある集合 − 理想にある集合` という**集合演算**であり、個別の `Read(id)` では「理想に書かれていないリソース」を発見できない。`DeletionPolicy: PruneWithinScope` は Lister を実装する provider にのみ許可し、Lister 無しの provider に prune を宣言したら理想状態ロード時に拒否する。

各関数が満たすべき契約に設計書の原則が集約される。

**Read の契約**:desired を絶対に見ない（§4.5）。三値（存在 = `ok` + State / 到達不能 = `unknown` / 相手が収束作業中 = `converging`）を正しく返す。`TrueObserve:false` なら State を返せず「変わったか」しか分からないので Capabilities で正直に宣言する。

**Apply の契約**:**冪等**（同じ plan を2回実行しても同じ結果）。毎周回る Sakutei では絶対条件。エラーを **transient / permanent に分類**して返す（backoff のため・§8.5）——権限エラー = permanent、API タイムアウト = transient。順序付きコレクションは**全置換**。commit-confirm 対応機器は candidate/commit を使う。部分適用は中途半端な状態を残さないか、次の Read で正しく観測できる状態にする。

**List の契約（prune するなら）**:scope 内の現実の全 ID を漏れなく返す。不完全だと prune が「List に無い = 消す」で取りこぼしを削除する——**List の網羅性が prune の安全性を担保する**。ページング完全性を保証し（next token が nil で終わったことを確認）、途中打ち切りを検知したら成功として返さず error にする（**complete-or-error**）。網羅を保証できない今回の List は `ListStatus`（complete / partial / unknown）で申告でき、コアは `complete` 以外の周は prune を保守側に倒す（`Read` の三値と対称）。中途半端な List を返すくらいなら実装しない方が安全。

### §5.5 エラー分類

Apply のエラーは2種類に分類して返す（§8.5 の backoff が読む）。

- **transient**（API 一時エラー・ロック競合）→ 指数 backoff（上限 15 分程度）で再試行。
- **permanent**（権限・バリデーション）→ 即 **stuck** 化し、**desired か `provider_version` が変わるまで再試行しない**。

SDK が Apply のエラーを transient / permanent に型付けするヘルパを提供する。

### §5.6 プロバイダの階級

全プロバイダに全機能を要求すると書けるものが激減する。**Capabilities で段階を認める**のが核心であり、作者は自分の階級を選べる。

| 階級 | 実装 | できること | 例 |
|---|---|---|---|
| **完全** | Read（TrueObserve）+ Apply + List + CommitConfirm | 全機能（prune・原因タグ A/B・converging・自動ロールバック） | 自前 DNS / VM |
| **標準** | Read（TrueObserve）+ Apply | prune 以外（List 無し = open-world のみ） | 多くの REST API |
| **最小** | Apply + changed 真偽（TrueObserve なし） | 加算的収束のみ。diff 粗い・原因タグ縮退 | Ansible ラップ |

Capabilities で正直に宣言すれば、コアが自動的に縮退して**安全に**扱う（prune させない・破壊的側に倒す）。「全機能を書けないと参加できない」ではなく「書ける範囲で参加でき、コアが安全に縮退する」——これがエコシステムを広げる鍵。

### §5.7 BulkObserve —— バルク観測と一貫性契約

多くの権威は **scope 内を1コールでまとめて返すバルク API** を持つ（vCenter PropertyCollector / Proxmox `/cluster/resources` / AWS Describe* / NetBox 一覧 API）。これを任意 capability として第一級化し、単一デーモンの天井（§16）を押し上げる。

```go
// prune 用の List(§5.4)と対をなす、Observe のバルク版(任意 capability)
type BulkObserver interface {
    BulkObserve(ctx, scope Scope) ([]Observation, error)  // scope 内をまとめて Read
}
```

`Capabilities.BulkObserve` で宣言する。宣言した provider には Executor がバルク経路を使い、宣言していない provider には **SDK が per-resource `Read` へ透過的に fallback** する（コントローラは差を意識しない）。`Read` の契約はそのまま適用される——バルクで取っても desired を見ず、**毎周・全量を再観測する原則は不変**である（観測 state レスに一切影響しない）。

**BulkObserve の一貫性契約（型で誤用を塞ぐ）:**

- **非アトミックスナップショット宣言**:結果は「走査窓内のある時点の各リソース」であって、全体の一貫スナップショットではない（DRS 稼働中は走査中に VM が移動する）。level-triggered が次周期で自己修正するため、これは正しさの問題ではなく**揺らぎの問題**である。
- **complete-or-error**:ページングは完走するかエラーを返す（`List` と同じ規律）。部分結果を「成功した BulkObserve」として黙示的に返すことを禁止する。
- **型分離（prune の根拠には使えない）**:BulkObserve は **Observe 専用**であり、**prune の根拠には一切使えない**。prune は `Lister.List` + List 完全性検証（§12）を根拠にする。SDK は BulkObserve の戻りが集合差 prune の入力に渡る経路を型レベルで塞ぐ（§5.4 で `BulkObserve` と `List` の型を別に保ったのはこの誤用防止のためでもある——`List` は ID 集合、`BulkObserve` は状態）。
- **走査窓内遷移の吸収**:走査中の DRS 移動が生む一時的不整合は既存機構が吸収する——runtime placement は managed fields 除外（§7）が diff から落とし、それ以外は converging / settle（§11.1）が自傷猶予として吸収する。
- **`observed_at` を各 Observation に刻む**:bulk 結果は当該周期でのみ使用し、周期をまたいだ再利用は禁止する（§4.5）。
- **SDK 等価性テストを標準装備**:fake provider 上で **`BulkObserve(scope) ≡ {Read(id) ∀ id ∈ scope}`** の等価性テストを SDK のテストハーネスに標準装備する。これを通ることが「完全階級 + bulk」を名乗る条件であり、バルク経路と個別経路が別の現実を返す実装バグを CI で捕まえる。

### §5.8 観測不能階級 —— observability:none / ledger_only

ブラウンフィールドには **Observe すら不可能な資産**（API を持たない旧機器、ドキュメントにしか存在しない配線、ベンダーロックで読めない箱）が必ずある。台帳上 **`unknown` のまま第一級で扱う資産階級**を型として定義する。

```yaml
resource: legacy_appliance
id: old-fw-01
observability: none          # Observe 手段が無い(Read も List も張れない)
ledger_only: true            # 台帳(intent)には存在するが、収束の対象外
last_verified: 2026-03-01     # 人が最後に目視確認した日(時間ベース health 述語と同型)
```

- **`observability: none` の資産は Read/Diff/Apply の対象にしない**（Observe できないものを収束しようとすると必ず事故る）。だが**台帳には第一級で存在させる**——「存在は知っているが観測できない」を `unknown` として明示的に持つのが `unknown ≠ absent` の台帳版である。
- **prune の巻き添えを防ぐ**:closed-world scope の List に現れない `observability: none` 資産を「理想に無い = 削除」と判定してはならない。ledger_only の資産は **List 集合演算から除外**する（現実を List できないのだから List 差分で消す根拠が無い）。
- **時間ベースの人手検証**:`last_verified` が古くなったら「人が目視確認せよ」を health 述語として上げる。観測できない資産の唯一の「鮮度」は人の確認である。

### §5.9 Executor seam —— 実行境界

`Provider` / `Lister` を束ねる実行境界。**コントローラはこの型にしか触らない**（DI で渡す）。

```go
type Executor interface {
    Observe(ctx, specs []Spec) ([]Observation, error)  // 内部で各 provider の Read を束ねて呼ぶ
    List(ctx, scope Scope) ([]ID, error)               // prune 対象 provider のみ有効
    Apply(ctx, plan Plan) (Result, error)
}

type LocalExecutor struct { /* providers */ }  // v1 唯一の実装:同一プロセス直接呼び
```

**守る規律は1つ**:コントローラのコードが `LocalExecutor` の存在を知らないこと。これさえ守れば、将来 `RemoteExecutor{conn *grpc.ClientConn}` を書いて差し替え口に挿すだけで分散が生える。プロセス境界は「隔離（障害・権限・言語）」で正当化されるのであってスケールのためではない（Observe は I/O バウンドで goroutine で足りる）。Ansible / Remote Executor は §18 / §28 を正とする。

---

## §6 3プリミティブ

収束（Resource）だけでは**手続き的作業**（migration / firmware 更新 / ローリングアップグレード / フェイルオーバー / ベアメタル導入）を表現できない。経路依存・非冪等・中断が致命的で、毎周回る収束ループに載せると破壊する。手続きの重さに応じて3層に分ける。

| プリミティブ | 扱うもの | 例 | state |
|---|---|---|---|
| **Resource** | 宣言的状態・収束 | DNS record / VM / volume / 設定 | 観測 state なし |
| **Task（単純型）** | 単一リソースの一回性手続き | migration | 進行フェーズのみ永続化（S2） |
| **Task（多相型）** | 状態機械・リブート跨ぎ | firmware 更新 | 同上 |
| **Workflow** | 順序 + 不変条件 + 手動承認ゲート | ローリング / DR | 進行位置 + invariant（S2） |

**共通原則:「いつ・どの順で変えるか」= 収束が判定。「どう変えるか」= Task/Workflow が実行。** 収束ループはトリガ・メンテ窓ゲート・依存順序・drift 検知・完了確認を担い、危険な手順そのものは Task/Workflow に閉じ込める。実行中の対象は `converging` 扱いにして収束の介入を止める。

### §6.1 Resource —— 宣言的・収束

宣言的状態を毎周期 Observe → Diff → Apply で収束する。観測 state を持たない（§4.9 S1）。全プリミティブの土台であり、Task / Workflow も内部では Resource の観測結果をゲートに使う。

### §6.2 Task —— 単純型 / 多相型 / 進捗

**単純型**（migration）:一度実行して completed 印を残す。完了印は**現実側**（DB 内 `schema_migrations`）に置けば Observe で問える=観測 state レスと両立する。

**多相型**（firmware 更新）:中断不可・長時間・リブート跨ぎ。実質ステートマシンである。

```
Idle → Staging(転送) → Verifying(hash/署名) → Applying(書込)
     → Rebooting → Confirming(バージョン再確認) → Done
                                            ↘ Failed → Rollback
```

- **進行中フェーズの外部永続化が必須**（S2）。ただし永続化するのは進行フェーズだけで、Done 判定は稼働バージョンを現実 Observe で問える。
- **リブート跨ぎ**:Applying 後に機器が消える → `unknown ≠ absent` で「リブート中」と解釈し、タイムアウト付きで再出現を待つ。Confirming で新バージョンを確認して初めて Done。
- **べき等な再開**:各相の入口で「もう済んでいるか」を現実 Observe で確認し、二重実行を防ぐ。

収束との接続（収束は判定とゲートのみ、手順は Task へ委譲）。差分検知しても直接 Apply せず Task を起動するだけ、というポリシーを Resource に付ける。

```yaml
resource: switch-fw
target_version: "24.2R1"      # 収束が監視する「あるべき状態」(機器ファームウェア版)
update_policy:
  strategy: task              # 差分検知しても直接 Apply せず Task を起動するだけ
  window: "Sat 02:00-04:00"   # メンテ窓外では起動しない
  rollback_on_fail: true
```

### §6.3 Workflow —— 順序 + 不変条件 + 承認ゲート

ローリングアップグレードは Task では足りない。集合をまたぎ、**動的な状態依存**（隣が今 healthy か）・**全体不変条件**（常に N 台 active / quorum 維持）・**部分失敗からの回復方針**を持つ。DAG は静的順序しか表せない。

```yaml
workflow: rolling-upgrade
target: cluster-db
invariant:                  # ★各ステップ前後で検証。破れるなら進まない
  min_active: 2
  quorum: maintained
strategy:
  order: one-at-a-time
  select: least-loaded-first
steps:
  - drain: "{node}"
  - failover_from: "{node}"
  - wait: { condition: role_moved, timeout: 5m }
  - firmware-update: "{node}"          # ← 多相 Task を内部で呼ぶ(階層構造)
  - health-check: "{node}"
  - undrain: "{node}"
  - verify: { invariant: min_active }
on_failure:
  policy: halt              # 既定は halt(止めて人間へ)。ローリングは全ロールバックが困難
  keep_drained: true
```

これは過剰ではない——HA 上げは本来的に「順序・不変条件・失敗回復」を持つ問題で、どのツールでも要る本質的複雑さである。それを第一級プリミティブとして構造化する。

### §6.4 承認ゲート

段階適用の承認ゲートは既存 Workflow（wave + invariant + Event/sink + フェーズ永続化）の組み合わせで表現でき、新プリミティブは不要である。ゲートは health_check（自動）と manual_approval（人）を持てる。

```yaml
rollout:
  stages:
    - name: canary
      target: { cluster: least-critical }   # 影響度順(明示指定)
      parallelism: 1
      gate:
        health_check: required               # 自動ゲート(conformance + health)
        manual_approval:
          requirements:
            quorum: { n: 2, of: 3 }          # N-of-M:3人の候補のうち2人の承認で通過
            roles: [platform-admin]          # 役割制約(役割判定は上位 IdP)
            separation: applicant_ne_approver # 申請者≠承認者を強制(4-eyes の中核)
    - name: broad
      target: { cluster: rest }
      parallelism: 5
      gate: { manual_approval: required }
  on_stage_fail: halt
```

**承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API → 次ステージ。タイムアウト（例 24h）で halt する。

- **API 形**:`POST /workflows/{id}/gates/{gate}/approve`（却下 = halt）。ボディに承認者 ID + 署名を載せる。ゲートは requirements を満たす承認が揃うまで「待ち」に留まり、揃った瞬間に次ステージへ進む。
- **承認 API 自体は認可を持たない**（誰が叩けるか・役割 X の判定は上位 IdP）。ゲートは「役割 X の署名が N 個必要」を requirements として保持するだけである（コアは認可を持たない・§7）。
- **申請者≠承認者（separation）**:PR や planned_change の申請者 ID と承認者 ID の不一致を強制し、自己承認を型で塞ぐ。
- **全承認者を S4 監査に記録**:誰が・いつ・どの署名で承認したかの全員分を S4 に append する。承認は「事実」として恒久記録され、「どの承認ゲートを誰が通したか」が追える。
- requirements を宣言しないゲートは従来どおり単一承認である。

### §6.5 RequiresTaskEvent —— 非冪等操作の収束ループからの排除

**非冪等な操作を毎周回る収束ループに載せてはならない。** 収束ループは冪等性（同じ Apply を何度実行しても同じ結果）を前提に毎周期 Apply を叩くため、非冪等操作（migration・firmware 書込・フェイルオーバー等）を Resource として直接 Apply させると、毎周期の再実行が破壊を招く。

そこで、非冪等操作を要する Resource は **`strategy: task` を宣言**し、収束は「差分を検知したら Task を起動する」というイベント発火だけを担う（§6.2）。収束ループが直接手を下すのは冪等な操作に限り、非冪等な手順は Task/Workflow というイベント駆動の手続きに排除する。これが「いつ = 収束 / どう = 手続き」の分離の型レベルの実装であり、収束の冪等性契約（§5.4 Apply）を非冪等操作から守る仕組みである。

---

## §7 所有と境界

### §7.1 ownership 2層が貫く7つの関心事

所有権は2層である。この1軸が、書込経路から blast-radius まで貫く。

- **`ownership: platform`** … 共有基盤（クラスタ / NW コア / 共有ストレージ）。誰か一人の所有ではない。platform 層の理想状態に**一度だけ宣言**する（三重定義を防ぐ）。
- **`ownership: tenant`** + **`tenant: <name>`** … 専有（VM 等）。

`ownership` が決めるものを表にする。これが原則6（ownership がすべてを貫く）の実体である。

| 関心事 | tenant | platform |
|---|---|---|
| 1. 書き込み経路 | **即時（レビュー無）** | **PR 必須（人が承認）** |
| 2. remediation | auto でよい | alert-only 寄り |
| 3. rate limit | 緩め（warn） | 厳（halt） |
| 4. 爆発半径 | 小（砂場内） | 大（全体） |
| 5. 供給源（source） | DBSource | FileSource（git） |
| 6. シャード境界 | scope で砂場を切る | 共有上流 |
| 7. prune 境界 | 自テナント所有分に限定 | platform 層に閉じる |

テナントの closed-world scope は**自テナント所有分に限定**され、**prune が共有基盤を消さない**。テナントは共有基盤を **placement 参照**するが**所有しない**。DAG は**扇状**（共有基盤 = 全テナント共通上流、テナント間はエッジなし = 独立・並列）。

**テナントがレビュー不要な理由**:scope で砂場境界を切ってあり、他テナントも基盤も壊せない。テナント内でレビューが要るかはテナントの自己責任である。プラットフォームは砂場の境界だけ保証し、砂場内の統治は委ねる。テナントが基盤に触れる操作（採番消費・容量消費）は、**authority（採番）と quota（容量）という事前に区切られた枠**を通すことで即時のまま安全であり、枠を超える要求だけ platform 層の承認に上がる。認可・quota・課金・セルフサービス窓口はコアの責務ではなく、上位プラットフォーム / IdP に委ねる（コアは論理分離の境界保証だけを担う）。

### §7.2 managed fields —— 外部システムが動かす欄の diff 除外

外部システムが動かすフィールド（DRS の vMotion・動的 IP・HA 電源状態）を「理想と違う」と収束すると、プラットフォームと**永久に喧嘩**する（非ゼロ diff が恒常化する）。**Schema で `managed` 宣言したフィールドを diff 対象から自動除外**する（Terraform `ignore_changes` / k8s server-side apply 相当）。

```yaml
resource: vm
ownership: tenant
tenant: acme
id: web-1
placement:
  cluster: prod-cluster                 # 依存先はクラスタ(プール)。intent が管理する
  demand: { cpu: 4, memory: 8 }         # 要求する容量
  # 現在どのホストに居るかは runtime = managed field。理想状態に書かない
```

宣言した intent のフィールドだけを管理し、runtime placement 等は無視する。これはフィールド単位の除外であり、次の `external_owner`（scope 単位の除外）と対をなす。

### §7.3 external_owner —— 外部オーケストレータ所有 scope の除外と一方向協調

UniFi・DRS/ACI・Heat/Neutron・k8s operator・MHA・VNFM・AWS マネージド等、**内部に自前の収束ループを持つ外部オーケストレータ**が管理対象の一部を所有する構成がある。managed fields（フィールド単位）では足りず、**scope / リソース種別単位の除外**を第一級で持つ。

**（a） 外部所有 scope の除外宣言。** ある scope / フィールド群を「外部オーケストレータが所有する」と宣言し、**Sakutei は Observe（可視化）はするが Apply しない**（収束対象から外す）。

```yaml
scope: { cluster: prod, subsystem: vm_placement }
external_owner: drs            # DRS が所有。Sakutei は Observe するが Apply しない
```

除外宣言は理想状態ロード時に検証する（prune・循環検出と同じ入口）。v1a では最も基本の "alert-only 相当の Apply 抑止" として入れる。「この収束器は触らない」を宣言的に固定するのが要点である。

**（b） 「観測して待つ」一方向協調 seam。** 縁切りだけでなく、外部の収束結果を **`ready_condition`（§6, §11）として Observe で待つ**ことで協調する（強い制御でなく結果整合）。例:MHA がフェイルオーバーを完了したことを Observe し、その ready を下流の gate にする。**Sakutei から外部へ指示は出さない**（外部の内部ループには介入しない）——協調は「観測して待つ」の一方向に閉じる。これはシャード間依存（観測 ready 待ち）と同じ機構の再利用であり、新プリミティブは要らない。

`ready_condition` の連携は、provider が `Observation.Ready` に名前付き condition の充足状況を返し、コントローラの依存解決（`canApply`）が上流の `Ready[condition]` が真になるまで下流を skip し続けることで成立する。これは失敗伝播・下流 skip と同じ機構であり、外部リソースにも張れる。

### §7.4 scope + managed による境界宣言

境界の宣言は2つの粒度を持つ。**scope**（§4.2）がリソース種別・所有単位の境界を切り、**managed**（§7.2）がフィールド単位で「触らない欄」を切る。両者を組み合わせることで、「この scope は DRS が所有（`external_owner`）、その中でこのフィールドは runtime（`managed`）」というように、外部世界との境界を宣言的に固定できる。prune・循環検出・境界検証はすべて理想状態ロード時の同一入口で行う。

### §7.5 合成リソースの子への ownership 伝播（managed_by）

合成リソース（§9 render フェーズ2 で下位リソース群を上位1つに束ねる）は、**下位を別 Resource として依存グラフに乗せる**（1つの provider に押し込まない）。子が独立した Resource だからこそ、子 50 個の削除を「1 リソースの変更」ではなく「50 件の削除」として rate limit（§13）で halt できる——合成を単一 Provider にすると安全機構が効かなくなる。

このとき**子は親の ownership を継承する**。子リソースは親合成リソースに `managed_by`（どの親が展開した子か）を持ち、これにより:

- **上位削除 → 下位を巻き取り**:親が削除されると、`managed_by` で結びついた子が所有権に従って巻き取られる（`pruneable` の判定も親から伝播する）。
- **所有境界の一貫性**:親が `ownership: tenant` なら展開された子も同じ tenant scope に属し、prune 境界（§7.1）・書込経路が親から一貫して決まる。人が子を個別に intent へ書いたわけではないので、子の所有は親から機械的に導出される。
- **粒度の保持**:`managed_by` は所有の伝播だけを担い、drift 個別検知・原因タグの粒度・部分失敗の扱いは子ごとに保たれる（子が独立 Resource である利点）。

これにより、合成リソースは「見せる面は1つ」でありながら、内部では所有・安全・粒度が親から子へ一貫して伝播する。


# 第III部 収束機構

収束機構は、理想状態（desired）を現実（actual）へ寄せる周期処理の本体である。第II部で定めた正準型と
Provider 抽象を土台に、本部は「いつ・どの順で変えるか」を決める四つの機構を規定する——収束ループ（§8）、
desired を生成する render パイプライン（§9）、差分とその原因の帰属（§10）、そして一周期で終わらない収束の
扱い（§11）である。安全機構（第IV部）はこの上に載る手綱であり、本部は「速く収束する力」そのものを定義する。

貫く二つの規律を先に置く。第一に **level-triggered な真実**——毎周期 desired と actual を再導出し、観測状態を
保持しない。第二に **観測 state レス**——キャッシュした「現在状態」を持たず、毎周期リアルを再観測する。この
二つが崩れると、本部の後続すべて（dry-run の正しさ、原因タグの帰属、drift 検知漏れゼロ）が成立しない。

---

## §8 収束ループ

### §8.1 周期ループの全景

収束ループは単一デーモン内で回り続ける単一のループである。一周期（Cycle）の骨格は次のとおり。

```
毎 Cycle:
  1. render(§9)で desired を生成(rendered_revision 確定)
  2. desired のリソース集合を Observe + prune 対象 scope を List
       (全並列・semaphore で同時度を制限)
  3. Diff(集合 diff → フィールド diff)→ 変更が要るノード集合 D
  4. D の部分 DAG を構築
  5. Kahn 法で wave に分解:
       while 残ノードあり:
         wave = 入次数0 かつ 上流が成功済み
         wave 内を並列 Apply(非同期・§8.4)
         失敗ノードの下流を skip 集合へ
  6. Diff の推移を記録(振動検知・ベースライン・§11.3)
  7. jitter 付き sleep → 次 Cycle
```

**render がループの先頭に来る理由**:「何を Observe するか（リソース集合）」も「どの scope を List するか
（prune 対象）」も desired から導かれる。desired が確定していない時点では「全リソース」を定義できない。render
入力は TTL キャッシュ（§9.4）なので、先頭に置いても毎周の外部呼び出しは増えない。

**Observe は全並列**。goroutine で並列化するため、Observe のコストは逐次和ではなく **最遅 Observe に漸近**する。
同時度は相手 API への礼儀として semaphore で上限を課す（既定 16）。

```go
sem := make(chan struct{}, 16)  // 同時 Observe 上限
```

**依存の与え方**は明示 `depends_on`（将来は参照からの暗黙抽出）。設定ファイル上のキーは snake_case
（`depends_on`）に統一し、コード内フィールド名（`DependsOn`）との対応は Loader（§21）が変換する。
**循環検出は理想状態ロード時に一度**行い、循環していればそのリビジョンを入口で拒否してループに持ち込まない。
基盤サービス（DNS/DHCP/NTP）は全リソースが暗黙依存する「依存の根」であり、全部に `depends_on` を書くのは
非現実的なので、粗い優先度帯 **`tier: foundation`**（基盤層は常に通常層より先）で順序を与える。

### §8.2 モデルA:毎周・全 DAG の採用

本システムは **毎 Cycle で全 DAG を回す**（モデルA）。リソース単位の独立コントローラ + 依存 gating（k8s 流）
ではない。理由は規模にある。本システムの想定規模（数十〜数千リソース）では、**plan が常に全体で正しい**ことの
価値が勝る。リソース単位ループは k8s 級の規模で初めて元が取れる。

**Observe は毎周・全部・間引きなし**。毎周全部見ることが観測 state レスの価値の源泉（drift 検知漏れゼロ）で
あり、間引きは本末転倒である。この毎周・全 DAG が **level-triggered な真実の本体**である。§8.6 の早期収束
パスはこれを置き換えず、上に重ねる——トリガで早起きして影響サブ DAG だけを先に収束させても、正しさの保証は
あくまでこの定期周期が担う（トリガを取りこぼしても次の定期周期が拾う）。

**失敗は下流へ伝播する**。依存が失敗・未達なら下流を skip し、次 Cycle で回収する。level-triggered なので
「今回諦めて次で回収」が自然に効く。全停止（Ansible の `any_errors_fatal` 的）より、部分適用 + 自己回復が優る。

**割り切りが破綻する早期警戒線**（立つまで間引き・TTL・頻度別スケジュールは不要）:

- Observe 全体が reconcile 間隔に対し無視できない比率（例:間隔 30s に対し 10s 超）
- 特定プロバイダの API 課金 / rate limit が Observe 頻度で効く
- リソース数が桁で増えた（数百→数千）

破綻したときの延命策は §16。

### §8.3 wave は Cycle 内の並列単位

**wave は「1 Cycle 内の並列処理単位」であって「依存解決の最終形」ではない。** 物理サーバの起動・OS
インストール・image DL のような長時間操作は 1 Cycle で終わらないため、**依存の解放は複数 Cycle に跨って
段階的に進む**のが正式なモデルである（§8.4 の in-flight と §11.1 の converging の組み合わせで扱う）。

そのため依存宣言は「存在するか」ではなく「**特定の観測可能な状態に達したか**」を条件にできる。

```yaml
depends_on:
  - type: proxmox_vm
    id: http-boot-1
    ready_condition: cloud_init_done    # 単なる存在ではなく、この condition の充足を待つ
```

provider は `Observation.Ready`（§4）に名前付き condition の充足状況を返す。コントローラの `canApply` 判定は、
上流の `Ready[condition]` が真になるまで下流を skip し続ける——これは §8.2 の失敗伝播・下流 skip と **同じ機構の
再利用**であり、新しいプリミティブは要らない。毎 Cycle 再評価されるため、条件が満たされた Cycle で自然に下流が
解放される。ready_condition の多周期収束としての位置づけは §11.2 に述べる。

### §8.4 非同期 Apply と in-flight 集合

VM 作成・image DL・firmware など分単位の Apply が wave に入ると、素朴なモデルは「ループが止まって他の drift
検知が遅延」か「Cycle を重ねて同一グラフへ並行 Apply で競合」のどちらかに陥る。実運用では **Observe コストより
先に Apply 時間が刺さる**。→ **Apply だけを Cycle から切り離す**。

```
・Observe/Diff ループは毎周・定刻で回り続ける(他リソースの drift 検知が長 Apply に巻き込まれない)
・Apply は goroutine で非同期実行し、対象リソースを in-flight 集合に入れる
・in-flight 中のリソースは converging と同じ扱い(再 plan・再 Apply しない、下流は skip → 次周回収)
・Cycle は重ねない(前周の Observe/Diff 完了後に次周)。長いのは Apply だけが継続
・in-flight 集合はインメモリのみ(S3)。再起動で消えても Apply 冪等 + 次周 Observe で安全に再導出
```

wave/skip セマンティクスに「Apply 完了は Cycle 内でなくてよい」を足すだけで、モデルの構造は不変である。
**Cycle は積み重ならない**——前周の Observe/Diff が終わってから次周が始まる。長時間化するのは Apply だけであり、
これは Cycle の外で継続する。

**per-resource in-flight 排他**:同一リソースに対する Apply は同時に一つだけ。定期経路と早期収束パス（§8.6）は
この排他を共有し、両経路が同一リソースへ同時に Apply することはない。rate limit（§13）は in-flight を含む同時
変更数で評価する——さもないと「毎周少しずつ発射し続けて in-flight が積み上がる」抜け道になる。

### §8.5 per-resource backoff

Observe は毎周続ける（安価・drift 検知を維持）が、恒常失敗リソース（権限欠如等）を毎周 Apply で叩くのは相手
API への攻撃になる。**（リソース, エラー種別）単位の指数 backoff + jitter** で Apply をゲートする。

- **transient**（API 一時エラー・ロック競合）→ backoff（上限 15 分程度）
- **permanent**（権限・バリデーション）→ 即 **stuck** 化し、**desired か provider_version が変わるまで
  再試行しない**（原因タグの再利用）

backoff テーブルは S3（インメモリ）。再起動で消えても一回余分に叩くだけである。

### §8.6 早期収束パス（トリガ収束）

本システムは level-triggered ゆえ、収束は **周期律速**である。定期周期（§8.1）は「drift 検知漏れゼロ」を
保証する一方、**人の意図変更（特に tenant 層のセルフサービス API）の反映が次の周期まで待たされる**という構造的
レイテンシを持つ。これに応えるため、**トリガによる早起き（edge ヒント）+ 定期周期（level 保証）** のハイブリッドを
第一級化する——「**edge-triggered な起床、level-triggered な真実**」。k8s の informer watch（edge ヒント）+
resync（level 保証）と同型である。

**貫く原則:トリガは「いつ収束するか」のヒントであり、「何が真実か」を一切運ばない。**

- **トリガ喪失は正しさを損なわない**（遅延のみ）。ヒントを取りこぼしても次の定期周期が必ず拾う。
- 早く起きた収束の中身は通常と同じ新鮮な Observe → Diff → Apply である。トリガはペイロード（変更後の値）を
  運ばず、「この scope が触られたらしい、今見に行け」としか言わない。**観測 state レスは無傷**——トリガに含まれる
  情報を「現在状態」として信じることは一切しない。

**入口は `DesiredSource.Watch`（§21）**。`DesiredSource` に任意 capability として `Watch` を足す（`Provider` に
対する `Lister` と同じ宣言制）。`Watch` は `ChangeHint = {intent_revision, 触られた scope 群}` のみを流す。
FileSource なら git webhook / merge フック、DBSource なら書込通知が発火源である。

**部分収束（scoped reconcile）**:トリガを受けたら全 DAG を回さず、影響サブ DAG だけを収束する。

```
ChangeHint 受信:
  1. render(§9)で新しい rendered desired を生成
  2. 新旧の rendered desired 同士を diff(★決定的・リアル非接触。ここでは Observe しない)
  3. desired が変わったリソースから影響サブ DAG を算出
  4. そのサブ DAG だけを Observe → Diff → Apply(通常の wave/skip セマンティクスをそのまま適用)
```

新旧 desired 同士の diff はリアルに触れないため安価で、**制御プレーンの追加レイテンシは秒オーダー**に収まる。
影響サブ DAG の算出は「新旧 rendered の desired 同士の決定的 diff」から出発する点が要である——ここで Observe を
挟まないから、速い経路が観測 state を再導入しない。

**安全柵（速い経路を汎用修復チャネルにしない）**:

- **速い経路は「desired が変わったリソースのみ」を扱う**。新旧 desired diff から入るため、構造上 **原因タグ
  A（意図）/ E（外部入力）のみ**が対象になる。道中で偶然見つけた **drift B は速い経路で修復せず、定期ループに
  残す**（B は周期律速のままでよい——速い経路の役割純化を優先）。
- **集合差 prune は速い経路で禁止**。prune の正しさは List 完全性（§12）に依存し、部分観測とは相容れない。
  テナントの即時削除は **DBSource 側の tombstone = 明示 absent**（`ExplicitAbsentOnly` の型・§12）で表現し、
  個別リソースの削除として扱う（集合差ではなく）。tombstone は個別リソース削除として速い経路で即時に処理できる。
- **rate limit・累積破壊予算のカウンタは定期経路と共有**する（§13）。速い経路がバイパスになってはならない——
  両経路は同一の予算・同一の per-resource in-flight 排他（§8.4）を共有する。
- **同一 scope へのヒントは合流（coalesce）する**。実行時に **最新の desired を読む**ことで level 意味論を保つ
  （古いヒントで古い desired を適用しない）。scoped run の同時実行数には上限を設け、**溢れたヒントは次の定期周期へ
  縮退**する（速い経路が詰まっても正しさは定期周期が保証する）。合流キューは最新 desired を読む一点に集約する。

**ownership との整合**:

- **tenant 層（即時書込）が主客**。セルフサービス API の変更が秒で反映され、UX が周期待ちから解放される。
- **platform 層は「PR マージ = ヒント発火」**。承認直後に収束が起きる（ガバナンスは不変・レイテンシのみ短縮）。
  承認ゲート・PR 必須（§7）はそのまま効く。

**plan と reconcile はそれぞれ独立に Observe する規律**は速い経路でも守る。トリガで起きた scoped run も、plan の
観測を reconcile に持ち越さない——持ち越すと排除したはずの観測 state を再導入することになる。

**注意点**:render フェーズ1 の副作用（採番・§9.1）が速い経路にも乗るため、**allocator の可用性が UX に
直結する**。速い経路で新規払い出しが要る変更は allocator 応答が律速になる（§9.3 の不達時は前回保持で安全側だが、
UX としては遅延として現れる）。allocator 停止中は当該変更が `waiting-allocation` へ縮退し（§19）、ヒントはキュー
保持されて allocator 復帰後の周期で払い出される（正しさは損なわれず、UX 遅延のみ）。定常収束は衛星コールゼロで
回るため、速い経路で allocator が要るのは「新規採番を伴う変更」だけである（§19）。

---

## §9 render パイプライン

desired の生成は、**複数の外部権威に依存し、副作用を持ち、毎周走る多段パイプライン**であり、収束ループと同格の
複雑さを持つ。第一級コンポーネントとして扱う。

### §9.1 二フェーズ

```
render = フェーズ1(外部入力解決:★副作用あり・外部権威に依存)
           ├ inventory 合成(外部 CMDB)
           ├ シークレット世代解決(Vault metadata:値は読まない)
           └ allocation 解決(AllocationAuthority への払い出し)
       + フェーズ2(決定的展開:副作用なし・同じ入力 → 同じ出力)
           └ 合成リソースの Expand(virtual_network → vxlan + vds + dhcp)
```

**フェーズ1 は外部入力解決**であり、副作用を持ち外部権威に依存する。inventory を外部 CMDB から合成し、
シークレットの世代を Vault metadata から解決し（値は読まない）、allocation を AllocationAuthority から払い出す。
**フェーズ2 は決定的展開**であり、副作用を持たず、同じ入力に対し常に同じ出力を返す。合成リソースの Expand は
このフェーズに属する。**Expand の居場所は render フェーズ2**（収束ループ内の入れ子コントローラではない）。
決定性の要求はフェーズ2の規律として一括で課す。

### §9.2 外部権威到達不能時は前回解決値を保持

**どの外部権威が不達でも「前回解決値を保持」する。空の desired を生成しない**（全削除事故の防止）。これは
Observe 側の `unknown≠absent` と対をなす、desired 側の非対称原則である。

| 外部入力 | 不達時 |
|---|---|
| inventory（CMDB） | 前回生成物を保持 |
| シークレット世代（Vault metadata） | 前回世代を保持 |
| allocation authority | 前回割当を保持（**新規払い出しは保留** → 未割当リソースは blocked で次周回収） |

Vault が sealed-restart 中は世代解決も死ぬため、SecretRef のキャッシュは **値だけでなく世代 metadata も含む**
（§20）。

**incomplete≠empty（部分成功の防御）**:「不達 → 前回保持」は二値であり、「200 OK だが中身が縮んで返る」
（CMDB のレプリカ遅延・部分障害）を扱えない。歯抜けの inventory で render すると、欠落分が「理想に存在しない」と
展開され closed-world prune で削除対象になる——空 desired ではないので「全削除事故防止」もすり抜ける。

- **render 入力に「期待基数 / 前回比の急減検知」を入れる**。前回 N 件だった inventory が今回 X%（既定 50%）未満に
  急減したら、それを「不達」と同じく前回保持側へ倒す（または halt + Event）。
- render 出力に対しても、削除方向 Diff の急増（§13）を専用に監視する。

### §9.3 TTL キャッシュと平常時の頻度規律

render が毎周実行になる結果、Vault metadata 読み（シークレット数ぶん）と authority Lookup（割当リソース数ぶん）が
**毎 Cycle の外部呼び出し**になる。→ **render 入力は TTL 付きキャッシュ**（staleness 上限 = TTL）。障害時挙動
（§9.2）と合わせ、平常時・異常時の両方の規律が揃う。TTL キャッシュのヒット/ミスや列挙順で rendered_revision の
値が揺れてはならない（§10.1）——揺れると intent も現実も変わっていないのに E と誤判定し、無用な halt を招く。

### §9.4 intent_revision と rendered_revision

```
intent_revision   … 人が書いた理想(git commit hash / DB トランザクション ID)
rendered_revision … render 出力の世代(intent + 外部入力の解決結果すべてのハッシュ)
```

**desired は人の commit なしでも変わる**（シークレット rotate・割当変更・inventory 変更）。この二つを型で分ける
ことが、原因タグ（§10）の判定基盤になる。`DesiredSource.Fetch()` が返すのは intent_revision、収束が比較するのは
rendered_revision である。**rendered_revision は scope 単位で算出する**——scope をまたいで一括ハッシュ化すると、
無関係な scope の外部入力変化が全体の E 判定を汚す。

`rendered_revision` は **decision-relevant な入力のみの決定的ハッシュ**とする。TTL キャッシュのヒット/ミスや
列挙順で値が揺れると E と誤判定し、逆に外部入力の変化がハッシュに反映されない実装バグは E を B と誤判定して、
rotate したシークレットを古い世代へ強制的に戻す。したがって「何を含め何を含めないか」と「TTL ヒット/ミスで
揺れないこと」を厳密に規定する。

### §9.5 合成リソースは実子リソースに展開する

「VXLAN 設定 + VDS 設定 → ユーザには仮想ネットワーク一つ」のように、下位リソース群を上位一つに束ねる。IaaS
カタログの各商品はこの合成リソースとして実現される。

```yaml
resource: virtual_network        # ユーザが書く(見せる面)
tenant: acme
id: app-net
cidr: 10.50.0.0/24
# ↓ render フェーズ2 が展開(内部)
#   → vxlan_segment{ vni: 5010, ... }
#   → vds_portgroup{ vlan: ..., uplink: ... }
#   → dhcp_scope{ ... }
```

**下位を別 Resource として依存グラフに乗せる**（一つの provider に押し込まない）。VXLAN と VDS は異種システム
（NSX / vCenter）を叩くため、単一 Apply に混ぜると部分失敗の扱いが破綻する。別 Resource なら、展開は扇状 DAG
（wave / 依存 gate がそのまま効く）になり、下位の drift を個別に検知・修復でき、上位削除で下位を巻き取り（所有権・
`pruneable`・§12）、原子性が要る下位群は「1 wave で原子的に」指定できる。

**採らない代替案:合成リソースを単一 Provider として実装する**（子を内部に隠蔽し Apply 時に展開する）。一見
「controller は基本か合成かを意識せず扱える」利点があるように見えるが、**安全機構が効かなくなる方向に働く**。
合成が一つの Provider になると、コントローラから見える変更は一件になり、子 50 個の削除が「1 リソースの変更」として
rate limit（§13）を通過してしまう。**子が独立した Resource だからこそ「50 件の削除」として halt できる。** 同じ
理由で、下位ごとの drift 個別検知・原因タグの粒度・部分失敗の扱いも失われる。→ **render フェーズ2 で展開し、子を
理想状態に実在させる方式を正とする**。

**展開ロジック**はテンプレート（inventory データ + テンプレート）による展開を方式とする。単純な変数展開を超える
処理（プール採番・CIDR 計算・tier 別の自動割り当て等）は、テンプレートエンジンのカスタム関数、または展開後に走る
処理層で埋める。カスタム関数は **決定性を保つ**こと（同じ入力 → 同じ出力・§9.1）が要件で、その担保は
rendered_revision 算出定義に含める——決定性は原因タグ E の判定基盤である。将来的には Loader（§21）を CUE に
差し替えることで、基本リソースと展開ロジックを同一言語で書けるようになる。

### §9.6 generated/ 規約

合成リソースの展開結果を明示的に生成物として書き出す経路では、出力先を `generated/` に限定する。`generated/` は
**自動生成専用**であり、自動生成ヘッダを必須とし、直接コミットを拒否する（二重管理防止）。人が書く intent の出力先は
`intent/` であり、両者を型レベルで分離する。

### §9.7 ステータス集約（最悪優先）

合成リソースの上位ステータスは、**最も悲観的な下位が上位を支配**する（fail-safe・非対称原則の再適用）。全順序を
最悪優先で定める。

```
unknown > stuck > oscillating > drifted > blocked > converging > converged
```

`stuck`（permanent 失敗・§8.5）は最悪級（人手を要する確定失敗）なので `unknown` の次に置き、`blocked`（依存未達）は
「上流待ち=一時的」なので `drifted` と `converging` の間に置く。これで報告用の Event 分類（§26）と収束ステータス
集約の軸が一致する。どの下位が原因かは追跡できる。この全順序は catalog（§21）の UI 表示、`sakutei status`
出力（§10）、多周期収束の判定（§11）で共通に用いる。

---

## §10 差分と原因

### §10.1 Observation と ConvergenceStatus の型分離

`drifted / converged` は desired との比較結果=**Diff の出力**であって、Observe（現実を見るだけ）では判定できない。
混ぜると Observe が desired を知る必要が生じ、**dry-run の正しさの根拠**（Observe/Diff 分離）が崩れる。

```go
type Observation struct {
    Availability Availability     // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State            // Availability == ok のときのみ有効
    Ready        map[string]bool  // 名前付き ready condition の充足状況(§8.3, §11.2)
    ObservedAt   time.Time        // この観測を取った時刻(周期越え再利用の検知に使う)
}
```

収束ステータス（報告用）はコントローラが **導出**する。

- `unknown` / `converging` … Observation から
- `converged` / `drifted` … Diff 結果（Plan が空か否か）から
- `oscillating` … 履歴から（§11.3）

**Observe は desired を知らず、Diff は現実を触らない**が型で保証される。

### §10.2 変更の原因区別（A〜E）

収束ループが見るのは「desired ≠ actual」という **結果**だけで原因を区別しない。だが同じ差分でも **なぜ生じたかで
危険度と対応が正反対**である。diff 結果に **原因タグ**を持たせる。

| 原因 | 判定 | 対応 |
|---|---|---|
| **A. 意図的**（人が理想変更） | intent_revision が変化 | 適用してよい（規模制御は効かせる） |
| **B. drift**（現実が乖離） | 消去法（diff が存在し、intent_revision / provider_version / rendered_revision がいずれも不変） | 自己修復 or アラート（remediation mode） |
| **C. ツール更新**（解釈が変化） | provider_version が変化 | 最も危険・見逃されやすい（ghost diff） |
| **D. カスケード**（A/E の依存波及） | 上流の変化に連鎖（DAG 下流） | plan 段階で波及範囲を可視化 |
| **E. 外部入力変化** | rendered_revision は変化したが intent_revision は不変 | 下記 |

- **B は消去法で判定する**:観測 state を持たないため「actual が変化した」こと自体は観測できない（前回の
  actual を保持していない）。diff が存在し、他の三つの revision がいずれも動いていなければ、現実側が動いたと
  結論する。
- **C（ghost diff）**:provider の正規化ロジックを直しただけで、理想も現実も変わっていないのに既存全リソースが
  「今の解釈と違う」と判定され再適用される。**正しい修正でもバグでも同じ見た目**。→ provider_version を diff
  計算の入力に含める。C を検知したら自動適用せず `ProviderUpgradeDiff` Event を発火し、dry-run で人が確認してから
  解禁する（twin（§26.2）への自動流し込みを既定運用に組み込み、ghost diff を本番前に観測する）。
- **E のサブタグ**:**E-secret**（世代 rotate）/ **E-alloc**（intent 不変のまま割当だけ変わった場合——リソース
  追加に伴う新規払い出しは intent_revision が変わるので A）/ **E-inventory**（CMDB 変更）。
- **E の扱い**:帰属は「人の commit 由来ではない」と記録（A と混ぜない）。rate limit は不明度が高いので破壊的側に
  重み付けする。
- **D の価値**:「image 一つ更新 → 依存 50VM が再作成」のようにカスケード規模が直感を超える。

### §10.3 原因タグは per-resource で帰属する

原因タグは **per-resource で確定する**。Cycle グローバルの revision（「intent_revision が変わった Cycle は全部 A」）で
語ると、**1 Cycle 内に A と E が混在するとき、E 由来（CMDB バグ）の破壊が無関係な人の小 commit に「相乗り」して
A の緩い扱いで通過する**穴がある。これを塞ぐため、**各リソースの diff 入力ハッシュを「intent 由来部分 / 外部入力
由来部分 / provider_version 由来部分」に分解し、どの部分が動いたかでタグをリソース単位に決める**。グローバル
revision は「候補の絞り込み」にのみ使い、最終帰属は per-resource-diff で確定する。これは `Plan.Op.Cause`（§4）が
原因タグを持つことで実装する。

### §10.4 provider_version の diff 埋め込みと fail-closed

C の検知は「provider_version を diff 入力に含める」に全依存する。埋め込みの付け忘れが「C 検知漏れ=ghost diff が
B に化けて自動適用」という **危険側に倒れる**緩い契約であってはならない。またモノリシックバイナリでは
provider_version = reconciler バイナリ version になり、本体の無関係な更新で全リソースを誤検知しうる。

- version の粒度は「**provider の diff/正規化ロジックのソースハッシュ**」を SDK（§28）が自動算出する（手動 version
  でなく）。reconciler 本体 version とは分離する。これで誤検知（本体更新）・検知漏れ（付け忘れ）を両方防ぐ。
- **version 未埋め込みの provider は起動時に登録を拒否**（fail-closed）。Schema lint（§28）に「version 未宣言」を
  加える。

### §10.5 last-seen リング（B の「なぜ」追跡）

運用者の最頻の問い「なぜ変わろうとしているのか / いつからズレたのか」に、B は「消去法（否定形）」でしか答えられ
ない——観測 state レスの代償で「actual がいつ変わったか」を原理的に言えなかった。→ **直近 N 周の actual
スナップショットを S3（インメモリ・非永続）にリングで保持し、drift 検知時に「前周は X だった」を Event に添える**。
喪失しても安全側（S3 の定義通り数周鈍るだけ）であり、**永続化しない限り観測 state レスの看板は守れる**。これを
`sakutei status` の「なぜ変わるか」表示（原因タグ + 直近スナップショット差分 + 下流波及 D + provider 階級）と
一体で提供する。

### §10.6 content-address 参照

image 等の参照は **名前ではなくハッシュで参照する**（`image@sha256:...`）。名前参照だと「同じ名前だから変わって
いない」と誤認し、古い image の VM が残る。**content-address が drift 検知を正しくする**——参照そのものが内容の
ハッシュなので、内容が変われば参照が変わり、Diff が正しく差分を検出する。これは型に食い込むため中核で導入する。
ready 判定も存在ではなくチェックサム/署名一致で行う（破損・改竄・途中失敗を ready と誤認して VM を産まないため）。

---

## §11 多周期収束

収束は一周期で終わらない（§8.3）。相手が内部に収束ループを持つ場合、依存が観測可能条件の充足を待つ場合、
そして自分と相手が綱引きして発振する場合——これらを扱う機構を本節に集める。

### §11.1 converging の三層検知

converging は「収束器の上の収束器」（SDN / VCF / アレイ / k8s は内部に収束ループを持つ）を扱う上で最重要だが、
多くの API に「作業中」の概念がない。**三層 + deadline** で扱う。

- **Tier1 ネイティブ**:vCenter task / NSX realization 等を写像する（`Capabilities.Converging = true`）。相手が
  「作業中」を返せる場合、そのまま converging として尊重する。
- **Tier2 settle 猶予（普遍的な床）**:自分の Apply 直後は型ごとの `settle: 30s / N サイクル` の猶予窓を置き、その間の
  不一致を drifted 扱いしない。**検知手段ゼロの provider でも成立**する。相手が「作業中」を返せなくても、自傷猶予で
  一時的な不一致を drift と誤認しない床が張れる。
- **Tier3 発振からの推定**:M 回再 Apply しても Diff が縮まない → `oscillating` 判定し、そのリソースを **自動で
  alert-only に隔離**する（綱引きの自動検疫）。発振は「自分の Apply と相手（他システム・人）の変更が互いを打ち消し
  合う」状態であり、放置すると毎周無駄な Apply を撃ち続けるため、自動隔離で連鎖を断つ。
- **deadline 必須**:converging には timeout を付け、超過で `drifted` + Event に昇格する（相手ループの stuck を
  取りこぼさない）。全て S3（インメモリ）で足りる。

三層いずれも報告用ステータスは §9.7 の全順序に載る（`oscillating > drifted > converging`）。

### §11.2 ready_condition（観測可能条件への依存）

依存は「存在するか」ではなく「**特定の観測可能な状態に達したか**」を条件にできる（§8.3）。provider は
`Observation.Ready`（§10.1）に名前付き condition の充足状況を返し、コントローラは上流の `Ready[condition]` が真に
なるまで下流を skip し続ける。毎 Cycle 再評価されるため、条件が満たされた Cycle で下流が自然に解放される。

```yaml
depends_on:
  - type: bare_server
    id: leaf03-node1
    ready_condition: data_network_verified   # 観測可能条件の充足を周期をまたいで待つ
```

これは **周期をまたぐ収束**の中核である。物理サーバの立ち上げ（`bare_server` 電源投入 → `installed` →
`data_network_verified` → `proxmox_node` クラスタ join）のように、1 台の立ち上げが数十 Cycle に跨って進行する。
各段の間の待ちは、§8.2 の失敗伝播・下流 skip と同じ機構で表現され、新しいプリミティブを要さない。

**シャード間依存の「観測 ready 待ち」への接続**は §18 を参照。あるシャードのリソースが別シャードのリソースの
ready を待つ場合も、この ready_condition の意味論をシャード境界越しに適用する。

### §11.3 oscillation 検出（Diff 縮小の記録）

発振の検出は **Diff の推移の記録**に基づく。収束ループは毎 Cycle で Diff の推移を記録し（§8.1 の手順6）、同一
リソースに対し M 回再 Apply しても **Diff が縮まない**ことを発振の兆候とする。振動検知が「Diff が縮まない」を見るのに
対し、異常検知（§13）は「Diff が突然巨大化した」を見る——同じ Diff 時系列監視の別用途である。

- Diff 縮小の履歴は S3（インメモリ）に保持する。喪失しても検知が数周鈍るだけ（安全側）。
- 発振と判定したリソースは §11.1 Tier3 に従い自動で alert-only に隔離する。
- ベースライン（平常時の変更量）も同じ時系列から導出し、§13 の異常検知が参照する。

### §11.4 capacity gate

多周期収束の途上でも、クラスタ使用率が閾値を超えたら新規の変更を止める。これは rate limit の一種であり、収束の
速度そのものを容量で律速する。capacity gate の宣言形・halt セマンティクス・「是正は reconciler の外」の分担は
変更予算に属するため **§13.4 を正とする**（本節はその多周期収束における適用点であることを述べるに留める）。


# 第IV部 安全機構

level-triggered な収束は、毎周・全リソースの desired と actual を突き合わせ、差分を全部埋めにいく。理想を1行間違えれば、次の1サイクルで数百リソースが同時に「差分あり」と判定され、並列 Apply で一斉に潰す。これはバグではなく設計通りの動作であり、収束は理想が誤っていても疑わない。

> **「速く確実に収束する」能力は、そのまま「速く確実に破壊する」能力の裏返しである。**
> しかも level 型は毎周繰り返す——手で戻しても次周また壊す。底なしである。

危険は3軸から独立に漏れる:**量**（一度に多すぎる）/ **種**（破壊的が混じる）/ **速**（気づく前に完了する）。本部はこの3軸への応答を、削除の規律（§12）・変更予算（§13）・修復の統制（§14）・隔離と防御（§15）として定める。安全機構を貫く原則は一つ、**デフォルトは halt、明示的意図で進む**である。

---

## §12 削除の規律

### §12.1 削除は集合演算であり、境界の宣言を要する

削除に要るのは観測 state ではなく、**理想にある集合を網羅列挙できるか**（closed-world assumption）という境界の宣言である。

```
消すべき = 現実にある集合 − 理想にある集合
```

- Ansible が `absent` の明示を要求するのは state が無いからではなく、open-world だから（playbook は言及したものしか語らない）。
- k8s が黙って消せるのは etcd に state があるからではなく、ownerRef + label selector で「管理する集合の境界」を宣言できるから。
- したがって **closed-world は state ではなく境界の宣言で作られる**。観測 state レス（§4）と削除は両立する。

集合演算の左辺（現実にある集合）を得るには集合列挙が要る。これが `Lister`（§5）であり、prune の前提である。**Lister を持たない provider に prune は許されない**。

### §12.2 DeletionPolicy（2モード）

```go
type DeletionPolicy int
const (
    ExplicitAbsentOnly DeletionPolicy = iota // open-world:absent 明示が要る
    PruneWithinScope                          // closed-world:scope 内を網羅管理(Lister 必須)
)
```

**モード1 closed-world（prune）** — 現実の全集合を網羅 List できる種別（DNS zone 内 record / volume 群 / VM 群 / FW ルール群）。境界宣言 `managed: true` により、scope 内で理想に無いものを「消すべき」と確信できる。

```yaml
scope:
  provider: dns
  zone: internal.example.com
  managed: true     # この zone 内は網羅管理。zone 外には触れない
records:
  - { name: web, ip: 10.0.0.1 }
  - { name: db,  ip: 10.0.0.2 }
# → zone 内で上記2つ以外の record は「消すべき」と確信できる(履歴 state 不要)
```

**モード2 open-world（explicit absent）** — 網羅 List が非現実的な種別（パッケージ / ファイル / 個別設定エントリ）。個別宣言し、消すには明示 `state: absent` を要求する。この層は Ansible 方式が正しい。

削除判定は共有リソースの種別で3種に分かれる。

| リソース種別 | 削除可否の判定 | 例 |
|---|---|---|
| **離散・被参照** | 参照カウントがゼロ | image / テンプレート |
| **容量プールのメンバー** | 抜いても残余が充足 + migrate 可 | クラスタ内のホスト |
| **単純専有** | closed-world scope 内で理想に無い | テナントの VM |

VM が依存するのは個別ホストではなくクラスタ（容量プール）なので、VM が乗っているホストも十分な残余容量があれば migrate して削除できる。参照カウントではなく容量充足性で判定する。

### §12.3 pruneable（二段構え・破壊的型は既定 false）

closed-world 種別でも、破壊的な種別（volume / VM）は `managed: true` だけでは消さず、**さらに削除 opt-in** を要求する。**`pruneable: false` を破壊的型の既定**とし、毎周回るループに `DROP` 相当を自動実行させない。closed-world の網羅性は「消せる」を意味するが、「消してよい」は別の明示的意図を要する、という二段構えである。

順序付きコレクション（FW ルール・ACL・ルーティングポリシー）は位置依存の順序リストであり、unordered set-diff で扱うと順序が入れ替わって穴が開く。

- **リスト全体が原子単位**。scope = リスト、prune は置換に吸収され、**Apply = 全置換**（candidate/commit と好相性）。
- ただし plan の表示粒度はルール単位の差分に展開する（**適用粒度 ≠ 表示粒度**）。

### §12.4 List 完全性の検証（prune の前提を実行時に守る）

`unknown ≠ absent`（§4）は `Read`（個別）の三値で守られるが、prune の根拠である `List`（集合）の**サイレント欠損**には無力である。ページングの途中打ち切り・フィルタバグ・相手 DB のレプリカ遅延はいずれも「成功した List」として返り、欠けた ID が「理想に無い = 削除」に直結する（volume = データ消失 / VLAN = 通信断）。「List の網羅性が prune の安全性を担保する」以上、その網羅性を実行時に検証する。

- **（a） Provider 契約にページング完全性を明記**（§28）:List は「next token が nil で終わったこと」を確認して返す。**途中打ち切りを検知したら成功として返さず、List を失敗（error）にする**。SDK に完走検証ヘルパを入れ、ページング処理を作者に手書きさせない。
- **（b） 前回観測比の件数急減 halt**:`List` 結果の基数が前回 List 比で急減（既定 −50% 超）したら、**その scope の prune をその周は保留し Event を出す**（render 入力側の desired 急減検知・§9 と対称）。前回 List 基数は S3（インメモリ・非永続。喪失しても次周は保守側 = prune 保留に倒れるので安全）。
- **（c） List に三値を許す**:網羅を保証できない今回の List を実装が申告できる `ListStatus`（`complete` / `partial` / `unknown`）を返せるようにし、`complete` 以外の周は prune を保守側に倒す。`Read` の三値と対称。

```yaml
scope:
  provider: storage
  cluster: ceph-a
  managed: true
  list_drop_threshold: 0.10   # この scope は急減閾値を厳しめに(既定 0.50)
```

急減閾値は既定 50% だが、`scope` に `list_drop_threshold` を宣言して scope ごとに上書きできる。**流動性の高い scope（テナントの短命 VM 群）は緩め、破壊が致命的な scope（ストレージ volume・VLAN）は厳しめ**に設定する。

**planned_change（§13.3）との連動**:窓内・scope 内・種別一致の宣言済み削除/再作成（棚卸し削除・一斉再イメージ）は「急減」とみなさない。宣言済みの件数減は誤発火の原因であり、planned_change が「これは意図された減少である」を宣言することで急減 halt を抑止する（宣言外の減少は引き続き halt）。

（a）（b）（c） はいずれも v1a（prune の安全性の中核）。

### §12.5 Release の非対称（下流削除を List で確認後 + cooldown）

払い出し資源（VLAN/IP/ASN）の返却 = Release は、削除の一種でありながら誤発火が最悪級の事故（使用中の値の再払い出し衝突）を招くため、非対称原則を特に厳格に適用する。素朴な「所有者が desired から消えたら返却」は、render 入力が stale（CMDB 断・理想の読み損ね）で owner 消滅と誤認すれば衝突を起こす。

- **（a） desired からの消滅では Release を発火させない。下流リソースの削除完了を確認してから**実行する（「理想から消えた」ではなく「現実から消えた」を根拠にする）。
- **（b） 削除完了は `List`（集合演算）を根拠にする**。`Observation` の三値（§4）には absent を表す値が無く、`Read` の `unknown` と「本当に消えた」を区別できない。したがって **削除完了 = 対象 ID が scope の List 結果に現れなくなったこと**（§12.1 の集合演算）とし、§12.4 の List 完全性検証（件数急減 halt・ページング完走）をそのまま前提にする。
- **（c） 返却後に cooldown / 検疫 TTL** を置く（即時再払い出しをしない。DHCP のリース再利用猶予と同型）。
- **（d） `pruneable: false` 等で下位が残存する間は Release を保留**する。

この規律により、**Release は Lister を持つ provider の下流にのみ許可**される。List できない下流の Release は保守側（保留）に倒す。allocator 側の返却プロトコルと cooldown は §19 に接続する。

### §12.6 final_backup と削除 Workflow 推奨形

破壊的削除は取り返しがつかないため、削除の直前に退避を取る規律を推奨形として定める。

- **final_backup**:破壊的型（volume / VM）の prune 前に、直近スナップショット / エクスポートを取得してから削除する。バックアップの取得先・保持は §25 の DR 経路に従う。
- **削除 Workflow 推奨形**:大量削除・容量プールメンバー削除は、収束ループ上の自動 prune ではなく Workflow（§6）として明示的にトリガすることを推奨する。順序は「List 完全性確認 → final_backup → migrate（容量プールなら）→ 削除 → List で消滅確認 → Release（下流採番があれば）」。危険な手順を毎周回るループから隔離する原則（§6）の削除への適用である。

---

## §13 変更予算

### §13.1 rate limit（cycle 単位・破壊的加重・in-flight 込み）

同時に走る変更の量に上限を張る。閾値は**破壊的変更に重み付け**する（create 100 件は概ね安全、delete 10 件は止める）。

```yaml
safety:
  max_changes_per_cycle: { absolute: 10, percentage: 5% }
  on_exceed: halt-destructive-only   # halt-all | halt-destructive-only | warn-and-proceed
```

- `on_exceed` は scope ごとに選択する（本番 = `halt-destructive-only`、dev = `warn-and-proceed`、最重要 = `halt-all`）。
- **rate limit は in-flight（非同期 Apply・§8）を含む同時変更数で評価する**。さもないと「毎周 10 件ずつ発射し続けて in-flight が積み上がる」抜け道になる。

### §13.2 累積時間窓予算（漸進的全削除を塞ぐ）

cycle 瞬間量だけを見る rate limit は、**周期をまたいだ漸進的削除**（理想から 500 件が消えたまま、毎周 10 件ずつ 50 周で全消し）に構造的に無力である（`halt-destructive-only` は「その周の超過分を保留」であって「削除意図そのものの凍結」ではない）。これを塞ぐため、cycle 瞬間量に加え**時間窓での累積破壊量**を見る。

```yaml
safety:
  destructive_budget:
    window: 24h
    max_deletes: 50            # 24h あたりの破壊的変更の総数上限
    max_scope_fraction: 20%    # 単一 scope の実在リソースの X% を超える削除進行で escalate
  on_budget_exceed: halt-all   # 予算超過 → rendered_revision 全体を凍結し人へ
```

- **24h 等の窓での破壊的変更総数**が `max_deletes` を超えたら **`halt-all`（理想変更そのものを凍結し人へ）** へエスカレーションする。
- **単一 scope の実在リソースの `max_scope_fraction` が削除進行中**、または **`halt-destructive-only` 発動が M 周連続**でも escalate する（「じわじわ全消し」の進捗率を見る）。
- **異常検知（巨大 Diff / 削除方向 Diff 急増）の "止める" セマンティクス**:検知したら「超過分保留（漸進継続）」ではなく、**その rendered_revision 全体を凍結し、planned_change 宣言（§13.3）が対応するまで一切の破壊を保留**する（非対称原則に整合）。`Plan` が破壊性フラグを持つ（§4）ことでこれを実装できる。
- 累積予算のカウンタは **S2（進行 state）相当として永続化**する（再起動で窓がリセットされると穴が復活する）。**予算カウンタはシャード単位で持つ**——シャード = ownership = 爆発半径境界（§15）であり、全域予算は外部集約で見る（§29）。

**悪意/大量破壊への追加ゲート**:安全機構は本来「誤操作」防御であり、原因タグ A（意図・§10）は「通す」側に倒れる。しかし侵害された reconciler は Apply を直接叩き、A タグとして安全機構を素通りできる。したがって **`ownership: platform` の破壊的大量変更は、原因タグ A でも承認必須**とする（「間違いは止め意図は通す」に「ただし大量破壊は意図でも人の承認を要する」を重ねる）。

### §13.3 planned_change 宣言（専用予算レーン）

累積時間窓予算・cycle rate limit・List 急減 halt（§12.4）は「誤操作・暴発」を止めるために破壊量を絞るが、これが**大量の正当作業**（棚卸しによる 500 リソース削除・HPC 500 台の一斉再イメージ・テナント一括退去）と正面衝突する。正当な作業なのに毎周予算に阻まれて進まない、あるいは予算超過で全体が halt する。かといって予算を恒常的に緩めると暴発が止まらない。この衝突を、**intent 内の第一級宣言 `planned_change` による専用解除弁**で解く。`planned_change` はそれ自体が PR 承認を経る（= 人の意図の明示）。

```yaml
planned_change:
  scope: { site: hpc-a, cluster: gpu-pool }   # 対象 scope
  kind: reimage                                # 変更種別:delete | update | reimage | recreate 等
  max_count: 500                               # 上限件数(この宣言で許す破壊/変更の総数)
  window: "2026-08-01 00:00 .. 2026-08-01 06:00"  # 時間窓(この間だけ有効)
  approval: { pr: 1423, approvers: [alice, bob] }  # 承認情報(§14.4 の要件を満たす)
```

効果は**バイパスではなく専用予算レーン**である。

- **窓内・scope 内・種別一致の変更は、宣言上限（`max_count`）までの専用予算レーンで実行**される。これは通常の累積時間窓予算・cycle rate limit とは**別枠で計上**され、通常予算を消費しない。上限件数は宣言され、超えれば止まる。予算を「無効化」するのでなく「宣言分だけ別レーンを開ける」。
- **List 急減 halt（§12.4）の抑止**:宣言済みの削除/再作成による件数減は「急減」とみなさない。棚卸し削除が誤って急減 halt を誘発する問題がこれで解ける。
- **reimage の delete 誤計上を解消**:再イメージは内部的に delete+create に見えて破壊予算を食い潰すが、**変更種別 `kind: reimage` を宣言**すれば「これは再イメージであって純粋な削除ではない」と分類され、一斉再イメージが破壊予算に delete としてカウントされない。
- **窓を過ぎたら自動失効**し、通常予算に戻る（宣言の効力は window に閉じる。恒久緩和にならない）。

**宣言外の変更は引き続き通常予算で評価される**。`planned_change` が site-a の reimage を許可していても、同じ周期で起きた宣言外の delete（別 scope・別種別・上限超過分）は通常の rate limit / 破壊予算 / halt が効く。すなわち **「宣言中でも、宣言外の暴発は止まる」**——これが「バイパスではなく専用レーン」の意味である。`planned_change` の型と専用予算レーンの評価は v1a、承認情報の requirements 連携（§14.4）は v1.5 に属する。

### §13.4 capacity gate

rate limit の一種として、クラスタ使用率が閾値を超えたら新規の変更を止める。

```yaml
safety:
  capacity_gate:
    max_utilization: 80%
    on_exceed: halt-destructive-only
```

**是正は reconciler の外**である。reconciler の役割は「危険域に入ったら止める」までで、「ではどうするか（ホスト追加 / migrate / quota 締め）」の判断と実行は含まない。超過時は `CapacityThresholdExceeded` Event（§26）を発火し、是正は Workflow として別途明示的にトリガする（自動連鎖させない）。

| 何をするか | 誰の仕事か |
|---|---|
| 使用率の観測 | reconciler（Observe） |
| 閾値超過時に新規変更を halt | reconciler（capacity gate） |
| 原因の判別（意図的増加 vs drift） | 人（原因タグ・§10） |
| クラスタ容量追加の意思決定 | 上位プラットフォーム / 人（§7 platform 層は PR 必須） |
| VM の他クラスタへの migrate | Workflow（手動トリガ推奨・§6） |
| 新規申請の quota 制限 | 上位プラットフォーム（§7） |

**キャパシティ管理のドメイン境界——現在充足性 = 収束 / 予測 = 収束の外**。capacity gate が収束として担うのは**現在の容量充足判定**（指定クラスタの残余が閾値内か・容量プール判定の延長）だけである。**時系列外挿による予測は持たない**——将来の枯渇予測は過去の観測を保持して外挿する営みであり、毎周リアルを再観測して現在だけを見る level-triggered の構造に元々存在しない（観測 state レスと相容れない）。予測は監視系へ export し（時系列予測 → Prometheus・§3 第4原則）、収束が引き受けるのは「今、閾値内か」の二値判定に限る。

---

## §14 修復の統制

### §14.1 remediation mode（auto / alert-only）

そもそも自動で直すかを決める最も手前の手綱。粒度はリソース / scope。

- **`auto`** — 検知した drift を自動修復する。
- **`alert-only`** — 検知は続け、修復だけ止める。**爆発半径ゼロ**で、最も保守的な本番リソース向け。「常に自己修復」はオンプレでは強すぎる（緊急対応で人が手を入れた変更を潰しかねない）。

### §14.2 remediation schedule（時間帯連動の自動切替）

remediation mode を手動 freeze / unfreeze でしか切り替えられないと、保守窓文化の SLA 環境ではヒューマンエラー面が残る。**時間帯連動で mode を自動切替する `remediation.schedule`** を加える（v1.5）。

```yaml
remediation:
  default: alert-only              # 既定は最も保守的な側
  schedule:
    - window: "Sat 02:00-04:00"    # 保守窓のみ auto
      mode: auto
    - window: "*"                  # それ以外は alert-only(検知は続け、修復だけ止める)
      mode: alert-only
  tz: Asia/Tokyo
```

**保守窓のみ auto、他は alert-only** を宣言的に表現する。窓の外で検知した drift は次の窓まで alert-only として持ち越され、level-triggered なので窓が開いた周に自然に修復される。freeze（§14.3）は「今すぐ全体を alert-only に倒すマクロ」、schedule は「時間帯で mode を張る宣言」であり、両者は重ねられる（freeze が最優先）。

### §14.3 優先規則（most-restrictive-wins）と freeze

複数の mode 決定源（schedule / ownership 既定 / scope 明示 / freeze）が同一リソースに対して異なる mode を指すとき、**より制限的な mode が常に勝つ**（`alert-only` > `auto`）。優先順位でなく「最も制限的」で解決するのは、安全側への非対称原則（§3）を mode 解決にも一貫適用するためである——どこか1つでも「止めろ」と言えば止まる。

- schedule が保守窓で `auto` を張っていても、`ownership: platform`（§7）や freeze や scope 明示 `alert-only` のいずれかが効いていれば、そのリソースは `alert-only` に落ちる。**auto は「全ての決定源が auto を許した」ときだけ成立**する。
- これにより「窓を開けたつもりが、実は freeze 中/platform だった」という取り違えが安全側に倒れる（窓を開けたのに動かない、はあっても、止めたつもりが動く、は起きない）。

**global freeze**:グローバルな変更凍結は、全体を一時的に `alert-only` に倒すマクロ（rate limit / gate 機構の一種）である。freeze 中も **drift 検知は続け、修復だけ止める**。

### §14.4 承認ゲート（N-of-M・申請者≠承認者）

段階適用における承認ゲートは、既存 Workflow（wave + invariant + Event/sink + フェーズ永続化）の組み合わせで表現でき、新プリミティブは不要。ゲート型そのものは §6 の Workflow を参照する。

**承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API `POST /workflows/{id}/gates/{gate}/approve`（却下 = halt）→ 次ステージ。タイムアウト（例 24h）で halt。承認 API 自体は認可を持たない（誰が叩けるかは上位 IdP）が、承認された事実は監査（S4・§26）に記録する。

規制環境（金融）が要求する 4-eyes（申請者≠承認者・複数承認）は、**ゲート定義への `requirements` 追加**と**承認 API の拡張のみ**で実装する（新プリミティブ不要）。

```yaml
gate:
  manual_approval:
    requirements:
      quorum: { n: 2, of: 3 }        # N-of-M:3人の承認者候補のうち2人の承認で通過
      roles: [platform-admin]        # 役割制約:この役割を持つ者のみ承認可(役割判定は上位 IdP)
      separation: applicant_ne_approver  # 申請者≠承認者を強制(4-eyes の中核)
```

- **API 拡張**:`approve` のボディに承認者 ID + 署名を載せる。ゲートは requirements を満たす承認が揃うまで「待ち」に留まり（S2）、揃った瞬間に次ステージへ進む。
- **役割制約**:承認者の役割判定は上位 IdP に委ねる（コアは認可を持たない・§7）。ゲートは「役割 X の署名が N 個必要」を requirements として保持するだけ。
- **申請者≠承認者（separation）**:`planned_change`（§13.3）や PR の申請者 ID と承認者 ID の不一致を強制でき、自己承認を型で塞ぐ。
- **全承認者を S4 監査に記録**:誰が・いつ・どの署名で承認したかを全員分 append する。承認は「事実」として恒久記録され、「どの承認ゲートを誰が通したか」が追える（§26）。
- requirements を宣言しないゲートは単一承認（team プロファイル既定・§14.5）。

### §14.5 運用プロファイル（solo / team / enterprise）

platform 破壊的変更に PR 承認必須という既定は、チーム運用を前提にしている。だが 1名運用（ホームラボ・スモールオフィス・段0-minimal・§23）では「自分で書いて自分でレビューする」承認儀式が空回りし、かえって運用を阻害する。逆に規制環境はより厳しい既定（N-of-M）を最初から要る。**規模別の既定値セット = 運用プロファイル**を定義し、`ownership` の書込ポリシー・承認ゲート・remediation の既定を規模で切り替える（個別上書きは常に可能）。

| プロファイル | platform 破壊的大量変更 | PR | 承認ゲート既定（§14.4） | 想定規模 |
|---|---|---|---|---|
| **solo** | cooling-off（遅延実行）で代替可 | 自己マージ可 | 承認者要件なし（単一 or cooling-off） | 1名運用・ホームラボ・段0-minimal |
| **team**（既定） | 単一承認必須 | PR 必須（他者承認） | 単一承認 | 少人数チーム |
| **enterprise** | N-of-M + 役割制約を既定化 | PR 必須 | N-of-M + separation | 規制・大規模 |

- **solo:cooling-off（遅延実行）による承認代替**。platform の破壊的大量変更の事前承認を、cooling-off = 遅延実行で代替できる。**`planned_change`（§13.3）の宣言から N 時間後に自動実行**され、**その間いつでも halt 可能**である。「他者のレビュー」を「時間差 + 自己による中断機会」で置き換える——1名運用では後者の方が実効的な安全になる。監査記録は team と同等（誰が・いつ宣言し・いつ実行されたかを S4 に残す）。PR は自己マージ可。

```yaml
profile: solo
solo:
  cooling_off: 2h        # planned_change 宣言から 2h 後に自動実行(その間 halt 可能)
```

- **team（既定）**:プロファイルを指定しなければ team。platform は PR 必須・単一承認・破壊的大量変更は A タグでも承認必須（§13.2）。
- **enterprise**:N-of-M + 役割制約（§14.4 の `requirements`）を既定化する。個別ゲートに書かなくても、platform の破壊的変更は自動的に 4-eyes（separation）を要求する。
- **段0-minimal（§23）と solo の対応**:段0-minimal（単一ノード・Vault/broker なし）は 1名運用と物理的に一致するため、段0-minimal では solo プロファイルを既定とする。規模が育って段0（3ノード）+ チーム運用へ移ったら team/enterprise へ切り替える。
- **プロファイルは既定値セットであって上書きを禁じない**:solo でも特定 scope に N-of-M を明示でき、enterprise でも dev scope を緩められる。プロファイルは「よく使う既定の束」を1語で選ぶ糖衣であり、個別の `ownership` / ゲート / remediation 宣言が常に優先する。

### §14.6 ブラウンフィールドの alert-only 常用

ブラウンフィールド導入では、人の変更を早々に自動で巻き戻さないため、read-only / alert-only から始めて信頼を得る。import → alert-only → scope ごとに auto へ昇格、という順序を採る。詳細は §24 に定める。

---

## §15 隔離と防御

### §15.1 blast radius（爆発半径）3軸

安全機構は「速く収束する力は速く壊す力」への応答であり、危険は独立した3軸から漏れる。本部の各節はこの3軸への応答である。

| 軸 | 危険 | 応答 | 節 |
|---|---|---|---|
| **量** | 一度に多すぎる | 変更予算（rate limit / 累積時間窓予算） | §13 |
| **種** | 破壊的が混じる | 異常検知（巨大 Diff / 削除方向急増の凍結） | §13.2 |
| **速** | 気づく前に完了する | 段階ロールアウト（canary → broad） | v2（§14.4 のゲートで骨格） |

段階ロールアウト本体（自動 canary / 自動段階昇格）は v2 に属し、v1.5 の最小 Workflow（直列 + 不変条件 + 手動承認ゲート）がその骨格を先に持つ。

### §15.2 fencing token（単一 writer の保証）

shared-nothing で「分散合意を中枢に戻さない」という方針は平常時は綺麗だが、**分断時に単一性を保証する機構**を別に要する。platform シャードの active/standby フェイルオーバーで旧 active が生きていると（GC pause / 片側分断）、2つの writer が共有基盤（NW コア・共有ストレージ）へ書き、順序付きコレクション全置換（§12.3）と組み合わさって FW ルール/ルーティングが発振し全機器全盲を誘発しうる。

- **platform シャード（単一 writer）に fencing token を必須化**する。共有基盤 provider の Apply は token 検証を通し、**古い世代の writer の書き込みを拒否**する（storage/lease fencing）。「合意は持たないが fencing は持つ」は両立する（fencing は合意より軽い）。
- **割当表（allocator 世代番号）にも fencing token を持たせ**、復元・再割当時に古い世代を fence する。static シャーディング（§17）では割当表そのものを持たず、**二重 claim（2 デーモンが同一 scope を掴む）の検知に fencing 世代番号だけを S2 として使う**（新しい可用性部品を増やさない）。
- **fencing フィールドは型定義時から最初から持たせる**（後付けは危険）。

fencing token / 世代番号は、単一 writer 前提の3経路——platform 共有基盤の書き込み・claim シャーディングの二重検知・allocator の払い出し権威（§19）——を同型の機構で守る。

### §15.3 oscillation の自動隔離

「収束器の上の収束器」（SDN / アレイ / k8s は内部に収束ループを持つ）と綱引きになると、M 回再 Apply しても Diff が縮まない。これを `oscillating` と判定し、**そのリソースを自動で alert-only に隔離**する（綱引きの自動検疫）。振動そのものの検知と多周期収束の扱いは §11 に接続する。

### §15.4 シャード = 爆発半径境界

シャードは shared-nothing の独立収束器であり、その論理境界（ownership）がそのまま物理的な爆発半径境界になる。

- **予算カウンタはシャード単位**（= ownership = 爆発半径境界・§13.2）。1シャードの予算超過は他シャードに波及しない。
- **全域不変条件（総容量・グローバル quota）はシャード内 gate + 外部集約でしか扱えない**。1シャードの invariant は自シャード内でしか強制できず、全シャード合算の制約は上位プラットフォーム（§7）や外部集約に委ねる。rate limit・累積破壊予算も同様にシャード単位で閉じる。
- **シャード間協調は現実を介して行う**——シャード同士は互いに問い合わせず、相手の作った現実を Observe して gate する（観測 state レス思想の延長）。詳細は §17 に定める。

### §15.5 デフォルト halt、明示的意図で進む

本部を貫く一つの原則を再確認する。**確信が持てないとき、加算はしても減算はしない**（非対称原則・§3）——unknown を absent と断じない、List が欠けたら prune を保留する、予算を超えたら halt する、mode 解決は most-restrictive-wins に倒す、Release は現実の消滅を待つ。

危険域に入ったときの既定は常に halt であり、そこから先へ進むには**明示的な意図**が要る。その意図の表明経路は一本化されている——コミット注記のような場当たり的タグではなく、**intent 内の第一級宣言 `planned_change`（§13.3）** と **承認ゲート（§14.4）** である。「間違いは止め、意図は通す」——ただし大量破壊は意図（タグ A）でも人の承認を要する（§13.2）。速く収束する力を持つ以上、止まる力を既定に据えることが、level-triggered な収束を運用に耐えさせる要である。


# 第V部 スケールと分散

本部はスケールと分散を扱う。単一デーモンの天井がどこにあり（§16）、それを超える最初の分割手段（§17）、
分割が守るべき原則と将来の姿（§18）を定める。

貫く姿勢は二つ。第一に、**分散の前に単一を延ばす**——単一デーモンの限界を実機レイテンシで正確に見積もり、
BulkObserve と間引きで数万まで届かせてから、はじめて分割する。第二に、**分割は共有 state を増やさない**——
シャードは共有 state を持たない独立収束器であり、共有が要るものはすべて衛星（採番=§19 allocator・秘密=§20・
intent=§21）へ切り出し済みである。この分解がそのままスケールアウトの分解になる。

---

## §16 単一デーモンの天井

### §16.1 天井は Observe 時間で決まる

**単一デーモンの天井 ≒ Observe 時間**である。収束ループ（§8）は毎周・全リソースを再観測するため
（観測 state レス・§4/§8）、1周に要する時間はリソース数と1回の Observe レイテンシの積で決まる。Observe は
並列化できる（§8）ので、コストは逐次和ではなく最遅 Observe に漸近するが、並列度には相手 API への礼儀という
上限（同時 Observe 上限 16）がある。

天井は単一の数字では表せない。**1回の Observe レイテンシは provider の階級で1桁以上違う**からである。
本設計書が主対象と宣言する泥臭い層（生 NW 機器・CLI・Ansible）ではレイテンシが 1.5〜6s に達し、実用上限は
数百台に落ちる。高速 REST 前提の「数千〜1万」はクラウド的な速い provider に限られた値であり、単一の天井として
掲げてはならない。

### §16.2 実機レイテンシ階級表と上限台数の算術

上限台数の算術は次の早期警戒線（§8）に基づく:reconcile 間隔 30s に対し Observe 予算 10s、並列度 16。
すると **30s 間隔に収まる上限台数 ≈ 10s × 16 / 1回のレイテンシ** となる。10,000 リソースを1周する時間も併記する
（1回のレイテンシ × 10,000 / 16）。

| Observe 手段 | 1回のレイテンシ | 10,000 台の1周 | 30s 間隔に収まる上限台数 | **BulkObserve あり（§5）** |
|---|---|---|---|---|
| 高速 REST（クラウド的） | 0.15s | 1.6 分 | ~1,067 | **Describe* 系で1万規模へ**（1コール/scope） |
| vCenter / govmomi（重い SOAP） | 0.8s | 8.3 分 | ~200 | **PropertyCollector で数千〜1万へ**（1コールでクラスタ全 VM） |
| Proxmox（REST） | 0.3s | 3.1 分 | ~533 | **`/cluster/resources` で1万規模へ**（1コールでクラスタ全量） |
| NETCONF / gNMI（NW 機器） | 1.5s | 15.6 分 | ~107 | バルク無し。ただし**台数が物理的に有界**（§16.4） |
| CLI scrape（旧機種） | 4.0s | 41.7 分 | ~40 | バルク無し。同上 |
| Ansible module（fork+SSH） | 6.0s | 62.5 分 | ~27 | バルク無し。同上 |

- **天井は provider 階級別に読む**:完全/標準階級（REST・自前）= 数千〜1万、**Ansible/CLI ラップ = 数百〜千**。
  単一の「数千〜1万」という表現は採らない。
- NW 機器は同時セッション数が 2〜8 に制限される機種が多く、機器単位で並列度を上げられない。Ansible は fork
  コストがあり goroutine 並列の恩恵が I/O 待ちほど効かない（CPU/メモリが先に律速しうる）。
- **`ready_condition` の複数 Cycle 依存（§11）は、1 Cycle が数分〜数十分になる規模では依存解放に数時間かかる**。
  遅いこと自体は設計上正しいが、運用ドキュメントで期待値を明示する。

### §16.3 BulkObserve による書き換え（スケールアウト段1）

現実の多くの権威は **scope 内を1コールでまとめて返すバルク API** を持つ（vCenter PropertyCollector /
Proxmox `/cluster/resources` / AWS Describe* / NetBox の一覧 API 等）。これを `BulkObserve`（§5）として第一級化
すると、「1 リソース1コール」前提の算術が書き換わる。

- **API 系は数コールに畳める**:バルク API を持つ層（REST/クラウド/CMDB 系）は「1 リソース1コール」前提の上限を
  突き抜け、**API 系で単一デーモン1万規模**へ届く。表の BulkObserve 列がこの書き換え後の姿である。
- **毎周・全量再観測の原則は不変**（§8）:取り方が「N コール → 1コール」に変わるだけであり、観測 state レスには
  一切影響しない。バルク結果は当該周期でのみ使用し、周期をまたいだ再利用は禁止する（§5）。

### §16.4 バルク非対応層の非対称（台数が自然に有界）

バルクを持たない生 NW 機器（NETCONF/CLI）・BMC は、表の上では上限が数十〜百台に見える。しかしこれらは
**台数が物理的に有界**である——1 デーモンが受け持つラック/サイトのポート数（1ラック 48 ポート等）は自然に
上限され、REST 系のように「テナントが無限に増やせる」スケール圧を受けにくい。

すなわち非対称が天井問題を実際上緩めている:

- **バルクを持つ層は数が伸びるが、1コールで畳める。**
- **バルクを持たない層は、そもそも数が伸びない。**

天井が破綻するのは「バルクも無く、かつ台数が無限に伸びる」象限だけだが、その象限は現実にはほぼ存在しない。

### §16.5 tier 別 Observe 頻度（間引き・v1b）

泥臭い層を主対象と宣言する以上、間引きを「破綻してから」入れるのでは遅い。**tier 別の Observe 頻度
（安定リソースは N 周に1回）の最小形を v1b の Observe スケジューラ seam として入れる**（型に頻度の余地を残す）。

- **既定は「毎周全部」を維持し（§8）、頻度は tier で上書き可能にする**。安定リソースの間引きは drift 検知が数周
  遅れるが許容する。
- 完全な間引き最適化と watch 併用は v2 のまま（§16.6）。v1b では seam（スケジューラの型）を切るところまでを行う。

破綻したときの延命の入口が、§8 の早期警戒線（間隔 30s に対し Observe 10s 超・API 課金/rate limit が頻度で効く・
リソース数が桁で増えた）である。

### §16.6 watch 併用ハイブリッド（v2）

provider が watch を持つなら、**drift 即知 + 保険の低頻度ポーリング**のハイブリッドが組める（v2）。
watch が流すのはヒント（触られた scope）だけであり、真実は運ばない——ヒントを受けてその scope を新鮮に
render → Observe → Diff → Apply する（§8/§21）。ヒントを取りこぼしても定期周期（低頻度ポーリング）が正しさを
保証するため、watch は「いつ収束するかの最適化」であって「何が真実か」には関与しない。

### §16.7 スケール段階表（規模 → 構成）

分散の前に単一を延ばし、数万まで届かせてから分割する。

| 規模 | 構成 |
|---|---|
| 〜数千 | 単一デーモン・毎周全 Observe |
| 数千〜数万 | 単一 + Observe 間引き / watch 併用 / **BulkObserve（§5）** |
| 数万〜 | **静的シャーディング（§17・`--claim`）**（tenant / site / cluster） |
| 地理分散 | サイトごとシャード + **claim の git 配布 / S2-free シャード（§17）** + 動的ルーター（§18・将来） |

> **注（衛星は Observe 天井の算術に乗らない）**:上表の天井は **Observe 時間**（リアル再観測）で規定される。
> 共有衛星（§19 allocator / §20 Vault）への QPS は**このスケールとは別軸**であり、**フリート規模ではなく変更
> レートに比例する**（§22）——定常収束は割当 Lookup キャッシュ（S3）と secret TTL キャッシュで**衛星コール
> ゼロ**で回る。したがって「シャード数を増やすと衛星が詰まる」は churn とポータル申請ピークで sizing すれば
> 回避でき、衛星は Observe 天井の算術に直接は乗らない（§22）。

---

## §17 claim シャーディング

### §17.1 claim-as-intent（割当を intent として宣言する）

単一デーモンの天井（§16）を超える最初の分割手段を、**新しい可用性部品（ルーター）も新しい S2 も増やさない**
静的シャーディングとして定義する。方式は **シャード割当そのものを git の intent として宣言する**ことである。

- 各デーモンは **`sakutei --claim <scope-selector>`** で起動し、**全 intent を pull した上で、自分の claim に
  マッチする scope 分だけを収束**する（claim に含まれない scope は Observe すらしない）。
- **どのデーモンがどの scope を担当するか = git 上の宣言的 intent** とする（例:`shards/tokyo.yaml` に
  `claim: {site: tokyo}`）。これにより **シャード割当が PR レビュー・履歴・監査の対象**になる——「割当は運用の
  隠れた設定」ではなく「収束すべき第一級の intent」になる。
- **二重 claim（2 デーモンが同一 scope を掴む）の検知は既存の fencing token（§15）**で行う。分断や設定ミスで
  割当が重なっても、共有基盤 provider の Apply が古い世代の writer を fence する。
- **ルーターという新しい可用性部品を増やさない**:動的ルーター（§18）は割当表（S2）とその可用性が新たな中心
  依存になるが、claim-as-intent は割当を git（既存の intent 経路）に載せるため、**新 S2 は fencing 世代番号のみ**。
  git が落ちてもローカルミラー（§23）で claim は読める。ルーターも新しい S2 も要らない。

### §17.2 shared-nothing（分散合意を中枢に戻さない）

**各シャードは shared-nothing で独立収束**する（デーモン間協調不要 = 分散合意を中枢に戻さない）。
分割キーは既存の `scope` / `tenant` / `ownership`（§7）がそのまま使える。スケールアウトの正しい軸はシャー
ディングであり、リーダー選出 active/standby は可用性用（active は1台）、機能分割（Observe 係/Apply 係）は
過剰であって、いずれもスケールの軸ではない。

| 軸 | スケールするか |
|---|---|
| **シャーディング**（tenant / site / cluster で水平分割） | **する（推奨）** |
| リーダー選出 active/standby | **しない**（可用性用。active は1台） |
| 機能分割（Observe 係 / Apply 係） | しない（過剰） |

### §17.3 ownership 境界 = シャード物理境界

所有と境界（§7）がそのままシャードの物理境界になる。

- **共有基盤は専用の platform シャードが1つ所有・変更**し、テナントシャードは **read-only で参照**する
  （書き手が1つなら綺麗）。**所有権の論理境界がそのままシャードの物理境界**になる。
- **platform シャード（単一 writer）には fencing token を必須化**する（§15）。共有基盤 provider の Apply は
  token 検証を通し、古い世代の writer の書き込みを拒否する。分断時（GC pause / 片側分断）に旧 active が生きて
  いても、2つの writer が共有基盤へ同時に書いて順序付きコレクション全置換と組み合わさり発振する事故を止める。
  **「合意は持たないが fencing は持つ」は両立する**（fencing は合意より軽い）。
- **fencing フィールドは型定義時から最初から持たせる**（後付けは危険）。

### §17.4 git 配布規約（パス規約・sparse checkout・ミラー中継）

300 拠点規模では、（1） 全 intent の pull トラフィックと （2） 店舗ローカル S2 の耐久性（段0-minimal は quorum
なし・§23）が詰め所になる。前者を配布規約で塞ぐ。

**リポジトリレイアウト規約 + sparse checkout**:claim でシャーディングする場合、intent リポジトリは
**`intent/<scope>/...` のパス接頭辞規約**に従い、**claim ↔ パスの対応を機械可能**にする。

```
intent/
  platform/…              # 全シャードが read-only 参照する共有 platform(§7)
  site-a/…                # claim: {site: site-a} のシャードが所有
  site-b/…
  …
```

- 各シャードは全 intent を pull しない。**sparse checkout / partial clone** で「**自 claim 分 + 共有 platform
  参照分**」のみを取得する。claim ↔ パスが機械対応するため、`--claim {site: site-a}` から fetch すべきパス
  （`intent/site-a/` + `intent/platform/`）が直接引ける。
- これは §17.1 の「全 intent を pull」を**否定せず精緻化する**——正しさは「自 claim にマッチする scope だけを
  収束する」ことで担保され、pull 範囲を絞るのはトラフィックの最適化である（sparse で取り逃した他 scope は
  そもそも自分の収束対象ではない）。

**ミラー中継層**:多拠点は中央 git を直接叩かず、**読み取り専用 git ミラー**（状態を持たない中継・中央 2 拠点
程度）から **jitter 付きポーリング**で pull する。

- **webhook（早期収束ヒント・§8/§21）は NAT 越え不可の拠点ではポーリングが代替**する。level ループが正しさを
  保証するため（§8）、ヒント欠落は遅延のみで正しさに影響しない——WAN 断・NAT 環境の拠点でも定期ポーリングで
  収束は回り続ける。
- **算術**:300 拠点 × sparse fetch/60s = 5 fetch/s（自 claim 分のみの小さな fetch）。**ミラー 1 対で余裕**で
  捌ける（共有 platform 分はキャッシュヒットが効く）。中央 git 本体には拠点が直接触れないため、中央の負荷は
  ミラー数（2）に固定される。

### §17.5 S2-free シャード

エッジ / 店舗の claim は **Resource 収束のみ**を担い、**ローカル S2 を持たない**階級を第一級で定義する。

- **デーモンホストが全損しても失うものがゼロ**——S2（進行 state・§4）を持たないので、**再イメージ → pull →
  収束で完全復旧**する。店舗ローカルディスクの耐久性（段0-minimal は quorum なし）に賭けなくてよい。
- **Task/Workflow が必要な操作**（S2 を要する手続き・§6）は、S2-free シャードでは次の 2 パターンで表現する:
  - **（a） 中央 claim 側で実行する**:手続きを中央のプロビジョニング claim（§17.7）側に置き、店舗シャードは
    収束済みの結果を受け取るだけにする。
  - **（b） 完了マーカーを現実側に置く**:一回性の完了印を**現実側**に置き Observe で問う（`schema_migrations` と
    同型・§6 の単純 Task）。進行 state を外部永続化せず、現実が state を持つ。
- S2-free シャードは **Resource（宣言的・収束）しか持たない**という制約と引き換えに、**耐久性要件をゼロにする**
  ——「何も覚えないから、何も失わない」。段0-minimal（単一ノード・quorum なし・§23）と最も相性がよい階級である。

### §17.6 再シャード（scope の移動）は Workflow

あるデーモンから別デーモンへ scope を移すのは、**drain → claim 変更 PR → fencing 世代更新** の順序付き手続きで
あり、最小 Workflow（§6）で表現できる。

```
1. 旧シャードで対象 scope を drain(新規 Apply を止め、in-flight を完了させる)
2. claim 変更 PR をマージ(旧の claim から外し、新の claim に足す)= 割当 intent の更新
3. fencing 世代を更新(旧シャードの当該 scope への Apply を以後 fence)
4. 新シャードが次周(または §8 の Watch)で当該 scope を収束開始
```

**この方式が観測 state レスと整合する理由**:シャードは shared-nothing の独立収束器であり、割当という「どの
現実を誰が見るか」の情報を **git（intent）に外出し**した。§17.2 の「共有物は衛星へ外出し済み」の一貫適用で
あり、シャード間で状態を交換しない（§18 の格上げ原則）。

### §17.7 所有権ハンドオフ（開店/閉店）

再シャード Workflow（§17.6）の具体適用として、拠点のライフサイクルを claim 変更で表現する。新規拠点は中央の
プロビジョニング claim で構築し、安定後に claim 変更 PR で現地デーモンへ引き渡す。撤収は逆順である。

```
開店:
  1. 新店舗の scope を中央の「プロビジョニング claim」下で構築(中央が S2 付きで Task/Workflow を実行)
  2. 安定後、claim 変更 PR で当該 scope を店舗デーモン(S2-free)へ引き渡す
     = 中央 claim から外し、店舗 claim に足す(§17.6 の drain→claim 変更 PR→fencing 世代更新)
閉店:
  上記を逆順(店舗 claim から外し中央へ戻す → デコミッション)
```

- **プロビジョニング（手続き的・S2 が要る）と定常収束（Resource のみ・S2-free）を claim 境界で分ける**のが要点。
  開店直後の重い立ち上げ手続きは中央が担い、安定後の維持だけを店舗が S2-free で担う。
- 所有権ハンドオフが **PR + fencing 世代更新**（§17.6）で表現されるため、開店/閉店が**監査・レビューの対象**に
  なり、二重所有（旧所有者が生きている）は fencing で検知される（§15）。

### §17.8 スライス

`--claim` の**型は v1a で切る**（起動フラグと scope-selector のパース。実装は必要時）。静的シャーディングの
**実動作（claim フィルタ + fencing + 再シャード Workflow）は v1b**。リポジトリレイアウト規約・sparse checkout・
ミラー中継・S2-free シャード階級・claim ハンドオフはすべて v1b（§27）。動的ルーター・自動リバランス・
`RemoteExecutor`・lease 縮退は v2（§18・§27）。

---

## §18 分散の原則と将来

### §18.1 原則:シャード間協調は現実を介して行う

**シャード間協調は現実を介して行う**。シャード同士は**互いに問い合わせない**——相手の作った**現実を Observe
して gate する**（観測 ready 待ち）。platform リソースの readiness をテナントシャードが Observe で gate し、
シャードをまたぐ依存は結果整合として扱う。

これは §16/§17 の妥協ではなく、**観測 state レス思想の延長**として明文化する。**シャード間 RPC / 状態交換は
正しさに一切関与しない**——交換した状態を信じることは、排除したはずの観測 state を持つのと同じ罠だからである。
シャード間で運んでよいのはヒント（いつ見直すか）だけであり、真実（何がどうなっているか）は必ず現実を Observe
して得る。

### §18.2 放棄するもの（結果整合と全域不変条件）

スケールアウトは次を正直に放棄する。

- **シャード間の強い順序保証は放棄する**（結果整合）。シャードをまたぐ依存は「観測 ready 待ち」（§18.1）で
  あり、シャード内のような plan 順序 gating は効かない。強い順序保証はシャード内に閉じたときのみ成立する。
- **全域不変条件（総容量・グローバル quota 等）はシャード内 gate + 外部集約でしか扱えない**。1シャードの
  invariant（§6）は自シャード内でしか強制できず、全シャード合算の制約は Sakutei 単体では強制できない——上位
  プラットフォーム（§7）や外部集約に委ねる。rate limit・累積破壊予算（§13）も同様にシャード単位で閉じ、全域
  予算は外部集約で見る（§29）。

### §18.3 共有物は衛星に外出し済み（分解の再確認）

**共有物はすべて衛星に外出し済み**である——採番 = §19 allocator、秘密 = §20 Vault、intent = §21 git。
**この分解がそのままスケールアウトの分解になる**:シャードは「共有 state を持たない独立収束器」であり、共有が
要るものは既に衛星へ切り出されている。

したがって次の天井はシャード自身ではなく**共有衛星の QPS**である。シャードは shared-nothing だが、採番
（allocator）と秘密（Vault）は共有衛星であり、シャード数を増やすと衛星への QPS が次の律速になる。これは
早期収束パス（§8）の「allocator の可用性が UX に直結する」論点と合流する共通の壁だが、**§22（衛星の停止は
変化を止めるが維持を止めない）で原則として塞ぐ**——定常収束は衛星コールゼロで回り、QPS は変更レートに比例
する（フリート規模ではない）。

### §18.4 v2 に残るもの（動的分散）

段2（静的シャーディング・§17）を第一級にした上で、次を v2 に残す。

| 機構 | 概要 |
|---|---|
| **動的ルーター** | 「どのリソースがどのシャードか」を動的に引く薄い層（状態 = 割当表 = S2）。自動リバランスを伴う。 |
| **自動リバランス** | シャード間の負荷を自動で再配分する。 |
| **`RemoteExecutor`（gRPC + mTLS）** | 実行体を別ホストへ出す。ネットワーク分断が常態化する。 |
| **lease 縮退** | 分断・lease 失効時の段階縮退。 |

**分散 Executor 時の段階縮退（将来設計）**:実行体を別ホストへ出すと分断が常態化し、キャッシュした理想は
分断中に陳腐化しうる。**非対称原則（§3）の分散版**として段階縮退を定める——通常は フル収束、制御面と分断なら
**加算のみ**（作る・直す・起動する。破壊的操作=削除・停止は保留）、lease 失効なら **Observe のみに縮退**
（自律収束を停止）。キャッシュに要るのは 2 フィールドだけ:`revision`（制御面が単調増加させる版。復帰時に
「古い理想で動いていたか」を検出）と `leaseUntil`（この時刻まで自律動作を許可。無期限の暴走と、意図的に切り
離した agent のゾンビ収束を防ぐ）。DB は不要。規律は「確信が持てないときは、加算はしても減算はしない」——
**破壊的操作だけが制御面への到達を要求する**（§3）。

動的ルーター（v2）の割当表にも **fencing token / 世代番号**を持たせ、復元・再割当時に古い世代を fence する
（§15 の `revision`/`leaseUntil` と同型）。静的シャーディング（§17）では割当表そのものを持たず、二重 claim
検知に fencing 世代番号だけを S2 として使う。動的ルーターを採る場合の割当表 fencing はこれと同型である。

### §18.5 メタ収束器を作らない

**全体を統べるメタ収束器は作らない**（k8s federation の轍を踏まない）。上位は司令塔ではなくルーター（§18.4）で
あり、状態は割当表（S2）に閉じる。分散の正しさは各シャードの独立収束と、シャード間を現実で結ぶ gate（§18.1）が
担い、その上に「収束機構を収束する収束機構」を積まない。共有が要るものは衛星に外出し（§18.3）、協調は現実を
介す（§18.1）——この 2 点で、中枢に分散合意を戻さずにスケールアウトを閉じる。


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


# 第VII部 運用ライフサイクル

本部は Sakutei を「現実の基盤に載せ、事故から復旧し、動作を検証・監査する」までの運用ライフサイクルを定める。第II部〜第VI部が機構そのものを定めるのに対し、本部はその機構を**時間軸に沿ってどう立ち上げ・移行し・守るか**を定める。貫く姿勢は「制御面は落ちてもよい・データ面は動き続ける」「壊れる前に止まる」「事前チェック最小・事後監査最大」の三つである。

---

## §23 ブートストラップ

### §23.1 自己を立ち上げる何かは自分の外側から来る

Sakutei はインフラを作るが、**自分自身（動作ホスト・自分の設定・資格情報）は自分では収束できない**。これは一般解の存在しない構造であり、自己を立ち上げる何かは自分の外側から来るしかない（k8s の kubeadm、Cluster API の management cluster、Terraform の state バックエンドと同型の構造）。妥協ではなく定石である。

したがってブートストラップは**2段構造**とする。

- **段0** — 人が Ansible と金庫で作る、**不変の最小の土台**。Proxmox 3ノード + 統合 Ceph + VM-HA の上に Vault / credential-broker / reconciler を載せる。
- **段1以降** — 段0 の上で reconciler が自律収束する、本番インフラ全体。

**設計目標は段0の最小化**である。段0 は「作って忘れる」不変の最小セットに保ち、変化するものはすべて段1以降へ押し出す。

### §23.2 段0の構成

```
人間（パソコン + Ansible + 金庫）
  ① 管理NW到達性の最小核  ② Proxmox 3ノード構築  ③ 統合 Ceph
  ④ VM 定義（Vault / [credential-broker=v2] / reconciler）
  ⑤ Vault unseal（★コールドスタートは Vault 先行・§23.5）
  ▼
段0（Proxmox 3ノード + 統合 Ceph + VM-HA）
  ├─ 管理NW到達性の最小核        … 段0 収容ポート + 最小 L2/L3（§23.4）。Sakutei は触らない
  ├─ Vault（VM-HA 保護）         … シークレットの根
  ├─ [credential-broker（VM-HA 保護）]  … 機器認証。reconciler の依存先。v2 で有効化
  └─ reconciler（VM-HA 保護）      … git から pull、ローカルミラーで全断耐性
  OOB：コンソールサーバ / OOB専用NW … 管理NW 全断時の唯一の経路（Break Glass）
  外部依存：GitHub/GitLab（理想状態リポジトリ）= 更新取得元。稼働必須依存にしない
  ▼ 自律収束
段1以降：本番インフラ全体
```

**credential-broker は v2 で有効化される衛星である**（§20）。v1（v1a/v1b）の段0 に broker は建てない——v1 は `reconciler → SecretRef（Vault）→ 機器` で到達し、broker への実行時依存を持たない。上図の broker 行と ④ の broker 部分は v2 で有効化される（v1 では角括弧の要素を除いて読む）。段0-minimal（§23.6）では Vault も broker も無い。

### §23.3 段0の設計判断（open-world・pull・occasional）

段0 は本システムではなく **Ansible** で構築・維持する。この役割分担は2ツールの性質差と一致する。

- **段0 = Ansible**（open-world・pull 型・稀・意図的）。段0 は不変基盤であり、その上位に「直す上位」がいない最上位ゆえ、**drift の自動修復はむしろ危険**である。人が意図したときだけ動く Ansible が正しい。
- **段1以降 = 本システム**（自律収束・常時ループ）。

段0 に固有の判断を確定する。

- **git は外部**（GitHub/GitLab）に置き、段0 内から git 本体を消す。各ノードにローカルミラーを持ち、外部 git 不達時は最後に pull した状態で収束を継続する（**稼働必須依存にしない**・§25.2）。
- **Vault と broker は段0 内**に置かざるを得ない。有状態サービスが複数になるため段0 が小プラットフォーム化し、**Ceph + VM-HA が正当化される**（Proxmox の統合 Ceph により運用重量が下がる）。収束デーモンは観測 state レス + level-triggered であり **VM 再起動の空白を許容**するため、アプリレベル HA は任意の上積みでよい。
- **unseal は Ansible（オペレータ起動）**で行う。unseal key は playbook の外に置く（Ansible Vault で暗号化 + 人の金庫）。「シークレットの根を守るものの根」は人間の金庫に着地する——これがブートストラップの無限後退の正しい終わり方である。
- **可用性の正直な但し書き**：VM-HA でも Vault は再起動で sealed に戻り、unseal はオペレータ起動である。Vault 載せノード障害の MTTR は**人間の unseal 時間を含む**（Raft HA でも sealed-on-restart は解消しない）。緩和は §20 の資格キャッシュと alert-only 縮退による。

**爆発半径は自己運用の最重要項目である。** 段0 では「1コミットで全 VM 削除」が原理的に可能なため、第IV部の安全機構は段0 の生命線となる。特に **変更予算による halt（§13）** と **remediation mode（§14）** は v1 に必須である（誤記1つで本番を壊さない装置は、事故る前に付ける）。

### §23.4 管理ネットワークの要件（段0 / 段1 の境界）

BMC・スイッチ管理ポートへ到達するための**管理NW**は段0 の定義に直結する。判定基準は段0 の一般則の特殊化である。

> **それを壊したとき、Sakutei が自分でそれを直せなくなるか。**（= single point of blindness を持つか）

これは「個別機器の config push で自分を締め出す」問題（§14 の管理プレーンロックアウト）の**データセンター規模版**である。要点は「**同じ機器の中で段が分かれる**」ことにある。

| 要素 | 段 | 理由 |
|---|---|---|
| reconciler / broker ホストの管理NW接続（NIC / IP / VLAN） | **段0** | これが無いと何にも到達できない |
| 管理NWコアの**到達性の最小核**（段0 収容ポート + 最小 L2/L3） | **段0** | 落とすと全管理対象が全盲 |
| 管理NWコアの**残りの config**（ACL / 追加 VLAN / 監視設定） | 段1 | 到達性を壊さない範囲 |
| 管理NWのアクセススイッチ（末端） | 段1 | 一部が落ちても全体は生きる |
| 各サーバの BMC 設定（IP / 認証） | 段1 | 1台死んでも他は生きる |

管理NWコアスイッチは、**到達性の最小核だけを段0**（Ansible が構築し Sakutei は触らない）とし、それ以外の config は段1（Sakutei が収束）とする。これは §5 の managed fields と同じ発想——同じ対象の中で、触ってよいフィールドと触ってはいけないフィールドを分ける。

**管理NW機器の自傷は3層で防ぐ。**

1. `tier: foundation`（§8）——管理NW機器はその先の全機器の上流であり、依存の根の典型例。
2. commit-confirm（§14）——到達性を切っても自動ロールバックする。**管理NW機器では `Capabilities.CommitConfirm` を必須とする**。
3. platform 層 + PR 必須 + 変更予算厳格（§7・§13）——管理NWコアの変更は全機器を全盲にしうる。

**管理 NW の要件（CommitConfirm 不在なら型で Apply 禁止）**：commit-confirm は Apply 契約の transactional / candidate-commit であり、v2 の NW 機器 provider で入る（v1a/v1b には無い）。commit-confirm 未実装のまま管理NWコアを Apply すると、config push で自分を締め出す経路が残る。→ **v1 では管理NWコアを Apply 対象にしない**。NW 機器 provider は `Capabilities.CommitConfirm` が false の間は「**Apply 不可・観測のみ**」を Capabilities で機械的に強制し、ロックアウト経路を型で塞ぐ。到達性の最小核はそもそも段0（Ansible）で Sakutei は触らない。

**OOB（帯域外）= ネットワーク版 Break Glass**：管理NW そのものが死んだときの経路として、コンソールサーバ（シリアル集約）/ OOB 専用NW を持つ。これは Sakutei の管理対象外に置き、Break Glass の経路（§20）として扱う。

**管理系とデータ系は別系統である。** 上記は管理系（BMC アクセス・reconciler から全機器への到達性）の話であり、実運用ではデータ系（VM トラフィック・分散ストレージのレプリケーション・live migration）がもう一系統存在する。両者は帯域も経路も異なる。データ系が全断しても reconciler は管理系経由で機器を制御でき自分で直せる（single point of blindness にならない）ため、**データ系は全て段1**とする。データ系スイッチ自体の設定変更も管理系経由で行い、「データ系を壊しても、データ系を直す経路は生きている」を保証する。

### §23.5 コールドスタート順序と段0 の SPOF

段0 は VM-HA でノード障害には備えるが、以下3点は個別に確定する。いずれも段0 の生命線である。

**（1）コールドスタート順序（Vault unseal 先行）**：reconciler は broker に、broker は Vault に、Vault は unseal（人手）に依存する。VM-HA が3つを同時起動すると、reconciler が Vault 未 unseal のまま起動し**全ターゲット unknown で空回り**する。→ 起動シーケンスを `ready_condition`（§11）相当で明文化する：`Vault unseal 完了 → broker healthy → reconciler start`。reconciler は依存が unready の間は **Observe すら試みず待機**する（空回りログの氾濫を防ぐ）。

**（2）統合 Ceph の SPOF と障害ドメイン分散**：統合 Ceph は段0 の全 VM（Vault/broker/reconciler）のストレージであり、**全損すると3つが同時に落ちる = 段0 内の単一障害点**である。3ノード Ceph の 2ノード同時障害（電源・空調・ラック単位）は quorum 喪失となる。全損時の RTO は Break Glass（§20・Ansible + unseal key）による人手再構築であることをリスクとして明記する。緩和として **Ceph の障害ドメインを分散**する（3ノードを別ラック / 別電源 / 別空調に配置）ことを段0 の人手運用要件に含める。段0 の SPOF が「共有 Ceph 1つ」に集約されている事実は隠さない。

**（3）段0 サービスの自己収束自傷を除外する**：reconciler は broker/Vault を管理対象（Resource）にでき、**誤った理想変更で自分の依存先（broker 到達設定・Vault アドレス）を壊すと、自分が全盲になり直せない**。管理NW で「到達性の最小核は段0（Sakutei は触らない）」と切り分けたのと**同じ切り分けを段0 サービスにも適用**する：reconciler は自分の broker 到達資格・Vault アドレスを**収束対象から除外**する（managed field 的除外 + `external_owner` 相当の「触らない最小核」宣言・§7）。commit-confirm はネットワーク専用でこれらには効かないため、除外で塞ぐ。

**Vault 断の全盲時間の緩和**：broker の短命資格 TTL キャッシュ（§20）は「短命」ゆえ、人手 unseal の MTTR より先に切れ、最も必要な「長時間 Vault 断」のときに全盲を防げない。→ 段0 に限り **Vault auto-unseal（HSM / Transit / cloud KMS）を選択肢として採る**。「人間の金庫に着地」の哲学は unseal key の最終保管に残しつつ（auto-unseal 用鍵を Shamir 分割 + 物理保管）、日常の再起動 unseal を自動化して MTTR から人間時間を外す。あるいは **Observe 専用の read-only 低権限資格を長め TTL で別枠キャッシュ**する（破壊はできないが全盲は防ぐ）。

段0 の人手運用として残るのは、Proxmox 3ノードのハード保守、管理NW到達性の最小核と OOB 経路の維持、Ceph 健全性監視、unseal key の金庫管理、たまのバージョンアップ（Ansible）である。頻度はハード信頼性と Vault の運用形態に依存し、Vault の sealed-on-restart はノード障害・保守再起動のたびに unseal を要求する。導入時に「段0 は片手間では回らない」ことを正直に提示する。

### §23.6 段0-minimal（1ノード縮退形）

段0（3ノード + Ceph + Vault-HA）は 1〜2 台構成には過大であり、ホームラボ・スモールオフィス帯を閉ざす。**段0-minimal を第一級の縮退形**として定義し、solo プロファイルの既定とする。

| 項目 | 段0（標準） | 段0-minimal |
|---|---|---|
| ノード数 | Proxmox 3ノード + VM-HA | **単一ノード**（HA なし） |
| ストレージ | 統合 Ceph | ローカルディスク（Ceph なし） |
| SecretBackend | Vault（VM-HA 保護） | **file ベース SecretBackend**（Vault なし。§20 seam の `file` 実装） |
| credential | broker（v2）/ SecretRef | SecretRef 直結（broker なし） |
| 可用性 | VM-HA + Ceph 冗長 | **単一障害点を許容**（トレードオフを明示） |

- **可用性トレードオフの明示**：段0-minimal はノード障害で段0 全体が止まる。だが収束デーモンは観測 state レス + level-triggered なので、**復帰後に最後の理想へ自然に再収束**でき、制御面が落ちてもデータ面（既存 VM/機器）は動き続ける。「段0 が止まる = 新規障害の自己修復が止まる」だけであり、1〜2 台構成では許容できるトレードオフである。
- **file ベース SecretBackend** は sealed-on-restart が無いのでコールドスタートが単純になる（§23.5-1 の順序制約が消える）。代わりにシークレットの保護強度は下がるため、ディスク暗号化 + アクセス制御で緩和し、規模が育ったら `SecretBackend` seam 経由で Vault へ移行する（seam があるので後付け可能）。
- 段0-minimal は **v1a で成立する**（broker/allocator/Vault いずれも不要な構成に一致する）。可視化・drift 検出器としての価値（§24.2）は 1〜2 台でも出る。

### §23.7 devenv は Sakutei 自身で構築する（dogfooding）

隔離検証環境である devenv（坪庭・§26.2）は、**Sakutei 自身で構築する**。本システムがインフラ収束エンジンである以上、その検証環境もまた収束の対象であり、自らを自らで立ち上げることが最も強い動作証明になる。段0 の立ち上げ手順（Ansible による不変土台 + reconciler による自律収束）を、devenv でそのまま踏襲する。

---

## §24 ブラウンフィールド

Sakutei はここまで「宣言から作る」前提で設計されているが、現実の導入は逆である——**現実が先にあり、宣言が後から追いつく**。既存基盤への導入は greenfield と質的に異なる問題であり、専用の戦略が要る。貫く原則は「**全既存インフラを配下に置くことを目指さない。価値が出る範囲だけ、安全に**」である。

### §24.1 最大の地雷：不完全な理想 × prune

> 理想状態が不完全なまま closed-world prune を有効にすると、「宣言に書かれていない = 余分」と判定され、本番の大半が削除対象になる。

これは §12 の削除セマンティクスが**正しく機能した結果**であってバグではない。Sakutei は「理想が完全である」ことを前提にするが、導入初期の理想は必ず不完全である。したがって**導入初期は prune を全面禁止する**（全 scope で `DeletionPolicy: ExplicitAbsentOnly`、`pruneable: false`）。closed-world への昇格は §24.4 の基準を満たしてからとする。

### §24.2 信頼獲得の順序：import → alert-only → スコープごと昇格

ブラウンフィールド導入は、技術以上に**組織の信頼獲得プロセス**である。Sakutei が正しく動いても、運用チームが受け入れなければ導入は失敗する。信頼は次の順序で獲得する。

**段階0：読むだけから始める（alert-only 強制）**。最初は一切 Apply しない。全リソースを `remediation: alert-only`（§14）に固定し、全 provider は Observe だけを動かす。この段階でも Sakutei は**構成の可視化ツール**として機能する——既存基盤の実態はたいてい誰も正確に把握しておらず、**実態を暴くことが最初の価値提供であり、信頼獲得の入口**になる。

**scope 単位の段階的昇格**。一気に全体を載せない。scope（§12）を最小単位で切り、1つずつ昇格させる。

```
段階0：全体 alert-only + prune 禁止（観測と可視化のみ）
段階1：1 scope を import → 人が理想を整理 → alert-only で観察
段階2：昇格基準を満たしたら auto + closed-world（prune）を有効化 ※その scope だけ
段階3：次の scope へ
```

**昇格基準**：

> その scope で **N 日間、diff がゼロ（または全て説明可能な diff）**であること。

diff がゼロにならないなら、それは「理想が不完全」か「誰かが手で触っている」かのどちらかであり、**どちらも昇格してはいけないサイン**である。管理外の領域には一切触れない（open-world のまま放置）。closed-world は境界の宣言で作られる（§12）——**境界を宣言できた範囲だけが Sakutei の領土**である。

### §24.3 人の変更を早々に巻き戻さない

既存基盤には人が手で触る運用が根付いており、Sakutei を入れた瞬間に両者が衝突する。誰かが緊急対応で設定を手で変えたとき、Sakutei が drift として即座に元に戻せば人の対応を潰す。**これが一度でも起きれば、運用チームは Sakutei を信用しなくなる**（そしてツールは殺される）。

対策は alert-only で drift 検知器として先に信頼を得ることである。導入初期に「勝手に戻す」を**絶対にしない**。人が手で変えたら Sakutei は「理想と違う」と報告するだけとし、報告を見て人が判断する（正しい変更なら理想 git を更新する / 誤りなら戻す）。信頼が積もってから `auto` に昇格する。**`alert-only` → `auto` の移行が、そのまま組織の信頼獲得のプロセスになる**。

### §24.4 import（reverse-render）

数千リソースの理想を手で書き起こすのは非現実的である。**Observe した現実をそのまま理想状態として書き出す**。

```
sakutei import --scope <範囲> --out intent/
  → Observe した現実を intent（YAML）として出力
  → 「今の現実 = 理想」の状態から始める（diff がゼロになる）
```

Sakutei の import は Terraform の import より構造的に楽である。観測 state を持たないため、import は「state ファイルを作る」のではなく**単に現実を YAML に書き出すだけ**であり、state と現実がズレる Terraform import 特有の苦しみが原理的に発生しない。実装も `Lister`（§5）があれば足りる。ただし2つの注意がある。

- **import した YAML は「理想」ではなく「現状のスナップショット」**である。既存基盤には設定ミス・レガシー・意図しない差異が含まれ、そのまま理想にすると間違った現状を永久に維持することになる。**import 後に人が読み、意図を反映して整理する工程は避けられない**（ここは人力）。
- **managed fields の切り分けが import 時に必要**（§5）。DRS が動かす配置・動的 IP・runtime な値を理想に取り込むと永久に喧嘩する。→ **managed 候補の自動サジェスト**：import 出力に「N 周観測して毎周値が変わるフィールド」を managed 候補としてマークし、人の切り分けを「ゼロから」から「サジェストの承認/却下」に下げる。これにより §24.2 の昇格基準も「diff ゼロ」でなく「**意味のある diff がゼロ（managed 除外後）**」に緩められ、ノイズと本物の drift を分離できる（アラート疲労の回避）。この機構は Sync Mode の commit 間引き（§24.5）と共通化する。

### §24.5 Sync Mode（reality → intent 一方向反映）

§24.4 の `import` は離散・手動の操作である。だが実際の移行では、Sakutei 管理へ切り替えるまでの数ヶ月間、対象 scope は引き続き正常な人手運用の対象であり続ける。この期間の設定変更は緊急対応でも逸脱でもなく、その基盤にとっての**正常な運用そのもの**であり、`import` を都度手動実行する運用は現実的でない。**現実の変化を継続的に intent（git）へ記録し続ける仕組み**が Sync Mode である。

scope は排他な2つの mode を持つ。

```go
type ScopeMode int
const (
    ConvergeMode ScopeMode = iota // 既定・従来通り。intent（git）→現実へ収束
    SyncMode                       // 現実→intent（git）へ片方向反映。Apply には一切接続しない
)
```

1つの scope は同時に両方の mode になれない。ロード時に検証し、矛盾する宣言は理想状態ロード時に拒否する（§5 の循環検出・prune 検証と同じ入口・同じ思想）。これは `DeletionPolicy`（§12）と同種の scope に紐づくポリシー enum であり、`Provider` / `Executor` のような実装差し替え可能な interface（seam）とは性質が異なる。

```yaml
scope: { site: legacy-dc, cluster: legacy-cluster-a }
mode: sync
sync:
  target_path: generated/legacy-dc/cluster-a/   # ★ generated/ 配下（下記参照）
```

**動作**（通常の収束サイクルとは別ループ）：

```
毎周：
  1. List（§5）で scope 内の現実を取得
  2. 前回 sync 時のスナップショットと比較
  3. 差分があれば intent ファイルへ書き出し + commit（コミット者はシステム）
  4. render → Diff → Apply には一切渡さない（意図的に非接続）
```

`Lister` があれば実装でき、内部ロジックが `import` と共通する——本機能は import を「都度手動」から「scope に紐づく継続ループ」に変えるものであり、新しい Observe/List の実装は要らない。

**なぜ双方向にしないか**：「intent → 現実への収束」と「現実 → intent への反映」を同一 scope で同時に有効化すると、人が書いた intent が次周の反映で即座に上書きされ、原因タグ A（§10）が現実のわずかな揺れと区別できなくなる。**片方向のみとし Apply 経路には一切接続しない**ことで構造的に回避する。これは「シークレット世代を git に書き戻さない」（§20）と同じ思想——**書き戻しループを作らない**。

**generated/ 規律を適用する**（§21）：片方向にしても、人が sync 対象パスを「普通の intent」だと誤解して直接編集すると次周の反映でサイレントに上書きされる、という穴が残る。RenderedSource の生成物に定めた規律——`generated/` 配下に置き、冒頭に自動生成ヘッダを付し、直接コミットを拒否する——を Sync Mode の出力にも同じく適用する。commit の attribution はシステムとして明確にし（§26.3）、人の intent commit と混同されないようにする。commit 粒度は衛生の問題であり正しさには影響しない——意味のない差分まで毎周 commit すると監査ログの可読性が下がるため、commit は間引くか意味のあるフィールドのみ normalize して比較する。

Sync Mode は `import` / `prune` / `closed-world` と同じく `Capabilities.List` に束ねられる（§5）。List できないプロバイダの scope は Sync Mode の対象にできない。

**切替（cutover）：Sync Mode → Converge Mode**。数ヶ月の同期期間を経て準備が整った時点で、離散的な1ステップとして切り替える。このタイミングのみ人のレビューが要る——理由は正当性判断ではなく、**§24.4 で指摘した managed fields の切り分け**（DRS が動かす配置・動的 IP 等の runtime 値を intent に取り込まない）が生スナップショットのままではできないためである。

```
1. Sync Mode を停止（最新 commit を凍結スナップショットとする）
2. 人がレビュー：managed fields の切り分け・不要な履歴ノイズの整理（§24.4 と同一の作業）
3. scope の mode を converge へ切替、remediation: alert-only で開始
4. §24.2 の通常の段階的昇格プロセス（N日間 diff ゼロ → auto 化）へ合流
```

### §24.6 観測不能資産の階級（observability:none / ledger_only）

ブラウンフィールドには **Observe すら不可能な資産**（API を持たない旧機器、ドキュメントにしか存在しない配線、ベンダーロックで読めない箱）が必ずある。これらを扱う型が無いと `unknown` の運用負荷が積み上がるだけになる。この資産階級（`observability: none` / `ledger_only` / `last_verified`）の型定義・prune 巻き添えの遮断・時間ベースの人手検証は **§5.8 を正とする**——ブラウンフィールドはこの型が最も必要になる現場であり、本節はその適用文脈である。

- **`last_verified` による鮮度述語**：`last_verified` が古くなったら「人が目視確認せよ」を health 述語（§25.2 の時間ベース health 述語と同型）として上げる。observe できない資産の唯一の「鮮度」は人の確認である。
- これは v1a で型として持つ（後から足すと台帳の意味論が変わる）。実際の運用フローは v1b（brownfield）。

### §24.7 導入順序と escape hatch

導入はリスクとリターンで順序づける。

1. **安全で価値が出る周辺から**：DNS レコード / 監視設定 / 証明書。壊れても影響が局所的で、drift 検知の価値が即座に出る。
2. **read-heavy な可視化**：全 VM / 全 FW ルールを Observe して実態レポートを出す（Apply しない）。既存基盤の実態を暴くこと自体が信頼獲得になる。
3. **新規構築部分から `auto`**：既存を触るより、これから作るクラスタ / テナントを最初から Sakutei で作る。greenfield 部分で実績を作り、brownfield は後回しにする。
4. **既存の中核は最後**（ハイパーバイザー / コア NW）。**コア既存インフラは永久に alert-only のままでよい**。

**escape hatch（freeze）は初日から用意する。** 導入初期こそ Sakutei を止める手段が要る。グローバル freeze（§14）を1コマンドで叩けるようにし、「おかしい → まず止める → 調べる」を即座にできるようにする。Sakutei を止めても本番は動き続ける（制御面が落ちてもデータ面は動く・§23）ことを運用チームが理解していることが重要である。**「いつでも止められる」と分かっていることが、新しいツールの受容を助ける。**

---

## §25 災害と復旧

被災時には**収束エンジン自身が死んでいる**（プライマリに居たなら）ことがあり、フェイルオーバーを指示する主体が不在になりうる。DR 自動化は制御面のマルチサイトが前提であり、自動化は最後発でよい。本節は、DR を人間トリガで安全に発動するための経路を定める。

### §25.1 DR モード

マルチサイト DR では、被災サイト（例：東京）が持っていた**通常の書込経路（git の正・PR 承認・署名鍵）そのものが被災で到達不能**になりうる。すると生存サイト（例：大阪）が引き継ごうとしても、PR を承認する経路・commit に署名する鍵が東京依存のままだと、**平時のガバナンスがそのまま DR の障壁になる**——収束の仕組みは生きているのに正への書込みができない。これを2つの経路で断つ。

**（a）DR intent のプリステージ**（平時にレビュー済みの構成を保持）。大阪が東京 scope を引き継ぐための構成（claim 変更を含む）を、**平時にレビュー・承認済みの状態でリポジトリに保持**する。発動時は新規レビューを要さず、**パラメータ / claim の切替のみ**で有効化する。

```yaml
# 平時に PR 承認済みで保持。発動は enabled: true への切替（または claim 切替）のみ
dr_plan:
  name: tokyo-to-osaka
  primary: { site: tokyo }
  standby: { site: osaka }
  on_activation:
    claim_change: { from: {site: tokyo}, to: {site: osaka} }  # 大阪が東京 scope を claim（§17）
    enabled: false        # ★平時は false。発動 = true（この1点の切替が DR 発動）
```

DR 発動時に新規レビューが不要なのが要点である。承認は「平時に、東京が健在なうちに」済ませておく——被災後に東京の承認者に PR レビューを頼む必要がなくなる。claim 変更（所有権ハンドオフ・§17）がプリステージ済みなので、大阪デーモンは切替後の周期で東京 scope を収束開始する。プリステージ構成は generated ではなく人が書く intent であり、通常の PR フローで平時にレビューされる（§7 の platform 書込ポリシーに従う）。発動フラグの切替だけを緊急時に行う。

**（b）書込 break-glass**（git の正へ到達不能時）。git の正（中央リポジトリ）へ到達不能な状況では、被災を免れたサイトのローカルミラー（§25.2）への**署名付きコミット**を許可する break-glass 型の書込経路を第一級化する。これは Break Glass 原則（「使えるが、こっそりは使えない」・§20）の**書込版**である。

- **位置づけ**：機器へ入る（認証）の Break Glass に対し、これは正へ書く（書込）の Break Glass である。既存の Break Glass 原則をそのまま適用する——オフライン相当の専用鍵 / 使用が必ず検出される / 使用後 rotate。
- **必ず検出される**：ローカルミラーへの break-glass コミットは `BreakGlassWriteUsed`（最高 severity）を Event → sink（§26）に流し、S4 監査（§26）に append する。「こっそり書く」ができない。
- **事後合流 PR とセット**：break-glass コミットは**必ず**「中央の正が復旧したら事後合流する手順」と一体で第一級化する。ローカルミラーの署名付きコミットを、正の復旧後に **PR として起票し事後レビュー**する（緊急時は書けるが、監査は後から必ず通る）。これにより DR 中に緊急で書いた変更が正の履歴に正規に取り込まれ、分岐したまま放置されない。
- **ローテーション**：break-glass 書込鍵は使用後 rotate する（使い捨て原則）。

**貫く原則**：DR 経路も「事前チェック最小・事後監査最大」——被災時は速度を優先して生存サイトが書けるようにし、抑止は確実な発覚（Event + S4）+ 事後合流の強制で担保する。平時のガバナンス（PR 承認・署名）を DR で捨てるのではなく、**（a）で平時に前倒し、（b）で緊急書込を検出可能な break-glass に閉じ込める**の二段構えで、「被災サイトへの承認・署名依存」を構造的に断つ。

DR intent のプリステージ（a）は claim ハンドオフ（§17・v1b）と DR Workflow（§6・v1.5）の組み合わせで表現でき、新プリミティブ不要である。書込 break-glass（b）は Break Glass（§20）と Event/S4（§26）の再利用であり、書込 break-glass の型と事後合流 PR 起票は v1.5（最小 Workflow と同じ段）とする。

### §25.2 git 外部化とローカルミラー（読み取り全断耐性）

git は外部（GitHub/GitLab）に置き、各段0 ノードに**ローカルミラー**を持つ（§23.3）。外部 git 不達時は最後に pull した状態で収束を継続する（**稼働必須依存にしない**）。これにより中央リポジトリへの読み取りが全断しても、reconciler は最後の理想へ向けて収束を続けられる。§25.1（b）の書込 break-glass も、このローカルミラーを書込先とすることで成立する。

バックアップ / DR の各側面は既存プリミティブに割り当たる——バックアップ構成は Resource（収束）、バックアップ結果（取れたか・鮮度・復元可能性）は health / canary（収束外・§26）、リストアは Task（一回性）、DR フェイルオーバーは invariant 付き Workflow（§6）である。最大の落とし穴は「構成が converged でも毎晩失敗して1ヶ月バックアップが無い」であり、**構成の収束は結果の健全性を保証しない**。鮮度は時間ベースの health 述語、復元可能性は隔離環境への定期リストア canary、3-2-1 等のポリシーは invariant で守る。

### §25.3 S2 喪失は事故（quorum ストア + fencing）

観測 state は毎周再導出するため喪失しても再構築できるが、Task/Workflow の進行（S2・§4）は**喪失が事故になる** state である。承認ゲート待ち・rolling 進行位置・invariant 状態などが失われると、二重実行や中途半端な適用が起こりうる。したがって **S2 は quorum ストアに永続化し、fencing token（§17）で二重 claim を検知**する。DR で claim をハンドオフする際も、この fencing により旧プライマリの遅延した書込みを弾き、split-brain を防ぐ。S2 の永続化先を持つことは、状態分類（§4）における S2 の定義そのものの帰結である。

### §25.4 サイト分断時の挙動と DR 判定

サイト分断（生存サイトから被災サイトの現実が観測できない状態）では、**観測できない資産を `unknown` として扱う**。ここで §12 の非対称原則をそのまま適用する——**確信が持てないとき、加算はしても減算はしない**。すなわち：

- unknown な資産を「存在しない」とみなして prune してはならない（`unknown ≠ absent`）。
- 分断中は**加算のみ**（生存サイトで必要な新規リソースの作成）を許し、分断先の資産への破壊的操作は halt する。

**DR 判定は人間トリガとする。** 観測 state レス設計では「現在状態のキャッシュ」を持たないため、「被災か・一時的な観測断か」をシステムが自動判定する材料を原理的に持たない。分断を「災害」と解釈して自動フェイルオーバーすると、一時的なネットワーク断で split-brain を誘発する。したがって DR の発動（§25.1 の `enabled: true` 切替 / claim ハンドオフ）は**人間が明示的にトリガ**する。システムは分断を Event として上げるところまでを担い、災害と断じて経路を切り替える判断は人に残す。

---

## §26 検証と監査

本システムは**インフラを操作対象にする**ため、「操作される側」を用意しないと検証にならない。しかも検証したいのは正常系より**危険な収束挙動**（prune / 爆発半径 / drift 自己修復 / converging 誤認）であり、わざと壊れる状況を安全に再現する必要がある。実機は論外である。

### §26.1 検証の4段

検証は忠実度と速度の異なる4段を積む。速い順に、下段ほど頻繁に、上段ほど最終確認として回す。

1. **ユニット + フェイク Provider（主戦場・秒）**。収束ロジック・DAG・prune・爆発半径・原因タグ・in-flight 排他の検証に本物は要らない。むしろフェイクの方が良い——**障害注入が自由**（unknown を返す / N 回に1回失敗 / converging 継続）、高速（秒）、決定的（CI 可）。`Provider` / `Executor` interface に挿すだけで収束エンジン本体を実インフラなしで完全にテストでき、これは seam 設計（§5）の副産物である。**開発の8割はここで済む。** 毎コミット CI で回す。
2. **単一 Provider 統合（分）**。provider 実装そのものの正しさを、軽量な本物（DNS=CoreDNS / コンテナ=Podman / FW=nftables / Linux 系）に対して検証する。
3. **devenv フルスタック E2E（時間）**。VM 払い出し → DNS 登録 → FW 設定 のように、段をまたぐ収束を通しで確認する。重い本物のミニチュア（Proxmox ネスト / containerlab / MinIO / targetcli / Vault）を含む。
4. **chaos（意図的破壊・最重要）**。provider をわざと落とす / drift を注入する / 大量削除をトリガする → **爆発半径制御・alert-only 縮退・oscillation（振動）検知・`unknown ≠ absent` が効くか**を検証する。本システムの肝は「壊れそうで止まる」ことなので、**壊す検証が最重要**である。カオスの常設先は twin（§26.2）とする。

### §26.2 devenv（坪庭）と twin（縮景）

**devenv（坪庭）** は開発・検証環境の総称（上記4段の層1〜3 を含む）。メモリの大きい物理1台に、ネストで段0〜段2 を丸ごと再現する（実機ゼロ）。町家の内側に囲い込まれた一坪の庭のように、敷地（本番）の内側に隔離された、本物と同じ材料でできた小さな庭である。**devenv 自体を Sakutei で構築する**（dogfooding・§23.7）。

**twin（縮景）** は devenv の最終像であり、実在の景観を縮小再現する作庭技法にちなむ。**本番の intent を同じ reconciler でツインに流し込み、本番とツインを同一の宣言から構築する**——検証環境が本番と構造的に一致する。定義は「**render の外部入力を差し替えた環境**」である（render を第一級化したことで「入力の差し替え点」として自然に表現できる・§9）。

- **ツイン専用の AllocationAuthority**（本番スナップショットから seed すれば具体 ID の忠実度も確保）。
- **専用/フェイクの SecretBackend**（本番 Vault を叩かない）。
- inventory もツイン用スナップショット。
- → **本番プールを消費せず、本番シークレットも展開しない**。

用途は、変更の事前検証（爆発半径・カスケード波及 D の可視化）/ provider 更新の検証（幽霊 diff C を本番前に観測）/ DR・ローリングのリハーサル / カオスの常設先である。

### §26.3 監査（S4）

Event を永続化（S4・§4）し、**append-only + 連鎖ハッシュ**で改竄耐性を持たせる。

- **git が理想変更履歴の半分を担う**（コミット連鎖がそのまま改竄検知）。Apply 実行記録は追記専用ストアへ書く。
- **外部アンカーが必須**：連鎖ハッシュ単体では先頭（head）を外部固定しないと全体書き換えで改竄可能である。head hash を Event として sink に流し、記録する。
- **アンカー間隔を短縮し即時 push する**：日次アンカーは「最後のアンカーから今日まで」の最大 24h 分を検知されずに改竄できる窓を残す。→ **監査を外部 SIEM / WORM / append-only 専用サービスへ即時（毎周 or 毎時）push** し、head アンカー間隔を短縮する。append-only の enforcement を「アプリの規律」でなく**ストレージ層（WORM / 外部即時 push）**で担保する（侵害されたホストが自分の犯行ログを書き換えられないように）。監査書き込みは reconciler と別権限・別プロセスとする。
- **S4 の永続化先**：S4 は **WORM ストレージ or 外部即時 push を正式な永続化先とする**。選択肢は排他でなく、どちらか一方（または併用）で「侵害ホストが自分の犯行ログを改竄できない」を満たせば足りる——WORM（write-once ストレージ）なら書込み後の改竄が物理的に不可能、外部即時 push なら侵害ホストの外に記録が出た後の改竄が不可能である。
- **帰属**：収束は自律動作なので「誰が」が問題になるが、**変更の原因タグ（A/B/C/D/E・§10）がそのまま監査の帰属になる**（自己修復か / 誰の commit 由来か / どの承認ゲートを誰が通したか）。原因区別の設計が監査で再利用される。
- **秘密の監査は SecretBackend 側の機能を使う**（自前実装しない）：Vault KV v2 は更新のたびに version history を自動保持し、audit log は全 read/write を誰が行ったかを記録する。→ git（理想が誰にどう変更されたか）+ SecretBackend（秘密が何回更新され誰に読まれたか）+ Event（S4・この2つを繋ぐ収束側の記録）の三者で監査が完結する。

### §26.4 適合レポート（status）

**適合（conformance）**——宣言どおり在るか——は収束ループの中で常時判定されている。その結果を人向け・監査向けに切り出すのが適合レポートである。

```
sakutei status       # drifted 一覧（適合レポート）
```

`status` は現時点で理想と現実がずれている（drifted）リソースの一覧を返す。これは health（在るものが動いているか・監視システムに委譲）や合成検証（canary・その操作が今も動くか・§26.5）とは別の「チェック」であり、混同しない——status が扱うのは「宣言どおり在るか」だけである。drifted 一覧はブラウンフィールドの昇格判定（§24.2 の「N 日間 diff ゼロ」）の根拠にもなり、監査向けの適合エビデンスとしても使える。

### §26.5 合成検証（canary）

3つの「チェック」を混同しない——**conformance**（宣言どおり在るか・収束ループが判定・§26.4）、**health**（在るものが動いているか・監視システムに委譲・§22.2）、そして**合成検証（canary）**（その操作が**今も動くか**）の三つである。canary は前二者とは別の**能動的検証**であり、受動監視では絶対に得られない情報を取る。特に狙うのは**削除経路の健全性**——「作成は動くが削除が権限不足で失敗」は現場で頻発し、conformance も health も「差分ゼロ」なので捕まえられない。

「VM を作成・削除できるか」を実際にやってみて確かめる。設計形は**時間トリガで起動する Workflow**であり、隔離 scope + TTL 自動 GC + cleanup 反復失敗で自動停止を必須とする。

```yaml
synthetic_check:
  name: vm-lifecycle-canary
  every: 1h
  scope: { datastore: test-only, network: canary-vlan }   # ★隔離必須
  steps:
    - create: { from: canary-template, ttl: 10m }          # TTL = 消し忘れ防止
    - assert: { powered_on: true, reachable: true }
    - delete: {}
    - assert: { gone: true }
  on_fail: { alert: page }
```

- **隔離必須**：canary は本番プールに触れず、専用の datastore / VLAN 等の隔離 scope で回す（爆発半径を canary 自身に閉じる）。
- **TTL による自動 GC**：作成物には TTL を付し、消し忘れ（canary 自身が孤児を残す事故）を防ぐ。
- **cleanup 反復失敗で自動停止**：cleanup（delete）が反復して失敗する canary は level に昇格して通知し、自動停止する（自らが drift 源にならないため）。canary 失敗そのものは edge（事件）として1回通知する（§22.3 の単一 Event）。
- **§26.1 の chaos との対比**：chaos は「わざと壊して安全機構が効くか」を twin（§26.2）で検証する破壊試験、canary は「正規の操作経路が今も生きているか」を隔離 scope で常時確かめる健全性検証であり、目的が異なる。実装は Workflow（§6）に載るため v1.5 以降。

### §26.6 CLI の全 verb

バイナリは `sakutei` 単一とし、サブコマンドで機能を分岐する。動詞は業界標準を用いる（既存知識が転用でき、学習コストが生じない）。

```
sakutei                          # 常駐して回り続ける（本来の姿）
sakutei plan                     # 何が変わるか（= Diff で止める。dry-run）
sakutei reconcile                # 一回だけ収束させる（手動キック）
sakutei reconcile --expect-revision <rev>   # 期待する rendered_revision を指定し、ズレていれば中断
sakutei status                   # drifted 一覧（適合レポート・§26.4）
sakutei import --scope <範囲> --out intent/  # 既存の現実を intent として書き出す（§24.4）
sakutei --claim <scope-selector> # 自分が収束を担当する scope を宣言して常駐（静的シャーディング・§17）
```

- **`plan` と `reconcile` はそれぞれ独立に Observe する**（§5）。plan の観測を reconcile に持ち越すと観測 state を再導入してしまうため、両者は別々に現実を見る。
- **`--expect-revision`** は、plan で確認した内容と reconcile が適用する内容の同一性を担保する（間に理想が更新されていれば中断する）。承認ゲートが「plan を1バイト単位で固定する」のではなく「意図を承認し実行時の rendered_revision で同一性を確かめる」思想（§14）と対応する。
- **`--claim`** は全 intent を pull した上で自分の claim 分だけを収束するための起動フラグ（スケールアウトの段2・§17）。シャード割当そのものを git の intent として宣言し、二重 claim は fencing token（§17）で検知する。型は v1a で切り、実装は必要時。
- **`apply` を主動詞に置かない**のは、このシステムが「一回叩くツール」ではなく「回り続けるもの」であることを名前で表明するためである（§8）。

**承認ゲート API**。段階適用（rolling）の手動承認ゲートは HTTP API で通過させる。

```
POST /workflows/{id}/gates/{gate}/approve   # ボディに承認者 ID + 署名
```

ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API で通過（却下 = halt）→ 次ステージ、という流れである（タイムアウトで halt）。承認 API 自体は認可を持たない（誰が叩けるかは上位 IdP・§7）が、**承認された事実は監査（S4）に記録**する。N-of-M・役割制約・申請者≠承認者（separation）などの承認者要件はゲート定義の `requirements` に載せ（§14）、全承認者を S4 に append することで「どの承認ゲートを誰が通したか」が原因タグの帰属（§26.3）と一体で追える。

---


# 第VIII部 実装

本部は Sakutei を実際に作る順序（§27）、採用を左右するプロバイダ SDK の設計（§28）、そして実装中に決める事項と設計では解けない既知の限界（§29）を定める。第II部〜第VII部で確定した機構を、どの順で・どこまで実装するかへ落とし込む。

---

## §27 実装スライス

### §27.1 スライスの基準

v1 を一括の MVP として積むと、後付け不可能なコア型と実装量の大きい機能が同列に並び、規模が過大になる。したがって v1 を **v1a（コア実証）／ v1b（brownfield 導入と採番連携）／ v1.5（手続きの最小形）** に割り、v2 以降を明確に切り離す。分割基準は二つである。

- **後付けすると全プロバイダ・全 scope に波及するか** → **v1a**（コア型・seam・安全機構の骨格）。
- **その機能が無いと使い始められない導入形態があるか** → **v1b / v1.5**（既存基盤への導入・手続き）。

言語は **Go**（全 interface 定義が Go）。バイナリは `sakutei` **単一**とし、サブコマンドで機能を分岐する。状態保存は**メモリ + file（git）が主**で、将来 `DesiredStore` として bbolt / SQLite を差し替える（§4 の状態分類に従い、喪失が事故になる S2 のみ外部永続化する）。**etcd や必須 DB プロセスは持たない**。

### §27.2 v1a — コア実証（≈10 人月）

v1a が実証するのは二つの体験である——**「触っていないのに勝手に理想状態へ戻る」**（level-triggered な収束）と、**「壊れそうで止まる」**（安全機構）。この二つが成立すれば、Sakutei の中核命題は証明される。

**コア interface / 型（後付け不可・最初に型へ）**

1. `Provider` interface（`Schema` / `Capabilities` / `Read` / `Apply`、正準は §5）と `Lister`（prune の前提）。
2. `Observation`（Availability / State / Ready）と `ConvergenceStatus` の分離（dry-run の根拠・§4）。
3. `Capabilities`（機械的縮退・§5）。
4. `SecretRef` 型（自動マスク + 明示 Resolve + 世代フィールド・§20）。
5. `DeletionPolicy` / `pruneable`（既定 false）/ `scope + managed`（§12）。
6. **原因タグ（A/B/C/D/E）** と `intent_revision` / `rendered_revision` の分離（**per-resource 帰属**・§10）。
7. `provider_version` を diff 計算に含める（埋め込みは登録必須契約・§10）。
8. **コンテンツアドレス参照**（`image@sha256`）。
9. `ownership`（platform / tenant）と `remediation mode`（+ `remediation schedule`・§14）。
10. `Executor`（実行境界。v1 は `LocalExecutor` のみだが、**コントローラは Executor 型にしか触らない**・§5）。
11. **コア型（正準形は §4 で確定済み）**：`State`（Schema 付き map）/ `Plan`（操作列 + 破壊性 + 原因タグ + ID + 診断）/ `ID`〈`(provider, scope, id)` 複合〉/ `Scope`（ラベル集合 + パス）/ `Event`（単一型 + kind enum + scope/resource/revision 参照）、および `rendered_revision` 算出定義〈**scope 単位で算出可能**〉。**構造は確定済み**であり、第0スプリントは算出定義の厳密な文書化と型の実装に着手する（新規の着手前決定は残っていない・§29）。
12. **早期収束の seam**：`DesiredSource.Watch`（§21）+ **scoped run（部分収束）+ 合流キュー**（§8）。reconcile/plan とほぼ同一コードパスを部分 DAG に適用するだけなので増分が小さい。v1a では FileSource の webhook/merge フックによる `Watch` を対象とする（DBSource 連動は v2）。
13. **スケールアウトの型切り**（実装は必要時）：`Capabilities.BulkObserve`（§5）+ Executor の per-resource `Read` fallback 経路、`sakutei --claim`（§17）の起動フラグと scope-selector パース。

**機能**

- 収束ループ（level-triggered / DAG / wave / 非同期 Apply / in-flight 排他 / backoff・§8）。
- **`tier: foundation`**（基盤サービスを依存の根として DAG 順序に反映・§8）。
- render パイプライン（2フェーズ / TTL キャッシュ / 障害時は前回値保持 + **incomplete≠empty 急減検知**・§9）。ただし **v1a のフェーズ2 展開は非採番の固定値展開のみ**（採番連携は v1b）。
- **rate limit + halt**（in-flight を含めて評価）+ **破壊的変更の累積時間窓予算**（§13）+ **List 完全性検証**（件数急減 halt・scope 単位閾値宣言・§12）+ **異常検知の "halt-all で凍結" セマンティクス**（§13）。
- **`planned_change` 宣言 + 専用予算レーン**（§13）——大量正当作業の解除弁。型と予算レーン評価は v1a（承認情報 requirements 連携は v1.5）。
- **運用プロファイル（solo / team / enterprise）**（§14）——規模別の安全既定値セット。既定は team。
- `ownership: platform` の破壊的大量変更は **A タグでも承認必須**（悪意・誤操作の両方を止める・§13。solo は cooling-off で代替可・§14）。
- Event 型 + 構造化ログ（JSON）出力。
- conformance レポート + **`sakutei status` のデバッグ体験**（原因タグ + provider 階級表示 + **軽量 last-seen リング**〈S3・非永続で「前周は X だった」を添える〉+ 下流波及 D・§10）。
- `FileSource`（git）+ YAML/JSON `Loader` + JSON Schema 検証（§21）。
- Vault `SecretBackend` 1つ + 段0 からのトークン注入（**provider 資格は SecretRef 直結。broker は無い**・§20）。
- **最小 SDK**：Schema 駆動 diff エンジン（順序付き含む）/ secret マスキング / managed 除外 / エラー分類 / version 埋め込み。**Schema lint はロード拒否レベルで強制**（§28）。
- devenv 層A（フェイク provider）+ 層B（軽量本物）+ カオステスト基盤（§26）。
- **provider 2〜3個**：DNS + VM + FW（順序付き）。**DNS を第一号**とする——Read / Apply が最もクリーンで検証環境も軽く、「触っていないのに勝手に戻る」体験を最小のコードで作れるからである。**管理 NW コアは収束対象にしない**（§23）。
- **非採番 allocation のみ**（採番連携は v1b）。

v1a のゴールは、DNS レコード provider から podman コンテナ / Caddy ルート / TLS 証明書と広げ、数百行のオーケストレータ + provider 数個で level-triggered な自己修復を greenfield / devenv 上に成立させることである。これは §27.3 の SDK フルセット・import・Sync Mode を含む「v1 全体」ではなく、規模は1桁小さい。

### §27.3 v1b — brownfield 導入と採番連携

v1b が実証するのは、**既存の大規模基盤に安全に入れる**ことと、**採番権威と連携して合成リソースを実体化する**ことである。

- **`sakutei import`（reverse-render）**（§24）+ **managed 候補の自動サジェスト**（§24）。
- **`ScopeMode`（converge / sync）+ Sync Mode の反映ループ**（§24）。
- **`AllocationAuthority` interface + 最小実装**（SQLite・§19）と **render フェーズ2 の採番連携展開**（§9）。**採番を使う合成リソースは v1b から**（v1a では非採番の固定値展開のみ）。
- **allocator HA 最小形 + Lookup キャッシュ + waiting-allocation 縮退**（§19）：段0 VM-HA 上の active/standby + fencing、割当事実のシャードローカル無期限キャッシュ（S3）、allocator 停止時の `waiting-allocation` 縮退。**プール分割 by claim**（intent での disjoint レンジ宣言・§19）。
- **テストハーネス**（契約テスト自動生成）+ golden path テンプレート（§28）+ **BulkObserve 等価性テスト**（fake provider 上で `BulkObserve(scope) ≡ {Read(id)}` を検証・§5/§28）。
- **`ready_condition` と複数 Cycle に跨る依存解決**（§8）+ **capacity gate**（§13）——最初の実インフラ provider（VM / 物理）を書く段で必要になる。
- **BulkObserve の実バルク実装 + 一貫性契約**（§5）——バルク API を持つ provider（vCenter / Proxmox 等）から書く。complete-or-error / prune 非流用 / `observed_at` 刻印を SDK で強制。型は v1a で切ってあるので追加は provider 単位で局所。
- **静的シャーディングの実動作**（§17）：`--claim` フィルタ + fencing による二重 claim 検知 + 再シャード Workflow（drain → claim 変更 PR → fencing 世代更新）。型（起動フラグ）は v1a で切り済み。
- **claim の git 配布 + S2-free シャード階級 + エッジ資格局所性**（§17/§20）：`intent/<scope>/...` レイアウト規約 + sparse checkout / partial clone、ミラー中継層、S2-free シャード（Resource 収束のみ）、店舗ローカル file SecretBackend への資格局所常駐、開店/閉店 = claim 変更 PR ハンドオフ。
- **tier 別 Observe 頻度（間引き）** の導入余地（§16 の早期警戒線が立った層から）。
- **inventory 検証**（フェーズ1 の外部入力〈inventory 合成〉が急減・欠損したときの incomplete≠empty 判定・§9）。

### §27.4 v1.5 — 手続きの最小形（v2 から前倒し）

20 構成の適用検討で 12 構成が「事業の核」を Task/Workflow 系に依存させていた。したがって**最も需要の高い最小形を前倒す**。多相 Task 状態機械・自動段階ロールアウトは重く需要が特定領域に偏るため v2 に残す。

- **単純 Task**（§6 単純型）：一度実行して現実側に完了印。migration / 一回性操作。S2 は進行フェーズのみ。
- **最小 Workflow**：**直列（one-at-a-time）+ 不変条件（invariant）+ 手動承認ゲート**のみ（§6/§14）。ローリング / DR の骨格をこの3要素で表現する。**動的 select・自動 canary 判定・自動段階ロールアウトは v2**。
- **承認ゲートの N-of-M 拡張**（§14）——承認者要件（N-of-M・役割制約・separation〈申請者≠承認者〉・全承認者の S4 記録）。ゲート定義の `requirements` と承認 API の署名対応のみで実装。
- **災害時運用経路（DR モード）**（§25）——DR intent のプリステージ（claim ハンドオフ + DR Workflow の組み合わせ）+ 緊急ローカル書込経路（書込 break-glass の型 + Event/S4 検出 + 事後合流 PR 起票）。
- **`planned_change` の承認情報連携**（§13）——v1a で入れた予算レーンに、承認 requirements を接続する。
- **`remediation schedule`**（§14）——保守窓文化の構成で必須なので v1.5 で入れる。

### §27.5 v2 以降

- **多相 Task 状態機械**（§6 多相型）。
- **自動段階ロールアウト + 自動 canary ゲート**（§14）/ health（observability as Resource）/ 常設 canary / Prometheus exporter。
- **`DBSource`**（および `DBSource` 連動の `Watch`・§21）。
- **credential-broker**（§20）——JIT 短命発行 / bastion / 個人単位の到達資格。
- **分散 Executor + lease 縮退**（§18）+ **動的ルーター / 自動リバランス**（§17 段3）。
- **Observe 間引き・tier 別頻度・watch 併用**の本格化（§16）。
- **intent UI（catalog）**（§21）/ twin フル機能（§26）/ 抽象リソース（§28.8）。

**注**：早期収束の Watch seam + scoped run（§8）と静的シャーディング（§17）は v2 ではなく v1a / v1b に前倒し済みである。v2 に残るのは **動的**ルーター / 自動リバランスと DBSource 連動 Watch のみである。

### §27.6 作らないもの（委譲）

シークレットストア本体（Vault）/ 時系列監視・通知ルーティング（Prometheus・Alertmanager）/ 配置最適化（DRS）/ 認可・課金・ポータル（上位プラットフォーム）/ アプリ層のデプロイ・スケール（k8s）/ CMDB 全体（採番のみ自作）。委譲の根拠と停止影響は §22 を正とする。

### §27.7 スライスの相対規模

| スライス | 実証するもの | 目安 |
|---|---|---|
| **v1a** | 触っていないのに戻る + 壊れそうで止まる（コア型・収束ループ・安全機構・最小 SDK・provider 2〜3個・devenv） | **≈10 人月** |
| **v1b** | 既存基盤に安全に入る + 採番連携（import / Sync / tier 別頻度 / 静的シャーディング / inventory 検証） | v1a と合わせ「使い始められる」水準 |
| **v1.5** | 手続きの最小形（単純 Task / 最小 Workflow / N-of-M 承認 / DR / remediation schedule） | 12 構成が依存する核 |
| v1 全体 | 上記 v1a + v1b + v1.5 の合計 | ≈22.5 人月（v1a とは1桁違う） |

---

## §28 Provider SDK

### §28.1 なぜ SDK が採用を決めるか

**プロバイダの書きやすさが Sakutei の普及を直接決める**。エコシステムでプロバイダの数と品質を得た側が広く使われる——これは先行ツールの教訓である。したがって設計の肝は、**プロバイダ作者が書く最小を極限まで削り、面倒はコア側の共通ライブラリ（SDK）が引き受ける**ことにある。作者が書くべきは **約4関数 + `List`**（prune を許す場合）に留め、残りはすべて SDK が担う。

### §28.2 作者が書くもの（約4関数）

削りに削ると、作者が書くべきは次の4つ。prune を許すなら `Lister.List` を1つ足す（正準 interface は §5）。

- `Schema()` — フィールド定義（型 / 必須 / secret / managed / ordered / identity / computed）。
- `Capabilities()` — TrueObserve / List / Converging / CommitConfirm / BulkObserve。
- `Read(ctx, id)` — 現実を読む（desired を知らない）。戻りは `Observation`。
- `Apply(ctx, plan)` — 現実を変える（create / update / delete を1関数に統合）。
- （任意）`List(ctx, scope)` — scope 内の現実の全 ID。prune の前提。
- （任意）`BulkObserve(ctx, scope)` — scope 内をまとめて Read（バルク API を持つ場合）。

設計上の負担軽減は三つである。**（1） Diff を書かせない**——差分計算は Schema を使ってコアが汎用実装する（順序付きコレクションのような特殊 diff も Schema で `ordered` と宣言すればコアが適用する）。**（2） create/update/delete を Apply 1本に統合**——`Plan` に「何をすべきか」が入っているので分岐は作者の自由。**（3） Schema がすべての鍵**——型・必須・secret・managed・ordered・identity・computed を宣言すれば、コアが diff・検証・マスキング・managed 除外を全自動化できる。

### §28.3 SDK が担うもの

| SDK 機能 | 何を肩代わりするか | 関連 |
|---|---|---|
| 差分エンジン | Schema ベースの汎用 diff（順序付き含む） | §5 |
| secret マスキング | Schema で `secret` 宣言したフィールドを自動 `SecretRef` 化・ログマスク | §20 |
| managed fields 除外 | Schema で `managed` 宣言したフィールドを diff 対象外に自動除外 | §7 |
| version 埋め込み | provider ビルド version を diff 入力に自動合成（原因タグ C） | §10 |
| エラー分類ヘルパー | Apply のエラーを transient / permanent に型付け | §5.5 |
| テストハーネス | フェイクプロバイダ基盤・Read/Apply の契約テスト自動生成 | §26 |
| **BulkObserve 等価性テスト** | fake provider 上で `BulkObserve(scope) ≡ {Read(id) ∀ id ∈ scope}` を検証。これを通ることが「完全階級 + bulk」を名乗る条件（§5） | §5 |
| **Schema lint** | 「secret 宣言し忘れ」「identity 未指定」等を静的検出。**ロード拒否レベルで強制** | §28.4 |

### §28.4 最難関は Schema 設計

実装の重さの本体は Read / Apply のロジックではなく、**Schema を正しく書くこと**である。以下の宣言を誤ると、設計が積み上げた安全保証が**無音で無効化**される（エラーにならず、静かに保証だけが外れる）。

- どのフィールドが **secret**（§20）→ 誤ると資格情報が平文でログに出る。
- どのフィールドが **managed / runtime**（§7）→ 誤ると DRS と永久に喧嘩する。
- どのフィールドが **ordered**（§12）→ 誤ると FW ルールの順序が壊れる。
- どのフィールドが **identity**（削除・参照のキー）→ 誤ると別リソースを消す。

**identity は「運用上の変更頻度」で選ぶ**。技術的な一意性だけで決めてはならない。変更されやすい属性（シリアル番号・MAC アドレス）を identity にすると、その属性が変わるたびに Resource の再生成と理想状態の commit が必要になり、運用コストが跳ね上がる。

> 例：物理サーバの identity をシリアル番号にすると、故障交換のたびに intent の書き換えが要る。**物理トポロジ座標**（「leaf03 スイッチの 12 番ポート」= `leaf03-p12`）を identity にすれば、筐体交換時に理想状態の変更が一切不要になる。シリアルや型番は `managed: true` + `computed` として観測のみ行い、identity にはしない。

したがってプロバイダ実装は「**まず Schema、次に Read/Apply**」の順で書く。SDK の Schema lint が宣言漏れ（secret 未宣言・identity 未指定）を静的に検出し、通らない Schema は理想状態ロード時に拒否する（循環検出・prune 宣言拒否と同じ入口・同じ思想）。

### §28.5 プロバイダの階級（全機能を要求しない）

**階級（完全・標準・最小）の定義は §5.6 を正とする。** ここでは SDK 文脈での含意だけを述べる。

**作者は自分の階級を選べる**。最小階級なら Apply だけ書けばよく参入障壁が劇的に下がる。Capabilities で正直に宣言すれば、コアが自動的に縮退して**安全に**扱う（prune させない・破壊的側に倒す・§5）。「全機能を書けないと参加できない」ではなく「**書ける範囲で参加でき、コアが安全に縮退する**」——これがエコシステムを広げる鍵である。

### §28.6 golden path と事例集

作者を最短経路に乗せるため、SDK は次を用意する。

- **golden path テンプレート**：Schema → Read → Apply の骨格を持つ雛形。最小階級はさらに短い。
- **契約テスト自動生成**：Read / Apply の冪等性・三値の正しさ・List 完全性を fake provider 上で自動検証。
- **worked example**：Provider と合成リソースの完全な実装例を参照実装として提供する（Proxmox 上の MySQL DBaaS の Provider / Schema を `docs/examples/proxmox-mysql-dbaas-schema.md` に置く）。

### §28.7 2層プロバイダ戦略

エンタープライズでの価値の大半は provider の数と品質にあるが、全ベンダーを自前で書き切るのは不可能である。したがって **自前コアと Ansible の二層**で埋める。

- **コア層（自作 provider）**：DNS / FW / VM 等、重要で **Diff 品質・横断項目対応**が要るもの。**完全階級**を目指し、prune・原因タグ・converging・commit-confirm を効かせる。
- **ロングテール層（Ansible provider）**：雑多な機器・OS 設定。**品質を落として広くカバー**する。**changed: true/false のみ**を返し、State を返さない（最小階級）。

**Ansible は「モジュール」だけ借りる**。冪等な実行実体であるモジュールだけを借り、実行モデル（playbook / 一回 push）は借りない。収束ループ（いつ・何を = Sakutei）と実行実体（どう = Ansible モジュール）が噛み合う。

```go
type AnsibleExecutor struct { ... }   // Executor の一実装（§5）
//   Observe → ansible module を check mode で叩く（= Read）
//   Apply   → ansible module を通常実行（冪等適用）
```

Ansible provider では横断保証の多くが落ちる。**check mode は「与えた desired に対する changed 真偽」しか返さず、current state を返さない**——Diff は Ansible 内部で完結し、コントローラ側の Diff・原因タグ A/B の区別・converging 検知・破壊的変更の重み付けが軒並み機能しない（`Capabilities.TrueObserve = false` による機械的縮退）。実装の8割が泥臭いロングテール層である以上、Sakutei の保証群は当面**少数派のコア provider にしか完全には効かない**。これは設計の嘘ではなく、正直に認識すべき制約である（§29）。

**緩和は二つ**。`*_info` 系モジュールで Observe できる対象は `TrueObserve: true` に**部分昇格**でき、縮退は全面ではない。また順序付きコレクション / managed fields / commit-confirm のような**横断機能は Sakutei 側でラップして扱う**（Ansible は1操作の実行だけを担い、賢さは自前側に置く）。時間発展としてはコア実装を広げ Ansible の担当範囲を狭めていくが、専用 API が複雑でベンダー固有性が高い領域（ブロックストレージ製品等）は自前化が割に合わず、Ansible provider が長く残る。

### §28.8 【将来検討】抽象リソースと実装の分離（v1 では扱わない）

依存を**具体型でなく能力（capability）に対して張りたい**場面がある（例：「DB は VM の上で動くが、その VM が VMware か Proxmox かは問わない」）。抽象リソース（役割）とその実装（プロバイダ）の分離であり、k8s の StorageClass / Crossplane Composition と同型である。**v1 では具体依存のみを扱い、以下を将来の検討事項として記録する**（seam として `Provides() []Capability` を予約する余地はあるが、解禁は慎重に行う）。決定として固定しておくのは次の四点である。

- **実装選択は誰が決めるか＝②platform ポリシー主 ＋ ①運用者上書き**。①運用者が明示 / ②platform 層のポリシーが決める（ownership の自然な拡張・主候補）/ ③システムが最適化して選ぶ、の三択のうち、③（配置最適化）は DRS へ委譲済みの領域（§13）であり**踏み込まない**。**②主 ＋ ①上書き**が既存の ownership 設計と整合する。なお「依存の抽象化」（能力に依存する語彙）は render の入力であって収束の対象ではない——抽象は人が書く語彙であり、収束が扱う実体は具体化後のリソースである（§9）。
- **最大の危険は leaky abstraction**。「VM」は VMware と Proxmox で実際には異なる（スナップショットの意味・NW モデル・live migration・managed fields）。→ **抽象は最大公約数の能力に限定**し、実装固有機能が要るなら具体プロバイダを名指す。「隠してよい差異か、隠すと事故る差異か」の見極めがドメインごとの設計判断である（VM は抽象化しやすい、ストレージは実装差が大きく慎重に）。
- **段階導入**。全面導入せず、`compute` のような抽象化しやすい少数の capability から始め、実装差の大きい領域は具体型のまま残す。役割能力は操作能力（`Capabilities`・§5）とは別の `Provides()` として宣言し、抽象と具体の適合は interface 型チェックで検証する。
- **解禁トリガ＝2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できず、早すぎる抽象化は最大公約数を誤る（§29.2 の再検討トリガ表に登録する）。

---

## §29 残る課題と既知の限界

設計は概ね閉じている。**実装着手前に決めるべきコア型・着手前決定はすべて確定済み**（§4 でコア型正準形、§20 で S4 永続化先まで確定）であり、この索引を実装中に**再オープンさせない**運用を敷く（着手前決定が新たに現れたときのみ再オープンする）。残るのは （1） 実装中に決める事項、（2） 意図的に先送りした機能とその再検討トリガ、（3） 設計では解けない既知の限界、（4） 非技術の着手条件である。

### §29.1 実装中に決める事項

設計の正しさには影響せず、実装・運用しながら詰める事項である。いずれもコア型に波及しない。

| 項目 | 決める内容 | 関連 |
|---|---|---|
| **多相 Task の「相の途中 death」の人手ゲート** | 各相の入口 Observe（§6）は相境界の death は守るが、Applying 途中（書き込み中）の death を「リブート中」と bricked で区別できない。Applying を「危険区間」としてマークし、区間内 death は Observe で Done 確認できない限り **auto 再開を禁止し人手ゲート**にする、その具体判定 | §6 |
| **reimage の delete/create ペアリング判定** | `planned_change` の `kind: reimage` 宣言下で、再イメージが「delete 誤計上」でなく正当な delete→create の対であることを機械的に確認する判定基準 | §13 |
| **break-glass の到達不能判定基準・合流競合手順** | 緊急ローカル書込経路（§25）を発動してよい「git 到達不能」の判定基準と、復旧後に本流と事後合流するときの競合解消手順 | §25 |
| **`planned_change` の kind 整合検証** | 宣言 kind（reimage / decommission 等）と実際に生じた Plan の操作種別が一致することの検証ルール | §13 |
| **List 急減閾値のチューニング** | scope 単位で宣言する件数急減 halt の閾値の実運用値（誤発火と見逃しの均衡） | §12 |
| **Ansible executor の Observe 頻度** | fork + SSH の高レイテンシ層で tier 別間引き（§16）をどう既定するか | §16 |
| **運用パラメータ** | Sync Mode の commit 主体（bot / 署名有無）、scope 昇格の「N 日間 diff ゼロ」の N、reconcile 間隔既定値、freeze の粒度 | §14/§24 |

### §29.2 意図的な先送りと再検討トリガ

一部の機能は**意図的に先送り**している。先送り自体は正しい判断だが、**いつ見直すか**を決めておかないと、放置されるか、逆に早すぎる時期に蒸し返される。したがって「先送りを決めたら、同時に**再検討トリガ**を決めておく」ことをメタ決定として固定し、主要な先送りにトリガを紐づける。

| 先送り項目 | 再検討トリガ | 関連 |
|---|---|---|
| 抽象リソース（capability への依存） | **2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できず、早すぎる抽象化は最大公約数を誤る | §28.8 |
| CUE への `Loader` 移行 | テンプレート側にロジックが溜まり始めたとき（カスタム関数の増加） | §21.3 |
| シャーディング / 分散 Executor | 単一デーモンの天井（§16 のレイテンシ算術）に近づく**早期警戒線**に触れたとき | §17, §18 |

### §29.3 既知の限界（設計では解けないもの）

検証を経て確定した、**設計で埋められない**限界である。これらは設計の穴ではなく、対象側の制約・別事業・責務外に純化している（第I部の境界宣言と一致する）。

- **（a） 管理対象の API が貧しい環境は観測止まりが上限**。Observe すら不可能な旧機器・ベンダーロックの箱（SOHO 級ネットワーク / ストレージ機器等）は、台帳上 `unknown` のまま第一級で扱い（`ledger_only` / `observability:none`・§24）、`external_owner` で縁は切れるが、**収束（Apply）の品質は上がらない**。可視化・drift 検知までが到達上限として確定する。
- **（b） 上位テナント基盤（quota / 課金 / 認可 / ポータル）は Sakutei と同規模の別事業**。委譲は工数を消さない——Sakutei は `DesiredSource` の一実装として認可・quota 適用済みの理想状態を受け取るだけであり、パブリック IaaS 級の構成で残る差分は、この委譲先の実装工数であって Sakutei 側の技術ギャップではない（§22）。
- **（c） ゼロタッチ物理・provisioner 実体は責務外**。電源も入っていない物理の「段0 の底」を人手・OOB なしに立ち上げる能力は持たない。ブートストラップは人手（Ansible + 金庫）から始まる（§23）——これは妥協ではなく定石であり、自己を立ち上げるものは自分の外側から来るしかない。provisioner（起動方式と OS 種別を分離したベアメタル導入基盤）の実体構築も責務外である。
- **（d） AWS 等パブリッククラウドは技術的に可能だが対 Terraform の経済判断**。バルク API（Describe*）を持つため BulkObserve 経路に素直に乗り、技術的には収束できる。しかしその領域は Terraform のエコシステムが厚く、**採否は技術ではなく経済（既存 provider 資産との費用対効果）で決まる**。Sakutei の主戦場はオンプレであり、パブリッククラウドは適用可能だが主対象ではない。

これらの限界は、20 構成の適用可能性検討で到達不能として残った領域がいずれも上記に純化したこと——**設計で解ける課題はすべて解けた（残るは実装順序と対象側の制約のみ）**という収束状態——を根拠とする。

### §29.4 非技術の着手条件

- **雇用主の IP / OSS 公開規程の確認**。実務知見が設計に入っている以上、**職務発明・OSS 公開に関する所属組織の規程確認**は、コードを書き始める前に済ませておく（後から覆ると成果が無になる種類のリスク）。**自組織 社内利用が主目的、OSS 化はボーナス**という位置づけを前提とする。
- **本書と参照実装文書の分割**（境界は第I部で定義済み）。参照実装の内容を足す時点で境界が崩れるため、分割は早いほど安い。
- **プロジェクト名の衝突確認**。コンポーネント名を機能名に改めたため、確認対象は固有名詞 `Sakutei` のみである。

### §29.5 着手前決定を再オープンさせない運用

コア型・着手前決定の索引（§4 のコア型正準形・§5 の Provider interface 正準形・`rendered_revision` 算出定義・S2/S4 永続化先・`ready_condition` 名前空間）は**すべて確定済み**である。実装の第0スプリントはこの索引を参照して型を書き始めればよく、着手前決定として残る未決は無い。**新たに「後から変えると全プロバイダに波及する」種類の決定が現れたときにのみ、この索引を再オープンする**——それ以外の実装中の判断（§29.1）は索引を再オープンさせない。この規律が、パッチ的改訂で見通しを崩さないための運用境界である。


