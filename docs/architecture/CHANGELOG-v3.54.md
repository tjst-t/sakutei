# CHANGELOG v3.53 → v3.54

**改訂日: 2026-10-28**

v3.53 の共通の口（`provider.IntentRanges`、§7.7）で、残りの組み込みの provider の管理の範囲を intent に移す改訂である（Backlog #182。
親のストーリー「M7：本番に向けた固め」の A3 の後半）。設計の向きは変えていない（正しい状態は宣言に置く、§1.7）。

## #182：残りの provider の範囲を intent に書く（§7.7・§29.1）

- **背景**: v3.53 で dnszone・nftables・proxmox-vm の範囲を intent の `providers:` に移した。ほかの provider の設定にも範囲が
  残っていた——NETCONF の観測する機器と部分木、LDAP の検索の根と filter と scope、cli-device・host-file・systemd-unit・guest-os の
  扱うホスト、guest-os の導入の目印、postgresql・s3-bucket の scope、s3-bucket のバケットの接頭辞。
- **決定**:
  - **何が範囲か**：provider のインスタンスが「どれを管理するか」を決めるもの——ID・List に付ける scope、ID の頭になる対象の
    集合（ホスト・装置。この集合に無い ID は読み込みで断られる）、管理の木の根と条件、観測の目印。宛先・資格・時間・コマンドの
    パス・機種の定義・実装の能力（S3 の endpoint の `fields`）・ホストの上の置き場（`stamp_dir`・`unit_dir`・`quadlet_dir`）・
    書き込みを許すか（`netconf_apply`）はつなぎ方として設定に残す。同じ 1 つの対象を写す先（dnszone の `zone_hosts`・nftables の
    `ruleset_hosts`）は ID に現れないのでつなぎ方。組み込みの provider の一覧は docs/providers/ranges.md。
  - intent の `providers.<名前>.` に書けるもの：netconf の `hosts`・`subtrees.<名前>.filter`、ldap の `scope`・
    `servers.<名前>.{base_dn, filter, scope}`、cli-device・host-file・systemd-unit の `hosts`、guest-os の `hosts`・
    `markers.<名前>`、postgresql の `scope`・`servers.<名前>.scope`、s3-bucket の `scope`・`endpoints.<名前>.bucket_prefix`。
  - 正規形：ホストの組は並べ替えて比べる（同じホストの二度書きは断る）。NETCONF の filter は XML の正規形（名前空間は URI、空白・
    兄弟の順を問わない）、LDAP の `base_dn` は DN の正規形で比べ、intent にだけあるときは書いた形を使う（`provider.RangeSetting`
    に `Raw` を足した）。名前ごとの範囲（部分木・目印・サーバ・endpoint）は名前ごとに突き合わせ、intent にだけある名前は設定の分に
    足す。設定のファイルに無いサーバ・endpoint を指せば断る。
  - 設定のファイルを持つ provider（ldap・postgresql・s3-bucket）は、intent の範囲をファイルの木に重ねてから読む。ファイルで
    必須だった範囲（ldap の `base_dn`）も intent に書けばファイルから外せる。NETCONF の部分木は intent にも一覧にも無ければ断る。
  - cli-device の管理する項目と host-file・systemd-unit の管理するファイル・ユニットは前から intent の `resources:`（prune しない）
    で、移すのは扱うホストだけ。
- **見送り**:
  - 設定から範囲を外す（設定に書けなくする）こと。次のメジャー版で行う（v3.53 と同じ）。
  - NETCONF の `filter_file` を intent に書くこと（intent のファイルからの相対パスの決まりが要る）。長い filter は部分木の一覧の
    ファイルに残せる。

## 影響

- §7.7 に「何が範囲か」と一覧を足し、断るものにサーバ・endpoint を足した。§29.1 に 1 行を足した。
- 公開の口 `provider.RangeSetting` に `Raw` を足した（足しただけ）。
- docs/providers/ranges.md を新設し、netconf・ldap・cli-device・guest-os・host-file・systemd-unit・postgresql・s3-bucket の
  文書に「範囲を intent に書く」を足した。
- 設定にだけ範囲を書いている構成（`netconf_hosts`・`netconf_subtrees`・ldap の `base_dn`・`filter`・`*_hosts`・`guest_markers`・
  `scope`・`bucket_prefix`）は、`plan`・`reconcile`・`status`・常駐が警告を出す（動きは変わらない）。
