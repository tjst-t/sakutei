# サンプル:Proxmox プロバイダ + MySQL DBaaS(合成リソース)

**目的**:設計書 §5(プロバイダ実装ガイド)と §7.5(合成リソース)を、具体的な題材で示す。
Proxmox VM を**基盤プロバイダ**として定義し、その上に MySQL DBaaS を**合成リソース**として構築する。

構成:

```
mysql_database(合成リソース・ユーザが書く面)
  └─ render フェーズ2 で展開
       ├─ proxmox_vm(基盤プロバイダ・Resource)
       ├─ mysql_install(多相 Task・§9.1)
       └─ mysql_health(observability・§17.2)
```

---

## 1. 基盤プロバイダ:`proxmox_vm`

Proxmox は govmomi 相当の REST API(`/api2/json`)を持ち、VM の作成・電源制御・スペック変更ができる。
以下は §5.1 の4関数(`Schema` / `Capabilities` / `Read` / `Apply`)+ `List` の実装例。

### 1.1 Schema

Schema はフィールドごとに **type / required / secret / managed / ordered / identity / computed** を
宣言する(§5.5)。ここが最も注意深く書くべき部分。

```go
var ProxmoxVMSchema = Schema{
    Fields: []Field{
        // --- identity(削除・参照のキー) ---
        {
            Name:     "vmid",
            Type:     Int,
            Identity: true,     // ★ このフィールドで個体を識別する
            Computed: true,     // ユーザは書かない。allocation authority(allocator)が払い出す(§1.4)
        },
        {
            Name:     "node",
            Type:     String,
            Required: false,    // 未指定なら Proxmox クラスタが HA 的に選ぶ(§2 参照)
        },

        // --- desired(ユーザが書く。収束の対象) ---
        {Name: "cpu_cores", Type: Int, Required: true},
        {Name: "memory_mb", Type: Int, Required: true},
        {Name: "disk_gb", Type: Int, Required: true},
        {
            Name:     "image",
            Type:     String,
            Required: true,
            // コンテンツアドレス参照(§20.3)。名前ではなくハッシュで参照し、
            // 「同じ名前だから変わっていない」という誤認による drift 検知漏れを防ぐ。
            Format: "content-address", // 例: "debian-12@sha256:9f1c..."
        },
        {Name: "network_bridge", Type: String, Required: true},
        {Name: "vlan_tag", Type: Int, Required: false},
        {
            Name:     "power_state",
            Type:     Enum("running", "stopped"),
            Required: true,
        },

        // --- secret(§14) ---
        {
            Name:   "cloud_init_password",
            Type:   SecretRef,   // ★ 自動マスク。値は render 直前まで平文化しない
            Secret: true,
        },

        // --- managed(§11.2-4:DRS/HA が動かすフィールドは収束対象から除外) ---
        {
            Name:    "current_node",   // 実際に今動いているノード(HA が migrate しうる)
            Type:    String,
            Managed: true,   // ★ diff 対象外。desired の "node" とは別物
            Computed: true,
        },
        {
            Name:     "ip_address",    // DHCP 等で動的に付く場合
            Type:     String,
            Managed:  true,   // ★ guest agent 経由の runtime 値。収束が「違う」と判定しない
            Computed: true,
        },
        {
            Name:     "power_state_actual", // 実際の電源状態(HA が再起動した場合など)
            Type:     Enum("running", "stopped", "paused"),
            Managed:  false,  // ★ こちらは desired と突き合わせる対象(§10.3 の B=drift 検知に使う)
            Computed: true,
        },
    },
}
```

**identity の設計判断**:Proxmox の `vmid` は数値プールからの払い出しであり、ユーザが指定する
フィールドではない。§16(allocator / AllocationAuthority)の対象にする——`vmid` プールを allocator が管理し、
`proxmox_vm` の Apply(create)時に `allocator.Allocate(pool: "proxmox-vmid", owner: <resource id>)` を呼ぶ。
**VLAN ID の払い出しと同じパターンがここでも使える**(§16.1)。

