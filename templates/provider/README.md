# provider の golden path テンプレート（§28.6）

provider を書き始めるときの雛形。コピーして名前を 1 語置き換えるだけでビルドでき、付属の契約テストが合格する状態から始められる（`tests/acceptance/provider_template_test.go` が毎回確かめている）。

| 雛形 | 階級（§5.3） | 骨格 | 付属テスト |
| --- | --- | --- | --- |
| `full/` | 完全 + bulk | Schema → Read → Apply → List → BulkObserve | `ValidateProvider`・契約テスト（冪等性・absent→Create→Read・List 完全性・三値）・BulkObserve 等価性テスト |
| `minimal/` | 最小（TrueObserve:false） | Schema → Read（存在のみ）→ Apply | `ValidateProvider`・契約テスト（List 完全性を除く） |

標準階級（Read が値を返すが List は無い）が欲しいときは `full/` から `List` と `BulkObserve` を消し、`Capabilities` の `List` / `BulkObserve` を false にする（等価性テストも消す）。

対象を作ったり消したりしない provider（例：BMC の向こうのサーバの電源、`internal/redfish/system`）は、契約テストに `Preexisting: true` を付ける（Apply の前の Read は、今ある対象を SampleDesired と違う値で返すこと）。Schema だけでは書けない決まり（別のフィールドしだいで比べない値・「どう収束させるか」の宣言だけの値・場合によって破壊的になる Op・対象の `AllowableValues` との照合）は、任意の `provider.PlanRefiner` で計画の時点に足す。core は、差分を落とすことと Destructive を上げることだけを受け付ける。

## 使い方

```console
$ cp -r templates/provider/full internal/mywidget
$ sed -i 's/goldenfull/mywidget/g' internal/mywidget/*.go
$ go test ./internal/mywidget/
```

1. **名前を置き換える** — パッケージ名・`Name`・`ProviderVersion` は `goldenfull`（minimal は `goldenmin`）の 1 語から作られている。
2. **`Client` を実装する** — 実機の API との境界。`Provider` 本体は `Client` 越しにしか対象に触らない。到達できないときはエラー、存在しないときは `found=false`（unknown ≠ absent、§4.5）。
3. **Schema を書く** — 最難関（§28.4）。identity は運用上の変更頻度で選び、secret / managed / computed を正直に宣言する。`toState` と `recordFromFields` を合わせて直す。
4. **契約テストを合わせる** — `provider_test.go` の `Examples` に、対象が値を検査するフィールドの例を書く。テストは `fakeClient`（インメモリの対象の代役）の上で回す。`Client` を実機用にしても fake は残す。
5. **`ProviderVersion` を上げる** — `toState` や正規化を変えたら上げる。上げ忘れると ghost diff（cause C）がドリフト（B）に化ける（§10.4）。
