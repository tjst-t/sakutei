# 採番（ファイル版 AllocationAuthority・pools・HA 最小形）を手で試す

採番（allocator、設計 §19・§27.3）を、実際の CLI（`plan`・`reconcile`・`status`）・常駐デーモン・`sakutei allocator`（HA 最小形）で回して確かめる手順と、
2026-10-23 にこのコンテナで回した結果。相手の実 provider は dnszone（ゾーンファイル）。外のサービスには繋がない（allocator は 127.0.0.1 で動かす）。

## まず知っておくこと

- **v1 のプールは整数だけ**（§19.8）。IP アドレスのプールは無い。だからこの手順は、サービスのポート番号をプールから払い出して
  dnszone の **TXT レコード**（`_port.<名前>.example.com.`）に入れる。A レコードに入れると何が起きるかは手順 6 で見る。
- 要求は `fields` の値に `from_pool: <プール名>` と書き、プールはトップレベルの `pools:` で宣言する（§19.1・§19.8）。YAML はブロックで書く。
- owner は `"<provider>/<local id>#<field>"`（scope を含めない、§19.3）。採番記録（`--alloc-store` のファイル）は JSON で、そのまま読める。
- `plan`・`status` は `Lookup` だけ（払い出さない・採番記録に書かない）、`reconcile` と常駐ループは `Allocate` する（§19.3）。

## 確かめる観点（設計書と受け入れテストから挙げたもの）

| # | 観点 | 設計 | 手順 |
|---|---|---|---|
| 1 | plan は払い出さず、採番記録も作らない。未割当は「割り当て待ち」と出る | §19.3 | 1 |
| 2 | reconcile がプールを宣言し、小さい順に払い出し、値がリソースのフィールドに入る | §19.3・§19.9 | 2 |
| 3 | 1 回収束を繰り返しても・デーモンを止めて起こし直しても、同じ owner に同じ値（記録は変わらない）。ずれは同じ値に戻る | §19.1・§19.6 | 3 |
| 4 | 宣言から外しても、現実から消えても、Release しない（v1 は Release の経路を作っていない＝保守側）。宣言し直すと同じ値 | §19.3・§19.5 | 4 |
| 5 | プールが尽きると、そのリソースだけ waiting-allocation。ほかは収束する。範囲を広げれば解ける。使用中の値を外す縮小は拒否 | §19.3・§19.9 | 5 |
| 6 | 整数のプールを A レコードに入れたとき | §19.8（整数のみ） | 6 |
| 7 | 同じホストの複数のプロセスが同じ採番記録から同時に払い出しても重ならない（flock） | §19.9 | 7 |
| 8 | HA 最小形：active を落としても既存のずれは直り続け（Lookup キャッシュ）、新しい要求だけ waiting-allocation。standby の昇格で解ける。古い active は fence される。直に開く `--alloc-store` も fence される | §19.6・§19.8・§19.9 | 8 |
| 9 | 採番記録を失ったとき（§19.7 の再構築手順） | §19.7 | 9 |
| 10 | プール分割 by claim：シャードごとの採番記録から disjoint なレンジで払い出す。重なる宣言はロードで拒否 | §19.8 | 10 |

## 置いてあるもの（docs/trial/allocation/）

| ファイル | 中身 |
|---|---|
| `initial/example.com.zone` | 最初の現実。SOA・NS・`ns1` だけのゾーン |
| `intent/main.yaml` | プール `svc-ports`（8000-8002）と、そこから払い出す `_port.app1`・`_port.app2`（TXT）、固定値の `www`（A） |
| `run.sh` | 手順 1〜10 を通して流すスクリプト。手順 5 以降の intent はスクリプトの中で書き足す・作る |

## 通しで流す

```sh
docs/trial/allocation/run.sh            # 作業ディレクトリは /tmp/sakutei-allocation（毎回作り直す）
```

Go と curl が要る。127.0.0.1 の 17071・17072 番で allocator を 2 つ動かす（`PORT_A`・`PORT_B` で変えられる）。リポジトリのファイルは書き換えない。
通しで 40 秒ほど。

## 1 つずつ流す

`run.sh` と同じことを手で行う。作業ディレクトリで：

```sh
W=/tmp/sakutei-allocation; rm -rf $W; mkdir -p $W
cp docs/trial/allocation/initial/example.com.zone $W/; cp -r docs/trial/allocation/intent $W/
go build -o $W/sakutei ./cmd/sakutei
cd $W
ONE="--intent intent --zone example.com.zone --alloc-store alloc.json"
./sakutei plan $ONE          # 手順 1
./sakutei reconcile $ONE     # 手順 2
cat alloc.json               # 採番記録
```

