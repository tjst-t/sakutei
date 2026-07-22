# 対象構成06: マルチサイト企業インフラ(3DC + DR)への Sakutei 適用性検討

**総合判定:△ 大幅な拡張が必要**(サイト=シャードと段0マルチサイト化を v2 以降に前倒し実装しない限り、DR 要件と分断耐性の中核が現行仕様では埋まらない)

---

## 1. 構成の前提整理

- **サイト**:東京(主・Proxmox 20)、大阪(副・12・DR 兼用)、福岡(小・4)。VM 約600、NW 機器 各サイト10-20台。運用は東京集中・各サイト無人。
- **サイト間**:MPLS + インターネット VPN バックアップ。レイテンシ 10-30ms、**年数回の分断実績**。
- **グローバルサービス**:AD(サイト間レプリケーション)、DNS(各サイト権威+キャッシュ)、Zabbix 分散監視。
- **DR**:東京全損時に大阪で基幹系を4時間以内復旧。VM レプリケーションは PBS + ZFS send。
- **決定的な構造欠陥**:git が東京 DC 内の GitHub Enterprise。**東京全損で intent(理想状態の正)ごと失われる**。これは §19.4「git は稼働必須依存にしない・各ノードにローカルミラー」の前提を破る。

この構成は Sakutei が §12.2 で「地理分散 = サイトごとシャード + 薄いルーター」と名指しした領域だが、当該機能は **v2 以降(§24.2)**。年数回の分断が「早期警戒線」ではなく常態であり、DR は §18 が「自動化は最後発・手動 runbook で長く足りる」と明言した領域。つまり本構成は Sakutei の設計が「先送りした部分」に正面から当たる。

## 2. リソースモデリング

3プリミティブへの写像は素直に効く。「産む/焼く/join する瞬間 = 手続き、前提と産まれた後 = 収束」(§20.2)がそのまま当てはまる。

| 対象 | 写像 | 根拠 |
|---|---|---|
| VM 600台・電源・スペック | Resource(managed fields + pruneable) | §20.2 |
| DNS 権威/キャッシュ(各サイト) | Resource + `tier: foundation` | §20.4 |
| NW/FW ルール(順序付き) | Resource(ordered + commit-confirm) | §8.5, §11.2-5 |
| PBS/ZFS レプリ**構成** | Resource | §18 |
| レプリ**結果**(鮮度・復元性) | health + canary(収束外) | §18 |
| クラスタ join / ホスト増設 | Workflow(invariant = quorum) | §20.2 |
| firmware / ローリング | 多相 Task / Workflow | §9 |
| **DR フェイルオーバー** | Workflow(invariant 付き) | §18 |

**必要 Provider 一覧**

| Provider | 階級 | List(prune/import) | 備考 |
|---|---|---|---|
| proxmox_vm | 完全 | ○ | 例文書に実装あり。vmid は allocator 払い出し |
| proxmox_cluster / node | 完全 | ○ | join は Workflow |
| pbs_backup / zfs_replication | 標準 | △ | 結果健全性は canary 側 |
| dns(権威/キャッシュ) | 完全 | ○ | zone 単位 closed-world |
| nw_switch(NETCONF/gNMI) | 完全(CommitConfirm 必須) | ○ | 管理断ロックアウト対策 |
| fw / lb | 標準〜完全 | ○ | 順序付きリスト |
| active_directory | **最小(Ansible)** | ✕ | 後述の最大ギャップ。TrueObserve/List とも困難 |
| zabbix(監視設定) | 標準 | ○ | observability as Resource(§17.2) |
| bmc(Redfish) | 標準 | ○ | 電源は managed 境界注意 |

**DR フェイルオーバーの Workflow(不変条件つき)例**:

```yaml
workflow: dr-failover-osaka
target: { site: osaka, tier: core-business }
invariant:
  primary_confirmed_lost: true       # ★東京の"生存"を誤検知した split-brain を禁じる最重要 invariant
  replica_freshness: { max_lag: 15m } # ZFS send の追い付き。古い複製で上げない
  quorum_at_osaka: maintained
strategy: { order: dependency-first } # DNS/AD → ストレージ昇格 → VM 起動 → NW 切替
steps:
  - assert: { tokyo_reachable: false, via: [mpls, vpn, oob] } # 3経路全滅を確認
  - promote_replica: { from: pbs+zfs, site: osaka }
  - start_vms: { group: core-business, source: replicated }
  - repoint_dns: { authoritative: osaka }
  - verify: { invariant: replica_freshness }
on_failure: { policy: halt, notify: page }
```

**設計上の急所**:`primary_confirmed_lost` の判定を Sakutei は自前で持てない(§17.1 health は監視委譲、分断とサイト全損の区別は Sakutei の観測 state レス設計では原理的に付かない — `unknown ≠ absent` が「東京は unknown、absent と断じない」を強制するため)。したがって DR 起動は **人間の明示トリガ(§18 の手動 runbook)**に留めるのが現行仕様の正しい姿であり、`primary_confirmed_lost` は人が承認ゲート(§10.5)で立てる入力になる。4時間 RTO のうち「人が招集され判断する時間」がクリティカルパスに残る。