**managed の設計判断**:`current_node` と `ip_address` は Proxmox の HA / DHCP が動かす値であり、
理想状態に書かせて diff 対象にすると「HA が migrate するたびに drift 判定される」という §11.2-4 の
落とし穴に直結する。**desired 側にはこれらのフィールドを置かない**(node は「配置ヒント」であって
「現在地」ではない)。

### 1.2 Capabilities

```go
func (p *ProxmoxProvider) Capabilities() Capabilities {
    return Capabilities{
        TrueObserve:   true,  // Proxmox API は current state を返せる
        List:          true,  // ノード/クラスタ単位で VM 一覧が取れる → prune・import・Sync Mode 可
        Converging:    true,  // タスク API(UPID)でジョブの進行状態を追える(§10.4 Tier1)
        CommitConfirm: false, // VM 設定変更に candidate/commit 相当は無い
    }
}
```

`List: true` により、この provider の scope は **prune 可能**であり、§23.3 の `import` と §23.8 の
`Sync Mode` の対象にもできる(§5.4 で確認した「List が import/prune/closed-world/Sync Mode を束ねる」
対称性がここで実際に効く)。

### 1.3 Read

```go
func (p *ProxmoxProvider) Read(ctx context.Context, id string) (State, ObserveStatus, error) {
    vmid, err := p.daicho.Lookup(ctx, "proxmox-vmid", id) // desired の id → 払い出し済み vmid
    if err != nil || vmid == "" {
        return State{}, Unknown, nil // まだ払い出されていない = 現実には存在しない
    }

    vm, err := p.client.GetVM(ctx, vmid)
    switch {
    case errors.Is(err, ErrNotFound):
        return State{}, Unknown, nil          // ★ 「存在しない」であって absent と断じない(§2-3)
    case errors.Is(err, ErrConnectionTimeout):
        return State{}, Unknown, err          // ★ 到達不能も unknown。破壊的判断に使わせない
    case vm.Task != nil && !vm.Task.Done:
        return partialState(vm), Converging, nil // 移行/クローン中(§10.4 Tier1)
    case err != nil:
        return State{}, Unknown, err
    }

    return State{
        "vmid":               vmid,
        "cpu_cores":          vm.Config.Cores,
        "memory_mb":          vm.Config.Memory,
        "disk_gb":            vm.Disk.SizeGB,
        "image":              vm.Config.ImageRef,       // コンテンツアドレスのまま保持
        "network_bridge":     vm.Config.NetBridge,
        "vlan_tag":           vm.Config.VLAN,
        "power_state_actual": vm.Status,
        "current_node":       vm.Node,   // managed。diff エンジンは Schema を見て自動除外する
        "ip_address":         vm.GuestAgent.IPAddress,
    }, Ok, nil
}
```

**desired を一切参照していない**ことに注意(§4.2 の契約)。`current_node`/`ip_address` は返すが、
managed 宣言により diff エンジン側で自動的に無視される——**Read はフィルタしない、Schema がフィルタする**。

### 1.4 Apply

```go
func (p *ProxmoxProvider) Apply(ctx context.Context, plan Plan) error {
    switch plan.Op {
    case Create:
        vmid, err := p.daicho.Allocate(ctx, "proxmox-vmid", plan.ResourceID) // 冪等:同 id→同 vmid
        if err != nil {
            return TransientError(err) // authority 不達は再試行可能(§6.6)
        }
        password, err := plan.Fields["cloud_init_password"].(SecretRef).Resolve(ctx) // Apply 直前のみ平文化
        if err != nil {
            return PermanentError(err) // secret 未存在は設定不備。stuck へ(§6.6)
        }
        defer password.Zero() // 使用後に即メモリから消す(§14.2)
        return p.client.CreateVM(ctx, vmid, plan.Fields, password)

    case Update:
        // cpu/memory の変更はホットプラグ非対応なら再起動を要する場合がある。
        // ここでは「reboot が要る変更」を検知したら Apply を拒否し、代わりに
        // §9.1 の多相 Task(vm-resize)を起動するよう Event を発火する設計にする
        // (収束ループに非冪等な手続きを持ち込まない・§2-2)。
        if requiresReboot(plan.Diff) {
            p.events.Emit(RequiresTaskEvent{Task: "vm-resize", Resource: plan.ResourceID})
            return nil // 今回は何もしない。次周も drifted のまま = 人/Task が対応するまで待つ
        }
        return p.client.UpdateVM(ctx, plan.Fields)

    case Delete:
        vmid, _ := p.daicho.Lookup(ctx, "proxmox-vmid", plan.ResourceID)
        if err := p.client.DeleteVM(ctx, vmid); err != nil {
            return err
        }
        // ★ Release は Observe で削除完了を確認してから(§16.4 の非対称原則)。
        //    ここでは Release しない。次の Read で Unknown が確認された後、
        //    別サイクルで allocator.Release を呼ぶ(cooldown 付き)。
        return nil
    }
    return nil
}
```