HA（手順 8）は、同じ採番記録を指して 2 つ起動し、デーモンに両方を並べる：

```sh
./sakutei allocator --store ha.json --listen 127.0.0.1:17071 &            # 起動時に昇格（世代 1）
./sakutei allocator --store ha.json --listen 127.0.0.1:17072 --standby &  # POST /v1/promote まで 503
./sakutei --intent intent --zone example.com.zone --alloc-url http://127.0.0.1:17071,http://127.0.0.1:17072 --interval 1s
curl -s http://127.0.0.1:17072/v1/health                                   # {"role":"standby"}
curl -s -X POST http://127.0.0.1:17072/v1/promote                          # 手で昇格（VM-HA の代わり）
```

## 回した結果（2026-10-23、このコンテナ）

`run.sh` の出力の抜粋。全文は `run.sh` を流せば同じものが出る（時刻と revision のハッシュ以外）。

### 1. plan（払い出さない）

```
sakutei plan: pending allocation: dnszone//_port.app1.example.com./TXT field "value" from pool "svc-ports" (allocated by reconcile)
sakutei plan: pending allocation: dnszone//_port.app2.example.com./TXT field "value" from pool "svc-ports" (allocated by reconcile)
Plan: 3 change(s)
+ CREATE dnszone//_port.app1.example.com./TXT
  + value                 (new) → "<pending allocation from pool svc-ports>"
…
→ plan のあとも採番記録（alloc.json）は無い
```

### 2. reconcile（払い出してフィールドに入れる）

```
+ CREATE dnszone//_port.app1.example.com./TXT
  + value                 (new) → "8000"
+ CREATE dnszone//_port.app2.example.com./TXT
  + value                 (new) → "8001"
Reconcile: 3 change(s) applied
_port.app1.example.com. 300 IN TXT 8000
_port.app2.example.com. 300 IN TXT 8001
  dnszone/_port.app1.example.com./TXT#value → 8000
  dnszone/_port.app2.example.com./TXT#value → 8001
```

`intent_revision` と `rendered_revision` が分かれる（割当は外部入力、§9.4）。

### 3. 起こし直しても同じ値

```
Converged (no changes)                                     ← もう一度 reconcile
→ ゾーンの _port.app1 を手で 9999 に書き換えた
_port.app1.example.com. 300 IN TXT 8000                    ← デーモンが同じ 8000 に戻した
--- 起こし直したデーモンのログ
sakutei: cycle 1: observed=3 converged (no changes)
sakutei: cycle 2: observed=3 converged (no changes)
sakutei: cycle 3: observed=3 converged (no changes)
→ 採番記録は 1 バイトも変わっていない（同じ owner に同じ値）
```

起こし直したデーモンは Lookup キャッシュ（メモリ）を失っているので、要求ごとに 1 回 `Allocate` し直すが、冪等なので同じ値が返る（§19.6）。

### 4. 宣言から外す → 解放しない（設計どおり）

```
→ intent から app2 を外した。既定（ExplicitAbsentOnly）なので現実のレコードも残る
→ 現実からも手で消した（List に現れない）。それでも採番記録の app2 はそのまま
  dnszone/_port.app1.example.com./TXT#value → 8000
  dnszone/_port.app2.example.com./TXT#value → 8001
+ CREATE dnszone//_port.app2.example.com./TXT
  + value                 (new) → "8001"               ← 宣言し直すと同じ 8001
```

§19.3「Release を呼ぶ経路はまだ作っていない」のとおり、v1 では割当は解放されない（保守側）。コードにも `Release` を呼ぶところは無いので、
prune（`deletion_policy: PruneWithinScope`）で現実から消しても同じはず（この手順では手で消した）。解放するには手順 8f の API を使う（「見つけたこと」の 5・6）。

### 5. プールが尽きる

```
sakutei reconcile: waiting-allocation: dnszone//_port.app4.example.com./TXT field "value" from pool "svc-ports": allocation: pool exhausted: "svc-ports" (not converged until allocated; others continue, §19.9)
sakutei: skip dnszone//_port.app4.example.com./TXT: unobservable this cycle (Observe error or Availability != Ok)
+ CREATE dnszone//_port.app3.example.com./TXT     value → "8002"
+ CREATE dnszone//api.example.com./A              value → "192.0.2.20"
Reconcile: 2 change(s) applied
Reconcile: 1 resource(s) waiting-allocation (retry once the allocator can allocate)
（終了コード 0）
--- plan / status での見え方
sakutei plan: pending allocation: dnszone//_port.app4.example.com./TXT field "value" from pool "svc-ports" (allocated by reconcile)
  DRIFTED dnszone//_port.app4.example.com./TXT
--- 範囲を 8000-8003 に広げる
+ CREATE dnszone//_port.app4.example.com./TXT     value → "8003"
--- 使用中の 8003 を外す縮小（8000-8002）は拒否され、その周は何も進まない
sakutei reconcile: render: declare pool "svc-ports": allocator: pool "svc-ports": new range 8000-8002 would exclude 8003, allocated to "dnszone/_port.app4.example.com./TXT#value"
  convergence not advanced (halt-safe, §9.2)
（終了コード 1）
```