## 3. ownership・DesiredSource・sharding 設計

**サイト = シャード**は §12.2 の推奨分割軸そのもの。各サイトに shared-nothing の reconciler を1つ置き、独立収束する(デーモン間協調不要 = 分散合意を中枢に戻さない)。

- **shard router**:「どのリソースがどのシャードか」を引く薄い層。割当表は **S2(§6.5)= 外部永続化必須**。router 自体が東京単独だと SPOF なので、割当表は各サイトにミラーする必要がある(仕様外の拡張)。
- **ownership**:VM/テナント = tenant 層(即時・DBSource)。クラスタ/コア NW/共有ストレージ = platform 層(PR 必須・FileSource=git)。**共有基盤は platform シャードが1つ所有・変更、テナントシャードは read-only 参照**(§12.2)。
- **サイト間依存の eventual consistency**:AD レプリケーション・DNS・PBS 転送は「シャードをまたぐ依存 = 観測 ready 待ち(結果整合)」(§12.2)。強い順序保証はシャード内に閉じる。大阪シャードは東京の AD/DNS readiness を **Observe で gate** する。ただし **AD のマルチマスタ・レプリケーション自体の収束は Sakutei の管轄外**(値・レプリ状態を収束できない。§20.1 の DB 行データと同型)。Sakutei が持てるのは「AD サービスが存在し起動している」まで。

## 4. render(inventory/allocation/secret)の使い方 — サイト横断採番

- **allocation(§16)**:IP/VLAN/ASN/vmid をサイトを跨いで一意採番する必要がある(DR で大阪へ VM が移動しても衝突しないよう、あるいはサイト固有プールを明確に分離する)。allocator は **単一権威**が原則(§16.2「並行/シャード環境でも競合しない」)だが、それを東京に置くと**分断中に新規払い出しが全サイトで止まる**。§7.3 の「authority 不達 → 前回割当保持・新規は保留 → blocked で次周回収」で加算は止まるが**破壊はしない**ため安全側には倒れる。ただし DR 時に新規リソースを作れない制約が残る。
- **サイト別プール分割**が現実解:VLAN/IP をサイト単位でシャードし、各サイトに allocator インスタンスを置く(SQLite + アトミック払い出し・§16.5 の最小実装で足りる)。サイト跨ぎのグローバル空間(ASN・ループバック)のみ東京 allocator が持ち、分断中は reserve 不可を許容。
- **secret(§14)**:各サイトの Vault が root。DR では大阪 Vault が sealed-restart する可能性があり、**unseal は人手(§19.4)**。SecretRef 世代キャッシュ(§7.3)で分断中も稼働継続。

## 5. 安全機構の適用 — 分断時の挙動(最重要)

これが本構成の核心。年数回の分断で、**分断されたサイトの reconciler が何をすべきか**を §12.3(分散 Executor 時の段階縮退)が直接規定している。ただし §12.3 は「将来設計」であり、**現行 v1/v2 スコープには入っていない点が最大の懸念**。

段階縮退(§12.3・非対称原則 §2-3 の分散版):

| 状態 | reconciler の振る舞い |
|---|---|
| 通常(制御面と疎通) | フル収束 |
| **制御面(git/router)と分断** | **加算のみ**(作る・直す・起動)。破壊的操作(削除・停止)は**保留** |
| lease 失効 | Observe のみ(自律収束停止) |

**「確信が持てないとき減算しない」の具体化**(本構成での実体):

1. **東京全損を大阪 reconciler が誤検知しないこと**:大阪から東京の VM/クラスタが `unknown`(到達不能)になっても、`unknown ≠ absent`(§4.2, §5.3)により「消えた」と断じない。→ **大阪が東京 VM の複製を勝手に本番昇格させない**(split-brain 防止)。DR 昇格は人の明示トリガのみ。
2. **stale な intent で削除しない**:git が東京にあり分断されると、大阪のローカルミラー intent が陳腐化する。`revision` + `leaseUntil`(§12.3)で「古い理想で減算」を禁止。「東京で削除された VM を、古い intent を持つ大阪が復活させ続ける」逆事故も同機構で防ぐ。
3. **allocation の Release 保留**(§16.4):分断で owner が消えたと誤認 → VLAN 再払い出し → 衝突、を回避。下流削除の Observe 確認後のみ Release。
4. **rate limit + halt**(§10.2):分断復帰時に大量 diff が噴くため、`on_exceed: halt-destructive-only` を platform 層に厳格適用。復帰直後の「Diff 突然巨大化」を異常検知(§10.2)で止める。

## 6. 段0ブートストラップの当てはめ