### 1.5 List

```go
func (p *ProxmoxProvider) List(ctx context.Context, scope Scope) ([]Observed, error) {
    vms, err := p.client.ListVMs(ctx, scope.Cluster) // ★ 網羅的に返す。ページング欠落は誤 prune に直結
    if err != nil {
        return nil, err // 不完全な結果を返すくらいなら error にする(§5.3)
    }
    return toObserved(vms), nil
}
```

### 1.6 プロバイダの階級

`proxmox_vm` は `TrueObserve: true` + `List: true` を満たすため **§5.4 の「完全」階級**。
prune・import・Sync Mode・原因タグ A/B 判定・converging 検知が全て機能する。

---

## 2. 合成リソース:`mysql_database`(DBaaS)

ユーザは `proxmox_vm` を直接書かない。**「MySQL データベースが欲しい」という高レベルの意図**を書き、
render フェーズ2(§7.1)が下位リソースへ展開する。

### 2.1 ユーザが書く面

```yaml
resource: mysql_database
ownership: tenant
tenant: acme
id: orders-db
spec:
  mysql_version: "8.0.39"
  size: { cpu: 4, memory_gb: 16, disk_gb: 200 }
  network: { bridge: vmbr-tenant-acme, vlan: { from_pool: tenant-acme-vlans } }  # allocator 払い出し(§16)
  backup:
    schedule: "daily@02:00"
    retention: 14
  root_credential:
    rotation: every 90d
```

**運用者が書くのはこれだけ**。低レベルの `proxmox_vm` フィールド(vmid・image のハッシュ値等)は
一切現れない——§7.5 で確立した「カタログの商品 = 合成リソースのテンプレート」そのもの。

### 2.2 展開後(render フェーズ2 が生成する下位リソース)

```yaml
# generated/ 配下(§13.1・人は編集しない)

- resource: proxmox_vm
  id: orders-db-vm
  ownership: tenant
  tenant: acme
  managed_by: mysql_database/orders-db     # 上位が所有(§8.4 参照カウント/所有権)
  spec:
    cpu_cores: 4
    memory_mb: 16384
    disk_gb: 200
    image: "mysql8-golden@sha256:7ac1..."   # golden image(§20.3)。build は別 Task
    network_bridge: vmbr-tenant-acme
    vlan_tag: 573                            # ★ allocator が払い出した具体値(§16.3 confirm 済み)
    power_state: running
    cloud_init_password: ${secret:vault:/acme/orders-db/os#password}

- task: mysql_install                        # 多相 Task(§9.1)。VM 起動後に実行
  target: orders-db-vm
  depends_on: [orders-db-vm]                 # DAG 上、VM の Apply 完了後にのみ起動可能
  spec:
    version: "8.0.39"
    root_password: ${secret:vault:/acme/orders-db/mysql-root#password}
    my_cnf_template: standard-oltp

- resource: observability                    # §17.2(監視設定を Resource として収束)
  id: orders-db-health
  target: orders-db-vm
  spec:
    - blackbox: { probe: tcp, port: 3306 }
    - blackbox: { probe: mysql_query, query: "SELECT 1", timeout: 3s }
    - alert: { name: orders-db-down, for: 2m, severity: page }

- resource: backup_policy                    # §18
  id: orders-db-backup
  target: orders-db-vm
  spec:
    schedule: "daily@02:00"
    retention_days: 14
    restore_canary: { every: 7d, scope: { datastore: canary-only } }  # §18 の「復元可能性」検証
```