尽きたリソースだけが waiting-allocation になり、エラーでも halt でもない（終了コード 0）。縮小の拒否は周期全体を止める（render の失敗、§9.2）。
宣言から外したが解放されていない値（手順 4 のような）も「使用中」なので、縮小を拒む。

### 6. 整数を A レコードに入れる

```
sakutei: apply dnszone//app5.example.com./A: permanent: dnszone: writeRecord: refusing to write app5.example.com./A: value "8004" is not an IPv4 address
Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)
（終了コード 0）
→ app5 の A は書かれていない（ゾーンはほかのレコードごと読み込める形のまま）
```

**直す前は `app5.example.com. 300 IN A 8004` をそのまま書いていた**（「見つけたこと」の 1）。dnszone 自身は読み戻せるが、DNS サーバはゾーン全体の
読み込みを拒む。

### 7. 4 つのプロセスが同時に払い出す（flock）

範囲 100-199（100 個）のプール `bulk` を、4 つの `reconcile`（それぞれ別の intent・別のゾーン、同じ `--alloc-store`）が 25 件ずつ同時に払い出す。

```
p1: Reconcile: 25 change(s) applied
…
→ 採番記録 100 件・重複 0 件・4 つのゾーンの TXT 100 件（100 の範囲を 100 件で使い切る）
--- プロセスごとの値（交互に払い出している = 同時に走った）
p1: 105 106 107 108 109 110 111 112 113 114 115 116 129 143 144 145 146 157 160 173 174 175 176 177 178
p2: 117 130 131 147 158 161 179 181 182 184 185 186 187 188 189 190 191 192 193 194 195 196 197 198 199
p3: 100 101 102 103 104 123 124 125 126 127 128 133 134 135 136 137 138 139 140 141 142 153 154 155 156
p4: 118 119 120 121 122 132 148 149 150 151 152 159 162 163 164 165 166 167 168 169 170 171 172 180 183
もう一度 p1: Converged (no changes)                 ← 4 つとも
sakutei reconcile: waiting-allocation: dnszone//_id.p5-r01.example.com./TXT … pool exhausted: "bulk"   ← 5 つめは尽きて待つ
```

値は交互に入っている（4 つのプロセスが本当に同時に払い出した）が、重複は 0。ロードと書き戻しを flock の下で行っているため（ロックを外すと
重複することは受け入れテスト `TestAllocator_ConcurrentAllocateNoDuplicates` が確かめている）。

### 8. HA 最小形

```
sakutei allocator: active (generation 1) listening on http://127.0.0.1:17071
sakutei allocator: standby listening on http://127.0.0.1:17072
  A: {"role":"active","generation":1}  B: {"role":"standby"}
_port.app1.example.com. 300 IN TXT 9000
--- 8a. active の A を SIGKILL。既存のずれは直り続け、新しい要求だけ waiting-allocation
→ allocator が落ちていても app1 のずれは Lookup キャッシュで直った
sakutei: waiting-allocation: dnszone//_port.app2.example.com./TXT … authority unavailable: http://127.0.0.1:17072: allocator is standby …
--- 8b. standby の B を昇格（POST /v1/promote）→ app2 が払い出される
{"role":"active","generation":2}
_port.app2.example.com. 300 IN TXT 9001
--- 8c. 落ちた A を --standby なしで起こし直す → A が世代を上げて active、B は次の要求で fence されて降りる
sakutei allocator: active (generation 3) listening on http://127.0.0.1:17071
  A: {"role":"active","generation":3}  B: {"role":"standby","generation":2}
→ クライアントは B（fence された）から A へフェイルオーバーして払い出した
--- 8d. 使用中のポートで 3 つめを --standby なしで起動（打ち間違いの想定）
sakutei allocator: listen: listen tcp 127.0.0.1:17071: bind: address already in use
（終了コード 1）
  A: {"role":"active","generation":3}  B: {"role":"standby","generation":2}
_port.app4.example.com. 300 IN TXT 9003
→ app4 も払い出された（起動に失敗したインスタンスは昇格していない）
--- 8e. 採番ファイルを直に開く（--alloc-store）と、HA の世代に fence されて書けない（1 プール 1 権威）
sakutei reconcile: render: declare pool "ha-ports": allocation: authority fenced (a newer generation is active): holds generation 0, store is at 3
（終了コード 1）
--- 8f. 手で解放する（v1 で解放する唯一の手段: active への POST /v1/release）
  cooldown: 9002（2026-10-24T…Z まで）
_port.app5.example.com. 300 IN TXT 9004
→ 解放した 9002 は cooldown 中なので使われない
--- 8g. 解放した app3 を同じデーモンの下で宣言し直す
_port.app3.example.com. 300 IN TXT 9002                   ← 解放した値がそのまま使われ、
  dnszone/_port.app5.example.com./TXT#value → 9004        ← 採番記録に app3 は無い
  cooldown: 9002（…まで）
```