- **各サイトに段0を置くか、東京集中か**:東京集中は**東京全損 = 収束エンジン全損**を意味し、DR が成立しない。§18 が「DR 自動化は制御面のマルチサイトが前提」と明言。→ **少なくとも大阪に独立段0(Proxmox 3ノード + Ceph + VM-HA + Vault + broker + reconciler)を置く**必要がある。福岡は小規模なので大阪シャードが遠隔管理してもよい(reconciler は東京/大阪、福岡は RemoteExecutor 対象 — ただし §4.4 の分散 Executor は将来)。
- **DR 時の収束エンジン自体の可用性**:東京段0全損時、大阪段0が生きていれば大阪シャードは自律収束を継続(段0 は shared-nothing・§12.2)。ただし **東京 platform シャードが所有していた共有基盤 intent** をどう大阪が引き継ぐかが未解決 — 割当表・platform 理想状態のクロスサイト複製が要る。
- **git 問題の解決**:GitHub Enterprise を東京単独に置く現状は §19.4 違反。**git ミラーを大阪/福岡にも持つ**か、外部(GitHub.com 等 DC 外)に intent の正を移す。各 reconciler はローカルミラーで全断耐性(§19.4)を得るが、**書き込み(PR マージ)経路が東京全損で失われる** — DR 中の intent 変更手段を runbook で確保する必要がある。
- **Vault の sealed-on-restart**(§19.4):DR で大阪 Vault を立ち上げ直すと unseal に人手が要り、MTTR に人間の unseal 時間が乗る。4時間 RTO の見積もりに含めること。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **分散 Executor / サイトシャーディング / 分断段階縮退がすべて「将来設計」**(§12.3・§24.2)。本構成の中核要件(分断耐性・DR)が依存する機構が **v1 にも v2 の実装優先項目にも入っていない**。設計思想(§12.3 の縮退表)は書かれているが、`RemoteExecutor`・lease・revision・クロスサイト割当表複製は未実装かつ「税金の塊」(§4.4)。**これは適用の可否を左右する最大のギャップ**。
2. **AD のサイト間レプリケーションは収束の管轄外**。マルチマスタ・レプリケーション状態・競合解決は Sakutei が扱えない(§20.1 の行データ・§11.2-1 の二重ループと同型)。Sakutei は「AD サービスが起動している」までしか保証できず、**グローバルサービスの整合性という本構成の主目的の一部を担えない**。
3. **DR フェイルオーバーの自動判定ができない**。観測 state レス設計が `unknown ≠ absent` を強制するため、「分断」と「東京全損」を機械判別できない。§18 通り**人間トリガの手動 runbook 前提**となり、4時間 RTO に人の招集・判断時間が構造的に残る。これは欠陥ではなく設計の帰結だが、「自動 DR」を期待すると成り立たない。
4. **shard router / platform 理想状態 / 割当表(すべて S2)のクロスサイト可用性**が未設計。東京集中だと SPOF、複製すると「割当表の合意」が必要になり、§12.2 が避けたはずの分散合意が裏口から要求される懸念。
5. **福岡4ノードに段0(3ノード + Ceph)は重すぎる**。段0 最小化原則(§19.1)と衝突。遠隔管理(分散 Executor)が要るが将来機能。
6. **年数回の分断は「早期警戒線」ではなく常態**。§6.2 の割り切り(分断は稀な将来事象)が本構成では成立せず、分断対応が v1 の必須要件に繰り上がる。

**必要な拡張(実装前倒し)**:サイトシャーディング + shard router(割当表クロスサイト複製込み)/ 分断段階縮退(lease・revision)/ サイト別 allocator + グローバルプール分離 / platform 理想状態のクロスサイト複製 / git ミラーの多拠点化。いずれも §12.2/§12.3 に設計思想はあるが**実装が丸ごと残っている**。

## 8. 総合判定

**△ 大幅な拡張が必要**

- **理由**:モデリング(3プリミティブ写像・Provider 群・DR Workflow の invariant 表現)と安全機構の思想(`unknown ≠ absent`・段階縮退・rate limit halt・allocation Release 保留)は本構成の要求に**驚くほど正確に対応している** — split-brain 防止と分断時の破壊保留は設計原則から自然に導ける。しかし、それらを支える **サイトシャーディング・分散 Executor・分断段階縮退・クロスサイト S2 複製が実装として存在せず(§12.3 は「将来設計」、§24.2 で v2 以降)**、DR とグローバルサービス整合という中核要件がまだ埋まらない。AD レプリケーション整合は原理的に管轄外。
- **○(条件付き成立)に上げる条件**:(a) 大阪に独立段0を構築し reconciler をマルチサイト化、(b) git の正を DC 外化 or 多拠点ミラー化、(c) サイト別 allocator + グローバルプール分離、(d) DR を人間トリガの手動 runbook(承認ゲート付き Workflow)に限定、(e) AD/DNS のレプリ整合は Sakutei 外の既存機構に委ね「サービス存在」までを収束対象とする。この5点を満たせば、まず**各サイト内の VM/NW/DNS/監視設定の drift 自動修復と可視化**という Sakutei 本来の価値は堅く出る(§23.7 の導入順序に沿い、DR 自動化は最後発)。
- **◎ に至らない構造的理由**:分断が常態でグローバル整合を要する本構成は、Sakutei が「観測 state レス・shared-nothing・分散合意を中枢に戻さない」という設計上あえて**避けた領域**に正面から当たるため、思想を曲げずに完全自動化することはできない。
