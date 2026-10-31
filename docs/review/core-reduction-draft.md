# Sakutei のコアを絞り直す案（下書き・未決定）

2026-10-31。v2.12.0（main d28dc4a）を見直した結果と、ゼロから組み直すときのコアの案。まだ決まっていない。

## 1. いまの姿（数えたもの）

| もの | 量 |
|---|---|
| Go のコード（テストを除く） | 約 10.8 万行、パッケージ 70 |
| テスト | 約 10 万行 |
| 起動の旗 | 126（provider ごとの旗 `--zone-*`・`--ruleset-*` などがコアの CLI に入っている） |
| provider の任意の口（pkg/provider） | 15 ほど（Actioner・Approval・CheckReader・CommitGroup・Confirm・PlanRefiner・ExternalOwner・AbsentValidator・IntentValidator・UpdatePolicyValidator・BulkObserver …） |
| 収束ループ（internal/reconcile） | 9,500 行、うち loop.go 1 本で 3,900 行 |
| 設計書 | 4,500 行、§1〜§29、改訂 77 回 |
| 組み込みの provider | dnszone・nftables・redfish（3種）・proxmox（vm・fw）・netconf・postgresql・s3・ldap・sqlmigrate・ansible・hostconf・cli-device・metrics-probe・guestos ほか |

## 2. 見立て

### 本当に要るもの（芯）
1. **毎周見て直す（level-triggered）、観測を記録しない**。
2. **確かでないときは消さない**（unknown ≠ absent、List が完全なときだけ prune、急減で止まる）。
3. **壊す操作は数えて止め、人に承認させる**。
4. **「いつ」は収束、「どう」は手続き**（一回きりの危険な手順は Task に閉じ込める）。
5. **正しい状態は宣言に置く**。

この5つは今も正しく、新しいコアでもそのまま土台にする。

### 複雑さの出どころ
- **想定した規模と実際の使い方がずれている**。設計は「1万件・複数拠点・テナントと基盤の2層・シャード・災害切り替え」を前提にした。実際の使い方は「VM 1台で1つの基盤」。claim とシャード、fencing、git のミラー、S2-free、DR モード、観測の間引き（tier）、BulkObserve、容量の門、振動の検知、原因タグ A〜E、運用プロファイルは、いまの使い方では効いていない。
- **同じ考えが別々の機能として何度も入った**。
  - 「止める・緩める」系：freeze、remediation mode と schedule、planned_change、max-* の予算の旗、approve-large-change、プロファイル、runtime の締める向きと緩める向き。どれも「この範囲で、いつ、どこまで変えてよいか」という1つの方針で言える。
  - 「観測で条件を待つ」系：ready_condition、require、done_when、apply_check、confirm の observe:／check:、cloud_init_done。どれも「観測した値についての条件」1つで言える。
  - 「比べない欄」系：managed fields、宣言に無い属性を比べない（#208）、provider ごとの無視の設定。「宣言に書いた欄だけを比べる」の1つの規則で足りる。
  - 「2段で当てて確かめる」系：netconf の commit confirmed、nftables の到達の確かめと時間切れの戻し、commit_group。どれも「準備 → 確かめ → 確定、だめなら戻す」1つの口で言える。
- **設定の置き場所が3つある**（起動の旗・intent・provider の設定）。食い違いの規則を作り続けることになった。
- **provider がコアの中にいる**。provider の都合がコアの CLI・loader・registry に漏れている（旗、intent の範囲の木、provider 名で分かれる処理）。
- **要望に早く応えたが、実際に使われたかを確かめずに積んだ**。

## 3. 新しいコアの案（8つの部品）

組み合わせれば今できることを全部言えて、それぞれは小さい部品にする。