**直す前の 8c・8d**（「見つけたこと」の 2・3）：

```
--- 8c.
  A: {"role":"active","generation":3}  B: {"role":"active","generation":2}   ← 2 つとも active と答えた
--- 8d.
sakutei allocator: listen: listen tcp 127.0.0.1:17071: bind: address already in use
（終了コード 1）
→ app4 が払い出されない                                                       ← 失敗した 3 つめが世代 4 に昇格して A を fence していた
sakutei: cycle 7: 1 resource(s) skipped (retry next cycle): [_port.app4.example.com./TXT]
```

### 9. 採番記録を失う

```
_port.app1.example.com. 300 IN TXT 8000
_port.app2.example.com. 300 IN TXT 8001
_port.app3.example.com. 300 IN TXT 8002
_port.app4.example.com. 300 IN TXT 8003
→ 採番記録を消し、同時に intent から app1 を外した（現実の app1 = 8000 は残る）
~ UPDATE dnszone//_port.app2.example.com./TXT
  ~ value                 "8001" → "8000"
~ UPDATE dnszone//_port.app3.example.com./TXT
  ~ value                 "8002" → "8001"
~ UPDATE dnszone//_port.app4.example.com./TXT
  ~ value                 "8003" → "8002"
Reconcile: 3 change(s) applied
_port.app1.example.com. 300 IN TXT 8000
_port.app2.example.com. 300 IN TXT 8000
→ app2 が 8000 に付け替わり、現実に 8000 が 2 つある
```

§19.7 の「現実から逆引きして再構築する手順」は v1 に無い。記録を失うと、払い出しが小さい順にやり直され、使用中の値が付け替わる（「見つけたこと」の 6）。

### 10. プール分割 by claim

```
sakutei reconcile: --claim site=site-a: 1 resource(s) in claim, 1 excluded (out of scope)
sakutei reconcile: --claim site=site-b: 1 resource(s) in claim, 1 excluded (out of scope)
_port.a1.example.com. 300 IN TXT 8100
_port.b1.example.com. 300 IN TXT 8200
--- site-a の採番記録
  dnszone/_port.a1.example.com./TXT#value → 8100
→ site-a の記録に site-b のプールは無い
sakutei plan: load intent: loader: pools "site-a-ports" (8100-8199, claim map[site:site-a]) and "site-b-ports" (8150-8299, claim map[site:site-b]) overlap: pools split by claim must be disjoint (§19.8); …
```

2 つのシャードを同時に、別々の採番記録で回した。各シャードは自分の claim のプールだけを宣言・払い出す。

## 見つけたこと

### 直したもの（テスト付き）

1. **dnszone が A / AAAA にアドレスでない値を書いていた**（手順 6）。v1 のプールは整数だけなので、A の value を `from_pool` にすると
   `A 8004` がゾーンに入る。dnszone は読み戻せるが、DNS サーバはゾーン全体を読み込まない（同じゾーンのほかのレコードも答えなくなる）。
   Apply で A に IPv4 以外・AAAA に IPv6 以外の value を書かずに Permanent で失敗させ、そのリソースだけを止めるようにした（#37 の「読み戻せない行は
   書かない」と同じ形）。ロード時の検証（ValidateIntent）にはしなかった——割当のあとの値にもかかり、そこで落とすと周期全体が止まるため。
   受け入れテスト `tests/acceptance/dnszone_address_test.go`（`[AC-98-1]`・`[AC-98-2]`）。
