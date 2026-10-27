# 外の provider を足した自前のバイナリ（§28.9）

本体のリポジトリの外で書いた provider を、fork せずに Sakutei に足す例。Sakutei を Go のライブラリとして使い、`pkg/cli` の入口に自前の provider を登録した自前のバイナリを作る。

- `main.go` — `cli.Main(cli.WithProviders(...))` で provider `kvfile` を登録する。verb・フラグ・終了コードは `sakutei` と同じ。
- `kvfile/` — `pkg/` だけで書いた provider。レコード（`name` → `value`）を 1 つの JSON ファイルに持つ。secret のフィールド `token` は SecretRef のまま受け取り、Apply の直前に file SecretBackend で解決して、値のダイジェストだけをファイルに書く。
- `go.mod` — 別のモジュール。`replace github.com/tjst-t/sakutei/v2 => ../..` で本体を指す（外部依存なし）。

## 使い方

```console
$ cd examples/external-provider
$ go build -o sakutei-kvfile .
$ cat provider-config.json
{"providers": {"kvfile": {"path": "records.json", "secret_store": "secrets.json"}}}
$ ./sakutei-kvfile plan      --intent intent --provider-config provider-config.json
$ ./sakutei-kvfile reconcile --intent intent --provider-config provider-config.json
$ ./sakutei-kvfile status    --intent intent --provider-config provider-config.json
```

intent の例：

```json
{"resources": [{"provider": "kvfile", "id": "greeting",
  "fields": {"name": "greeting", "value": "hello", "token": "secretref:file/app/token"}}]}
```

`--provider-config` の値は、共通のフラグ（`--state` 等）から作る設定に重ねて provider ごとに factory に渡る（同じキーはファイルが勝つ）。`kvfile` の設定キー：`path`（無ければ `--state` の値、それも無ければ `kvfile.json`）、`secret_store`（`token` を Apply するときに要る）。

組み込みと同じ名前（例 `fake`）で登録すると、起動時に終了コード 1 で止まる。

`tests/acceptance/external_provider_test.go`（`[AC-114-5-*]`）が、このモジュールをネットワークに出ずにビルドし、plan・reconcile・status・import・常駐で動くことを確かめている。
