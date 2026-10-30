# provider の版を上げるときの注意（ProviderUpgradeDiff）

動いている環境で Sakutei を新しい版に入れ替えると、その中の provider の版（`ProviderVersion`。例 `netconf-v1` → `netconf-v2`、`proxmox-vm-v1` → `proxmox-vm-v2`）が上がることがある。版が上がった直後の最初の `reconcile` は、差分のあるリソースを当てずに **`ProviderUpgradeDiff`（原因タグ C）で 1 回止まる**。壊れたのではなく、設計どおりの見張り（§10.2・§10.4）である。この文書は、その意味と、確かめ方と、進め方を書く。

どの版で provider の版が上がったかは `CHANGELOG.md` に書いてある（「ProviderVersion は `netconf-v2`」のように）。今の版は `sakutei status` の各リソースの `ProviderVersion=` に出る。

## 何が起きるか

provider の版は、その provider が現実をどう読み、宣言とどう比べるか（正規形・比べる欄）の版である。版が変わると、宣言も現実も変わっていないのに「今の読み方では違う」という差分（ghost diff）が出ることがある。それが正しい直しの結果なのか provider の不具合なのかは、見た目では区別できない。そこで Sakutei は、版が変わった回に出た差分を原因タグ **C** とし、当てずに人に見せる。

一回性の `reconcile` に `--cause-state` を渡していると（試用の手順は渡している）、前の回の provider の版がそのファイルに残る。新しい版で最初に走らせたときの出力は次のようになる。

```console
$ sakutei reconcile --intent intent/ --cause-state cause-state.json ...
sakutei reconcile: ProviderUpgradeDiff: netconf//sw1/if-uplink [cause: C] provider_version changed (netconf-v1 → netconf-v2); NOT applying (review required)
Converged (no changes)
```

- C の差分は当てない。C の差分だけで止まった回の終了コードは 0 で、最後の行は `Converged (no changes)` になる（当てるものが無かったという意味で、現実が宣言どおりになったという意味ではない）。止まったことは stderr の `ProviderUpgradeDiff` の行（level の Event、§26.3）で知る。
- 作り直した環境（`--cause-state` のファイルが無い・空の状態から始めた）では前の版を知らないので、C は出ない。
- 一回性の動詞（`plan`・`reconcile`・`status`）は、その回に使う provider の版を 1 つにまとめて前の回と比べる。そのため、1 つの provider の版が上がった回は、ほかの provider のリソースの差分（ふつうの drift を含む）も C として止まる。

## 確かめ方

1 回止まったら、当てる前に差分が版の違いだけで出たものかを確かめる。

1. **差分を見る。** `sakutei status --cause-state cause-state.json` を走らせる。status は cause-state を読むだけで書かないので、何度走らせても C のまま見られる。

   ```console
   $ sakutei status --intent intent/ --cause-state cause-state.json ...
     DRIFTED netconf//sw1/if-uplink
       原因タグ: C（ghost diff: provider 更新）
       provider 階級: ... ProviderVersion=netconf-v2
       ずれ（現実 → 宣言）:
         ~ config: ...
   ```

   機械で読むなら `sakutei plan --json` の `operations[].cause` が `"C"` のものを見る（形は [plan-json.md](plan-json.md)）。ただし `plan` は終わりに cause-state を今の版で書き直すので、`plan` を走らせたあとは C でなく B（drift）として出る。C のまま何度も見たいときは status を使うか、cause-state のファイルを写してから `plan` に渡す。

2. **差分が版の違いで説明できるかを確かめる。** CHANGELOG のその provider の項（読み方・比べ方の変化）と、出た差分を突き合わせる。次のどれかなら当ててよい。
   - 新しい版の読み方で見ると、現実が宣言からずれている（前の版では見逃していた、または比べ方が直った）。当てると現実が宣言に寄る。
   - 差分が新しい版で比べるようになった欄だけにある。

   次のときは当てない。
   - 宣言にも現実にも触っていないはずの欄・リソースが、思っていない値に変わろうとしている。
   - 差分が大きすぎる（多くのリソースの多くの欄）。provider の不具合のことがある。前の版に戻し、Backlog に報告する。

   本番の前に、写しの環境（devenv・twin、§26.2）で新しい版の `plan` を流し、ghost diff を先に見ておくのがよい。

3. **ふつうの drift と混ざっていないかを見る。** 版を上げた回は、その間に起きた現実のずれも C として止まる。差分の中に、版の違いでは説明できないずれがあれば、それは drift として扱う（ふだんどおり、現実を戻すか宣言を直す）。

## 進め方

確かめたら、当てる道は次のどれか。

- **そのまま当てる。** もう一度 `sakutei reconcile --cause-state cause-state.json` を走らせる。止まった回の終わりに cause-state は新しい版で書き直されているので、2 回目は同じ差分が B（drift）として出て、ふつうの drift と同じ決まり（remediation の mode・予算・承認）で当たる。確かめに `plan` を走らせたあとでも同じである。
- **当てたくない差分を消してから進む。** 新しい版の読み方に合わせて宣言を直す（差分が消える、または A として当たる）か、前の版に戻して直りを待つ。

止まったあと確かめずに 2 回目の `reconcile` を走らせると、確かめないまま当たる。止まったら、まず status で差分を見る。

## 常駐の場合

常駐（`sakutei` を引数なしで動かす）は `--cause-state` を受けず、前の周の版をメモリに持つ（#165）。

- 常駐を新しい版に入れ替えるには起こし直すので、起こし直した最初の周は前の版を知らず、C は出ない。版の違いで出た差分は B（drift）として出て、その周の remediation の mode で扱われる（`auto` なら当たる）。
- 動いている間に provider の版が変わったとき（provider が版を動的に返すとき）は、C を当てずに止め、差分が無くなるまで比べる基準を前の版のまま止め続ける（周ごとに `ProviderUpgradeDiff` が出る）。差分を消す（現実を戻す・宣言を合わせる）か、確かめたうえで常駐を起こし直すと進む。

そのため、常駐を新しい版にするときは、起こし直す前に新しい版で一回性の `plan`（または上の手順の status）を走らせて差分を確かめる。確かめられないうちは、起こし直した常駐を `--mode alert-only` で回し、差分を `DriftWouldApply` で見てから `auto` に戻す。

## 関わる箇所

- 設計：§10.2（原因タグ）・§10.4（ghost diff を当てずに止める）、`docs/architecture/spec/part3-convergence.md`
- Event：`ProviderUpgradeDiff`（warning、level。属性 `provider_prev_version`・`provider_cur_version`）
- 終了コード：[exit-codes.md](exit-codes.md)（C だけで止まった `reconcile` は 0）