2. **`--standby` なしの `sakutei allocator` が、listen の前に昇格していた**（手順 8d）。使用中のポートで起動を誤ると、listen に失敗して終わるのに
   世代だけ上げていて、動いている active が fence され、**active が 1 つも無くなった**（新しい払い出しがすべて止まる）。listen してから昇格するようにした。
   `tests/acceptance/allocator_trial_test.go`（`[AC-98-3]`）。
3. **fence された active が `GET /v1/health` で active と答えていた**（手順 8c）。fence を確かめるのが次の操作のときだけだったので、それまで
   2 つの active が見えた（実際に書けるのは新しいほうだけで、採番の衝突は起きない）。health でも世代を確かめ、fence されていたら standby に降りて
   答えるようにした。`[AC-98-4]`。

### 直していないもの（設計の判断が要る・大きい）

4. **IP アドレスのプールが無い**（手順 6）。v1 のプールは整数だけ（§19.8）なので、「プールから払い出したアドレスを A レコードに入れる」はできない。
   IP のプール（CIDR の範囲）か、整数を値に埋め込むテンプレート（例 `192.0.2.{n}`）が要る。どちらにするかは設計の判断。
5. **解放する手段が API だけ**（手順 4・8f）。Release の経路（§19.5「下流の削除を List で確かめてから」）が無いので、宣言から外した値は解放されない（設計どおり）。
   手で解放できるのは `sakutei allocator` の `POST /v1/release` だけで、`--alloc-store`（ファイル版を直に開く）の運用では JSON を手で編集するしかない。
   解放されない値はプールを埋め、範囲の縮小も拒む（手順 5）。CLI（例 `sakutei allocator release --store … --pool … --owner …`）か Release の経路が要る。
6. **手で解放した値を、動いているデーモンの Lookup キャッシュが使い続ける**（手順 8g）。キャッシュの無効化は「自分の Release のときだけ」（§19.6）なので、
   API で解放した owner を同じデーモンの下で宣言し直すと、キャッシュの古い値（9002）がそのまま使われ、採番記録には載らない。cooldown（24h）が
   明けると allocator は 9002 をほかの owner に払い出しうる＝**衝突**。いまの回避策は「手で解放したらデーモンを起こし直す」。desired から消えた owner の
   キャッシュを捨てる（宣言し直したら 1 回 `Allocate` し直す）などが考えられるが、§19.6 の決まりを変えるので判断が要る。
7. **採番記録を失うと、使用中の値が付け替わる**（手順 9）。§19.7 の「現実から逆引きして再構築する手順」は v1 に無い。いまの守りは、採番記録（S2）を
   バックアップすることだけ。
8. **Apply が Permanent で失敗しても、reconcile の要約が `Converged (no changes)`・終了コード 0**（手順 6）。失敗の行は出るが、要約は収束したと言う。
   Apply の失敗で終了コード 0 は既存の決まり（AC-S94f1f9-2-2）だが、要約の文言は誤解を招く。採番に限らない reconcile 全体の話。
   → #110 で直した：要約は失敗した件数を言う（`Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)`、
   ほかに適用した変更があれば `Reconcile: N change(s) applied; 1 resource(s) failed to apply`）。終了コード 0 は変えていない。
   `tests/acceptance/apply_failed_summary_test.go`（`[AC-110-1]`・`[AC-110-2]`）。
9. **waiting-allocation のリソースが「unobservable this cycle (Observe error or Availability != Ok)」とも出る**（手順 5・7）。観測はできていて、
   理由は割当待ち。直後の行に waiting-allocation と出るので害は小さいが、文言が誤っている。
10. **plan・status は、プールが尽きたことを言わない**（手順 5）。`Lookup` しかしないので「pending allocation … (allocated by reconcile)」と出るだけで、
    reconcile しても払い出されないことは reconcile の出力でしか分からない。
11. **plan・status に `--claim` が無い**（手順 10）。シャードごとに採番記録を分けた構成では、plan はほかのシャードの要求も「割り当て待ち」と出す。

### 気づいたこと（不具合ではない）

- **落ちた active を `--standby` なしで起こし直すと、いまの active から役を奪う**（手順 8c）。設計どおり（§19.8「`--standby` なしで起動したインスタンスは
  起動時に昇格する」）で、fence が働くので衝突は起きないが、VM-HA で起こし直すときは `--standby` を付けるのが穏やか。
- waiting-allocation の理由には、最後に試したエンドポイントの答え（`allocator is standby`）だけが出る。A が落ちていることは出ない。
- この手順の「HA」は同じホストの 1 つのファイルを 2 つのプロセスが指す形。本来の置き場（段 0 の VM-HA・Ceph など、§19.8）の上で flock が
  効くかは確かめていない（ネットワークファイルシステムの flock の振る舞いによる）。
