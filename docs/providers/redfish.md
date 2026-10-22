# Redfish の BMC provider（redfish-system / redfish-virtual-media）

物理サーバの BMC を Redfish で扱う provider。決まりの正は設計書 §29.1「Redfish の BMC provider」（v3.15・v3.16）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `redfish-system` | ノード名（例 `node1`） | 電源（On / Off）・起動先の上書き（Continuous / Disabled） |
| `redfish-virtual-media` | `<ノード名>/cd`（例 `node1/cd`） | 仮想メディアの CD（Insert / Eject） |

どちらも標準階級（List・BulkObserve を持たない）で、prune の対象にしない（サーバは Sakutei が作ったり消したりしない）。

## BMC の一覧（--redfish-config）

BMC のアドレスと資格は intent に書かず、`--redfish-config <file>` で渡す（`plan`・`reconcile`・`status`・常駐の `sakutei`）。書式は intent と同じ JSON / YAML サブセット。

```yaml
# bmcs.yaml
secret_store: secrets.json            # パスワードを解決する file SecretBackend（必須）
bmcs:
  node1:
    address: https://172.20.20.2      # https だけ（scheme を省けば https）
    system: /redfish/v1/Systems/1     # 省略可（/redfish/v1/Systems の最初のメンバー）
    manager: /redfish/v1/Managers/1   # 省略可（/redfish/v1/Managers の最初のメンバー）
    username: admin
    password: secretref:file/bmc/node1  # SecretRef だけ（生の文字列はエラー）
    ca_file: bmc-ca.pem               # 省略可。BMC の証明書を検証する CA（PEM）
    timeout: 10s                      # 省略可（既定 10s、1 要求あたり）
  node2:
    address: https://172.20.20.3
    username: admin
    password:
      $secret: bmc/node2
      backend: file
    insecure_skip_verify: true        # 自己署名をそのまま許す。明示したときだけ（ca_file とは併用不可）
```

- `password` は SecretRef でしか書けない（§20）。`"secretref:file/<path>"` の文字列か `{"$secret": "<path>", "backend": "file"}`。生のパスワードを書くと読み込みの時点でエラーになる（エラーの文言にも値は出さない）。
- 値は `secret_store` の file SecretBackend（`internal/render/secretbackend_file.go`、§23.6 段0-minimal）から、BMC に要求を送るたびに解決する（JIT、§20.2）。sakutei は値を持ち続けないので、store の値を書き換えれば次の要求から新しい値を使う。読み込みの時点では store にその path があることだけを確かめる。
- store が読めない周は BMC に届かないのと同じ扱い（Unknown、何も操作しない）。
- 相対パス（`secret_store`・`ca_file`）は設定ファイルのあるディレクトリから。
- 未知のキーはエラー。
- intent に、設定に無いノードを書くと計画の時点でエラー（`no BMC is configured for node "…"`）。redfish のリソースがあるのに `--redfish-config` が無いのもエラー。

secret store（file SecretBackend）の例:

```json
{"secrets": {
  "bmc/node1": {"gen": 1, "value": "…"},
  "bmc/node2": {"gen": 1, "value": "…"}
}}
```

## intent の例

dnszone・nftables など、ほかの provider のリソースと同じ intent に混ぜられる。

```yaml
resources:
  # システム: 電源を落とし、PXE 起動を常に上書きしておく
  - provider: redfish-system
    id: node1
    fields:
      power: "Off"
      power_off: graceful_then_force   # 既定は graceful（ForceOff に進まない）
      force_after: 5m                  # graceful_then_force のときだけ・必須
      boot_enabled: Continuous         # Continuous / Disabled（Once はエラー）
      boot_target: Pxe                 # BMC の AllowableValues にある値

  # システム: 電源を入れ、起動先の上書きを外す
  - provider: redfish-system
    id: node2
    fields:
      power: "On"
      boot_enabled: Disabled

  # 仮想メディア: CD にインストーラを入れる（空の image は取り出した状態）
  - provider: redfish-virtual-media
    id: node1/cd
    fields:
      image: https://repo.example/install.iso   # URL か BMC から見える絶対パス

  - provider: redfish-virtual-media
    id: node2/cd
    fields:
      image: ""
```

YAML の `On` / `Off` は文字列のまま読まれる（bool になるのは `true` / `false` だけ）が、誤読を避けるため引用符で囲むのを勧める。

### システムのフィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `power` | `On` / `Off` | Off→On は Reset On、On→Off は GracefulShutdown。PoweringOn / PoweringOff の間は操作を重ねない。再起動は宣言できない（Task の領分）。 |
| `power_off` | `graceful`（既定）/ `graceful_then_force` | graceful は ForceOff に進まず、止まらなければずれが続いて stuck で知らせる。graceful_then_force は最初の GracefulShutdown から `force_after` を過ぎたら ForceOff（予算で破壊的に数える）。 |
| `force_after` | 時間（例 `5m`） | graceful_then_force のときだけ書け、そのときは必須。 |
| `boot_enabled` | `Continuous` / `Disabled` | `Once` は計画の時点でエラー（1 回起動で BMC が Disabled に戻すので収束しない）。 |
| `boot_target` | 例 `Pxe` / `Hdd` / `Cd` | `boot_enabled: Continuous` のときだけ比べる。PATCH だけで再起動はしない。 |

観測だけの値（宣言できない）: `uuid`・`macs`・`cpu_count`・`memory_gib`・`boot_mode`・`power_state`。

### 仮想メディアのフィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `image` | URL / 絶対パス / 空 | 違う image が入っていれば同じ Apply の中で Eject → Insert、空なら Eject。取得失敗で外れたら次の周に入れ直す。破壊的に数えない。 |

観測だけの値: `inserted`・`media_image`。

## 実行例

```console
$ sakutei plan      --intent intent/ --redfish-config bmcs.yaml
$ sakutei reconcile --intent intent/ --redfish-config bmcs.yaml
$ sakutei           --intent intent/ --redfish-config bmcs.yaml --interval 30s
```

偽 BMC（`internal/redfish/redfishtest`）を相手に plan → reconcile を通す受け入れテストは `tests/acceptance/redfish_cli_test.go`（`[AC-83-*]`）。
