# CHANGELOG v3.55 → v3.56

**改訂日: 2026-10-28**

エンジンが現実に到達する資格を、観測用と書き込み用に分ける改訂である（Backlog #185。親のストーリー「M7：本番に向けた固め」の
「鍵を役目ごとに分ける」の前半）。後半（HTTP の管理 API の provider の資格の分け）は同じ口で次に行う。

## #185：観測用と書き込み用の資格を分ける共通の口と、ssh 越しの provider（§20.7 新設・§29.1）

- **背景**: provider の資格は 1 本で、観測（`plan`・`status`・読み取りの口・常駐の毎周の観測）と書き込み（`Apply`・`action`）が同じ
  資格で回っていた。最も多く・最も広く使われる観測の経路（非常駐のクライアント PC からの `status` を含む）に、最も強い書き込みの
  権限（ssh なら sudo で root）が常に乗る。書かない mode（`--mode alert-only`・`--freeze`）や `plan` でも、書き込みの鍵が store に
  無ければ起動できなかった。
- **決定**:
  - 共通の口（`pkg/provider`）は 2 つ。**書き込みの文脈**：コアは `Apply`・`Action` を `provider.WithWrite(ctx)` で呼び、provider は
    `provider.Writing(ctx)` が真の呼び出しにだけ書き込み用の資格を使う（自分の `Apply` の中の読み直しを含む）。ほかの呼び出し
    （`Read`・`List`・`BulkObserve`、Apply のあとの効いたかの読み直し §11.1 を含む）は観測用の資格だけを使う。書き込み用の資格は
    呼び出しの直前に取り出し、終われば手放す。**この回が書くか**：factory の設定のキー `access`（`provider.ConfigAccess`）。書かない
    回は CLI が `observe` を入れ、provider は書き込み用の資格を確かめず・取り出さず（store に無くてよい）、書き込みを
    `provider.ErrNoWriteAccess` で断る。
  - 書かない回は `plan`・`status`・`import`・`sync`・`graph` と、`--mode alert-only`・`--freeze` の `reconcile`・常駐（freeze は、一回性の `reconcile` ではその回に効いている freeze（`--freeze`・`--freeze-state`）、常駐では期限（`--freeze-until`）の無い `--freeze` だけ。期限つきの `--freeze` と `--freeze-state` の freeze は常駐の途中で解けるので、常駐は書く回）。remediation
    schedule で時間帯により alert-only になる構成は書く回（時間帯の外では書く）。書く回は、設定した書き込み用の資格が store に
    あることを読み込みで確かめ、無ければ起動しない（何もしない）。
  - `access` は命令と mode から決まる。`--provider-config` に書けば読み込みで断る（ファイルで「書かない」と書いても、本体は書く回と
    して動くので食い違う）。
  - ssh の対象の一覧のホストに `observe:`・`write:` を書ける（どちらも `user`・`private_key`・`sudo` だけ。書かないキーはホストの値を
    受け継ぐ。どちらも書かなければ今までどおり 1 本）。書き込み用の資格の ssh は接続を使い回さない（master を残さない。鍵の一時
    ファイルはコマンドごとに消す）。観測用は今までどおり使い回す。
  - 観測用のユーザーは sudo 無し、または観測のコマンドだけを許した sudo で動かす。nftables の live の観測は `nft -j list table <f> <t>`
    と `nft -a -s list table <f> <t>` だけなので sudoers で絞れる。dnszone・host-file・systemd-unit・guest-os の観測は `sh -c` の
    小さなスクリプトで、`sh` を sudo で許すのは root を与えるのと同じなので、観測用は sudo 無しで読める権限（グループ・ACL）で動かす。
    cli-device は装置の側の読むだけの権限の段、netconf は NACM の読むだけの規則。どの項目が読むだけでは観測できないかは各 provider の
    文書に書いた。
  - **読むだけでは在るかどうか分からないパスは absent にしない**（§3 第3原則）。host-file・systemd-unit（元のファイル・印・依って
    立つファイル）・guest-os（目印）の観測は、無いパスの上で最も近い在るディレクトリを辿れるときだけ absent と答え、辿れなければ
    観測できない（unknown）とする。これまで root でない利用者で動かすと、覗けないディレクトリの中のファイルを「無い」と読み、
    host-file は在るファイルを作り直す計画を出していた。dnszone は前からこの決まり。
- **見送り**:
  - HTTP の管理 API の provider（proxmox-vm・s3-bucket・postgresql・redfish・ldap・metrics-probe）の資格の分け。同じ口（`Writing`・
    `access`）で次の段で行う。
  - 書き込み用の資格を「Apply の周の間だけ」まとめて持つ形（ssh の master を Apply の間だけ残す）。並行の Apply が同じ master を
    使うので、閉じる時を決めるには参照の数え上げが要る。今はコマンドごとに接続し直す（Apply は稀なので費用は小さい）。
  - 手元（`local`・手元の nft・ファイル）の資格の分け。手元はデーモンの利用者の権限そのもので、分ける資格が無い。

## 影響

- §20.7 を新設した（§20 の目次に足した）。§29.1 に 1 行を足した。
- 公開の口 `pkg/provider` に `ConfigAccess`・`AccessObserve`・`AccessWrite`・`ObserveOnlyRun`・`CheckAccess`・`WithWrite`・`Writing`・
  `ErrNoWriteAccess` を足した（足しただけ）。外の provider は `Writing(ctx)` を見れば同じ分けができる。
- 対象の一覧（`remote_config`）に `observe:`・`write:` を足した。今の書き方はそのまま動く。
- host-file・systemd-unit・guest-os を root でない利用者で動かしている構成は、覗けないディレクトリの中のパスが「無い」でなく
  「観測できない」になる（そのリソースは何もされない）。