1. **Resource**：`(type, id)` と欄の map。比べるのは**宣言に書いた欄だけ**。
2. **Provider**：必須は4つ——`Observe(id) → present | absent | unknown と欄`、`Apply(id, 望む欄)`、`Delete(id)`、`List(scope) → ID の集合と「完全か」`。Schema は欄の型と「この欄が変わると壊す変更か」だけ。任意は1つ——**Transaction**（Prepare → Verify → Commit／Rollback。commit confirmed・時間切れの戻し・複数台をまとめた確定を全部これで書く）。
3. **Condition**：観測した値についての条件の式1つ。依存の待ち・ready・done_when・apply の後の確かめ・confirm の確かめに同じ式を使う。
4. **Graph**：`depends_on` と Condition。順序と待ちはこれだけ。
5. **Policy**：`scope の選び方 × mode（apply | report）× 時間の窓 × 予算（周ごと・窓ごと、壊す操作は重く数える）× 承認`。freeze は予算 0、alert-only は mode report、planned_change は一時的な Policy。旗や runtime の締め緩めの規則は全部ここに入れる。
6. **Procedure**：一回きりの手順。前の条件（Condition）、動作、done_when（Condition）、承認、時間の上限。Workflow は Procedure の Graph。strategy: task／manual もこれ（manual は「人がやって、done_when で閉じる」）。
7. **Expand**：intent から Resource の一覧を作る純粋な関数の口（Go か外のコマンド）。合成リソース・テンプレート・網の許可の変換（netpolicy）・採番の値の差し込みはここに入れる。コアは変換の中身を知らない。
8. **Record と Events**：持つ状態は3つだけ（進行中の Procedure、承認と監査の鎖、採番の結果〈正は intent のファイル、コアは空きを選んで書き足すだけ〉）。ファイルと flock を使う。外への出口は level の Event の流れと sink の口1つ、加えて読み取りの HTTP（status・plan・events）。Alertmanager・syslog・OTLP・metrics は sink の実装でコアの外に置く。

設定の置き場所は2つにする：intent（何を・範囲・方針）と、つなぎ方の設定ファイル1つ（宛先・鍵・待ち受け）。起動の旗はファイルの場所と一回性の動作だけにする。

## 4. コアの外に出すもの・やめるもの

| 扱い | 対象 |
|---|---|
| 別のモジュールに出す（コアの SDK だけに依存） | 全部の provider、netpolicy の変換、sink（Alertmanager・syslog・OTLP・metrics）、import、sakutei-nft-guard |
| やめる（いまの使い方では効かない。必要になったら Expand・Policy・Record の上に作り直す） | claim とシャード、fencing、git のミラー（gitdist）、S2-free、DR モード、観測の tier、BulkObserve、容量の門、振動の検知、原因タグ A〜E と last-seen、運用プロファイル、採番の authority・キャッシュ・TTL・Reserve/Confirm、runtime の役ごとの引数と締め緩めの規則 |
| 1つにまとめる | §2 の「同じ考えが何度も入った」ものを Policy・Condition・Transaction・「宣言した欄だけ比べる」に置き換える |

## 5. 進め方の案
- **コアはゼロから書き、provider は移植する**。provider には機器ごとの癖の知見（Redfish・Proxmox・NETCONF・nftables）が入っていて、ここは捨てるのが惜しい。コア（reconcile・loader・cli・budget・task・workflow・registry）は新しく書く。
- v2 系は直しだけに止め（機能は足さない）、新しいコアが使う側の宣言を全部扱えるまで並べて保つ。intent の書き換えは変換の道具で支える。
- 強さの確かめ方：コアは偽の provider で性質のテスト（どんな観測の並びでも unknown で消さない、予算を超えない、承認なしで壊さない）と、故障の注入を主にする。統合試験は provider の側で回す。

## 6. 決めること
1. ゼロから組み直す（コアを新しく書き、provider を移植）か、いまの v2 の中で段々に削るか。
2. 使う側との互換：v2 を直しだけで保ち、変換の道具を出すか。
3. 使う側が実際に使っている provider・欄・旗を調べてもらうか（何を最初に移すかの決め手になる）。
