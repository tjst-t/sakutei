# CHANGELOG v3.30 → v3.31

**改訂日: 2026-10-28**

status の宣言外（unmanaged）の検出の範囲を決めた改訂である（Backlog #164。親のストーリー「proxmox-vm の使い勝手」の 2：
宣言から外した VM が status の unmanaged に出なかった）。設計の向きは変えていない。

## #164：宣言の外にあるものを status の unmanaged に出す（§26.4）

- **背景**: proxmox-vm を入れ子の Proxmox VE で使い、VM の宣言をすべて外したところ、status の unmanaged に何も出ず
  summary の total が 0 だった。宣言の外にあるものが見えないと、宣言と現実のずれを見落とす。
- **原因**: 候補を 3 つ確かめた。
  1. unmanaged の検出が prune する型（`Pruneable`）だけを見ている —— **違った**。検出は List を持つ provider をすべて見て
     おり、prune しない型でも宣言に 1 つでも残っていれば宣言外が出る。
  2. intent にその provider のリソースが 1 つも無いと provider が組み立てられず List されない —— **これだった**。
     一回性の status は provider を intent に出てくる名前からだけ組み立てる（plan・reconcile と同じ `resolveProviders`）ので、
     宣言を全部外した provider は List されなかった。
  3. scope の合わせ方 —— 関わらない（宣言との照合は provider・scope・local id の組で、proxmox-vm の List の scope は
     Read と同じ）。
- **決定**:
  - status の宣言外の検出は、provider の型が prune する型かどうかに関わらない（見せるだけで消さない。削除は §12 の規律
    だけに従う）。今の実装どおりで、決まりとして書いた。
  - 宣言の無い provider も、**その回に設定されていれば** List する。設定されているとは、`--provider-config` にその
    provider の節がある、またはその provider だけの設定（`--zone` → dnszone、`--ruleset`・`--ruleset-tables` →
    nftables、`--s3-config` → s3-bucket、`--postgresql-config` → postgresql）が与えられたこと。`--state` は fake provider
    の既定で他の provider の代わりにもなるので、それだけでは設定されたと見ない。List を持たない provider は組み立てても
    何も出さない（注記もしない。宣言が無いので検出するものが無い）。
  - そうして組み立てた provider は List にだけ使い、Read・Diff・原因タグ（provider の版）には加えない。
  - 設定されているのに組み立てられない（設定のファイルが読めないなど）provider は、List のエラーと同じく
    `unmanaged_undetectable` に `list_error` で出し、終了コード 3 にする（「宣言外ゼロ」と読まない）。
  - provider が触らないと決めているもの（proxmox-vm の `protected_vmids`・`protected_names`・テンプレート、nftables の
    base chain など）は List に出ないので宣言外にも出ない。List が complete でなければ今までどおり検出しない。
  - 常駐の `/status`・`/metrics` は今までどおり宣言外を出さない（口のために List を増やさない、§26.4 の読み取り専用の口）。
- **plan・reconcile を変えなかった理由**: 宣言の無い provider の現実を prune の候補にするのは、集合の境界（§12.1）を
  宣言せずに広げることになる。prune は宣言のある provider の `PruneWithinScope` の範囲だけのままにした。

## 受け入れテスト

- `tests/acceptance/proxmox_vm_test.go`（偽の Proxmox VE）：
  - `[AC-164-1]` 宣言が 0 件でも、宣言に無い VM が status（文と `--json`）の unmanaged に出る。触らない VM と
    テンプレートは出ない。宣言した VM は unmanaged に出ない。reconcile しても unmanaged の VM は消えない。
- `tests/acceptance/unmanaged_undeclared_provider_test.go`：
  - `[AC-164-2]` fake だけを宣言し、dnszone（`--zone`）・nftables（`--ruleset`）・s3-bucket（`--s3-config`）・
    ldap（`--provider-config`）の現実がそれぞれ unmanaged に出る。何も設定しなければ今までどおり List しない。
  - `[AC-164-3]` 設定されているが組み立てられない provider は `unmanaged_undetectable` の `list_error`・終了コード 3。
- `make integration-proxmox`（本物の Proxmox VE 9.2）に「宣言の外の VM は unmanaged」の段を足した。