### 2.3 上位ステータスの集約(§7.5)

`mysql_database` の収束ステータスは、下位4リソースのうち**最も悲観的なもの**が支配する:

```
unknown > oscillating > drifted > converging > converged
```

- `proxmox_vm` が `converging`(クローン中)→ `mysql_database` も `converging`
- `mysql_install` Task が `Failed` → `mysql_database` は `drifted`(どの下位が原因かは追跡可能)
- 全て `converged` → `mysql_database` も `converged`(=「使える状態」)

### 2.4 mysql_install(多相 Task)の内部(§9.1 の適用)

```
Idle → Provisioning(VM が running になるのを待つ)
     → Installing(mysql-server パッケージ導入)
     → Configuring(my.cnf 適用・root password 設定)
     → Verifying(SELECT 1 が通ることを確認)
     → Done
                ↘ Failed → (収束は再試行しない。alert のみ。§6.6 の permanent 相当)
```

- **べき等な再開**:各相の入口で「もう済んでいるか」を現実 Observe で確認する(例:Installing の
  入口で `mysql --version` を叩き、既に導入済みなら Configuring へスキップ)。
- **VM 再起動を跨ぐ**:Provisioning 中に VM が unknown になっても(§2-3)、タイムアウト付きで再出現を
  待つ。再出現しなければ Failed へ。

### 2.5 削除(§8.4 の判定)

```yaml
resource: mysql_database
id: orders-db
# ↓ このブロックを丸ごと削除すると…
```

- `mysql_database` は **単純専有**(§8.4 の3分類のうち)。closed-world scope 内で理想に無くなれば
  削除対象。
- ただし `pruneable: false` が既定(§8.3)なので、DB 削除は **明示 opt-in が要る**:

```yaml
resource: mysql_database
id: orders-db
deletion:
  pruneable: true          # ★ 明示しない限り、理想から消しても実際には削除されない
  final_backup: required   # 削除前に最終バックアップを強制(Workflow 化してもよい)
```

- 削除フローは Workflow(§9.2)が妥当:`final backup → 停止確認 → proxmox_vm 削除 → vlan Release
  (§16.4 の非対称原則:下流削除を Observe で確認してから allocator.Release)`。

---

## 3. この例が示す設計の接続

| 使った機能 | 章 |
|---|---|
| Schema の secret/managed/identity/computed 属性 | §5.1, §5.5 |
| Capabilities による階級判定(完全) | §5.4 |
| コンテンツアドレス参照(image) | §20.3 |
| VMID・VLAN の allocation(allocator) | §16 |
| managed fields(current_node/ip_address を diff から除外) | §11.2-4 |
| 合成リソース(mysql_database → proxmox_vm + Task + observability + backup) | §7.5 |
| 上位ステータスの集約優先度 | §7.5 |
| 多相 Task(mysql_install) | §9.1 |
| observability as Resource | §17.2 |
| backup の4分割(構成/結果/リストア/DR) | §18 |
| pruneable 既定 false・削除は明示 opt-in | §8.3, §8.4 |
| SecretRef の遅延展開(cloud_init_password 等) | §14.2 |
| generated/ 規律(展開後ファイルは人が編集しない) | §13.1 |

**この例が省略した将来検討**:`spec.network.bridge` は Proxmox 固有のまま書いている。§5.6 で触れた
「compute 抽象化」(VMware でも Proxmox でもよい)を適用するなら、`proxmox_vm` を `compute` capability の
一実装として `Provides()` に宣言し、`mysql_database` 側は `requires: { capability: compute }` とだけ
書く形に発展できる。v1 では見送り、具体プロバイダを名指す本例の形が妥当。
