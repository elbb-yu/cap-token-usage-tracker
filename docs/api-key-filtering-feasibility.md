# 按 API Key 筛选用量数据的可行性分析

## 问题陈述

在不修改 CLIProxyAPI 的前提下，cap-token-usage-tracker 插件能否获取每次调用所使用的 CLIProxyAPI API 密钥，并据此筛选用量数据？

**简短回答：部分可行。** 插件无法获取也无法存储 API Key 原始值（这是刻意的安全设计），但可以通过非密密的凭据标识符实现按密钥维度的区分和筛选。当前插件已有部分基础设施，但存在 API Key 类型认证无法区分 individual key 的缺口。

---

## 数据流分析

### 1. CLIProxyAPI 下发的 UsageRecord

CLIProxyAPI 通过 `usage.handle` RPC 方法向插件下发 `UsageRecord`（定义于 SDK `pluginapi/types.go`）。与密钥相关的字段如下：

| 字段 | 类型 | 说明 | 当前插件处理方式 |
|---|---|---|---|
| `APIKey` | `string` | 被选中的上游提供商 API Key | 读取后**仅用于检测 Source 是否为密钥**，不持久化 |
| `AuthID` | `string` | 被选中的凭据 ID | **完全未读取** |
| `AuthIndex` | `string` | 凭据运行时索引 | 读取后用于调用 `host.auth.get_runtime`，解析完成后**立即清除** |
| `AuthType` | `string` | 凭据类型（如 `apikey`、`oauth`） | 持久化为 `Dimensions.AuthType` |
| `Source` | `string` | 请求来源；API Key 认证时可能等于 API Key 本身 | 经过 `safeUsageSource` 清理后持久化 |

### 2. 插件的密钥安全处理

`usage.go` 中的 `safeUsageSource` 函数（第 92-106 行）是核心安全屏障：

```go
func safeUsageSource(rawSource, apiKey, provider, executorType, authType string) string {
    source := strings.TrimSpace(rawSource)
    if safeURL := sanitizeServiceURL(source); safeURL != "" {
        return normalizeDimension(safeURL)
    }
    // CLIProxyAPI currently uses the selected upstream API key itself as Source
    // for API-key credentials. Never persist that value.
    if isAPIKeyAuth(authType) || sameSecret(source, apiKey) || looksLikeCredential(source) {
        return normalizeDimension(providerServiceAddress(provider, executorType))
    }
    return normalizeDimension(source)
}
```

该函数通过三重检测阻止密钥持久化：
- `isAPIKeyAuth(authType)` — 认证类型为 `apikey`
- `sameSecret(source, apiKey)` — Source 与 APIKey 相同
- `looksLikeCredential(source)` — 值匹配凭据特征（`sk-`、`bearer `、长度≥24 且含字母数字等）

命中任一条件时，Source 被替换为提供商公共服务地址（如 `https://api.openai.com/v1`）。

### 3. AuthIndex → 身份解析

`auth_identity.go` 中的解析链路：

```
AuthIndex → host.auth.get_runtime → HostAuthGetRuntimeResponse
    → HostAuthFileEntry { Provider, Type, Email, AccountType, Account, Label, Name, ID, ... }
    → authRuntimeMetadata { Provider, Type, Email, AccountType, Account, Label }
    → usageIdentity { Provider, Account }
    → Dimensions.AuthProvider, Dimensions.AuthAccount
```

**关键限制**（`auth_identity.go` 第 112-116 行）：

```go
if strings.EqualFold(metadata.AccountType, "oauth") {
    metadata.Account = safeAuthAccount(metadata.Account)
} else {
    metadata.Account = ""  // ← API Key 类型认证的 Account 被清空
}
```

对于 OAuth 认证（如 Codex、Antigravity），`AuthAccount` 可以是邮箱，能区分不同账号。但对于 API Key 类型认证，`AuthAccount` 为空，回退链路为：

```
Email(空) → Account(非oauth清空) → Label(可能为空) → Source(已被清理为提供商URL)
```

这意味着**多个不同的 OpenAI API Key 在当前数据中会显示为相同的 `AuthProvider: "openai"`，无法区分**。

### 4. 当前持久化的维度

`Dimensions` 结构体（`aggregate.go` 第 11-24 行）：

```go
type Dimensions struct {
    Provider        string // 提供商
    ExecutorType    string // 执行器类型
    Model           string // 模型
    Alias           string // 别名
    Source          string // 已清理的来源
    AuthProvider    string // 认证提供商（显示名）
    AuthAccount     string // 认证账号（OAuth有值，API Key通常为空）
    AuthType        string // 认证类型
    ServiceTier     string // 服务层级
    ReasoningEffort string // 推理强度
    Failed          bool   // 是否失败
    FailureStatus   int    // 失败状态码
}
```

### 5. 当前筛选能力

`usageFilter`（`aggregate.go` 第 28-32 行）支持三个筛选维度：

| 筛选参数 | 对应字段 | API Key 认证下的效果 |
|---|---|---|
| `source` | `Dimensions.Source` | 所有同提供商的 API Key 请求 Source 相同，无法区分 |
| `auth_provider` | `Dimensions.AuthProvider` | 同提供商的多个 Key 值相同，无法区分 |
| `auth_account` | `Dimensions.AuthAccount` | API Key 认证下通常为空，无法区分 |

---

## 可行性结论

### ❌ 不可行：获取并存储 API Key 原始值

- **CLIProxyAPI 下发了 `UsageRecord.APIKey` 字段**，插件技术上可以读取
- 但插件的隐私安全边界（README 明确声明）和 `safeUsageSource` 的三重检测**刻意阻止密钥持久化**
- 存储原始 API Key 会违反插件的安全设计原则，不应实现

### ✅ 可行：通过非密密凭据标识符实现按密钥筛选

插件在不修改 CLIProxyAPI 的前提下，可以通过以下方式区分不同的 API Key 凭据：

#### 方案 A：存储 AuthIndex 的哈希指纹（推荐）

**原理**：`AuthIndex` 是 CLIProxyAPI 分配的稳定运行时凭据索引，每个 API Key 对应一个唯一的 AuthIndex。插件已读取该字段用于身份解析，但解析后即清除。

**实现方式**：
1. 在 `decodeUsage` 中，计算 `AuthIndex` 的 SHA-256 哈希，截取前 16 字节作为 `CredentialHash` 存入 `Dimensions`
2. 将 `CredentialHash` 加入 `usageFilter`，支持 `credential_hash` 查询参数筛选
3. 在仪表盘添加凭据哈希筛选器和分组维度

**优点**：
- 无需修改 CLIProxyAPI
- 不暴露密钥原始值
- 哈希值稳定，可跨会话关联同一凭据
- 对 OAuth 和 API Key 认证均有效

**缺点**：
- 哈希值不可读，用户需要额外的映射表（或通过 Label 关联）
- 需要修改 `Dimensions` 结构和持久化 schema（当前为 schema v6，需升级到 v7）

#### 方案 B：存储 HostAuthFileEntry 的 Name 或 ID

**原理**：插件通过 `host.auth.get_runtime` 获取的 `HostAuthFileEntry` 包含 `Name`（凭据文件名）和 `ID`（凭据记录 ID）字段。这些是非密密的标识符。

**实现方式**：
1. 扩展 `authRuntimeMetadata`，增加 `Name` 和 `ID` 字段
2. 在 `hostRuntimeAuthLookup` 中提取这些字段
3. 将 `Name`（如 `openai-key-1.json`）或 `ID` 作为 `AuthCredential` 存入 `Dimensions`
4. 加入筛选器

**优点**：
- 文件名/ID 对用户有意义，可读性好
- 不暴露密钥
- 利用已有的 host API 调用，无额外开销

**缺点**：
- `Name` 可能包含文件路径信息，需要清理
- `ID` 的格式和稳定性取决于 CLIProxyAPI 实现
- 需要修改 `authRuntimeMetadata` 和 `Dimensions`

#### 方案 C：存储 Auth Label（最小改动）

**原理**：`HostAuthFileEntry.Label` 是用户可配置的凭据标签，已通过 `authRuntimeMetadata.Label` 获取，但当前仅在 `AuthAccount` 为空时作为回退使用。

**实现方式**：
1. 将 `Label` 作为独立维度 `AuthLabel` 存入 `Dimensions`
2. 加入筛选器
3. 在仪表盘添加标签筛选

**优点**：
- 改动最小，`Label` 已在解析链路中获取
- 用户可读
- 不暴露密钥

**缺点**：
- Label 是可选的，用户可能未配置
- 多个凭据可能使用相同 Label
- 需要修改 `Dimensions` 和持久化 schema

#### 方案 D：组合方案（A + C）

存储 `CredentialHash`（用于唯一标识）和 `AuthLabel`（用于可读性），在仪表盘中同时展示。用户可以按 Label 筛选，系统内部用 Hash 关联。

---

## 实现影响评估

### 需要修改的文件

| 文件 | 修改内容 |
|---|---|
| `aggregate.go` | `Dimensions` 增加 `CredentialHash` / `AuthLabel` 字段；`usageFilter` 增加对应筛选条件 |
| `usage.go` | `decodeUsage` 中计算 AuthIndex 哈希并存入 Dimensions |
| `auth_identity.go` | 扩展 `authRuntimeMetadata` 和 `identityFromRuntimeMetadata`，提取并存储 Label/Name |
| `persistence.go` | schema 升级至 v7；序列化/反序列化新字段；聚合键包含新维度 |
| `management.go` | 三个查询端点增加 `credential_hash` / `auth_label` 查询参数 |
| `dashboard.go` | 前端增加筛选器 UI、分组维度、表格列 |
| `request_log.go` | `RequestDetail` 增加新字段 |
| 测试文件 | 更新所有受影响的测试用例 |

### 数据库 Schema 迁移

当前 `persistenceSchemaVersion = 6`。新增维度字段需要：
1. 将 schema 版本升至 7
2. 在 `initialize()` 中检测旧版本并执行迁移（新字段默认空值）
3. 聚合键 `aggregateKey` 需要包含新字段，否则历史数据与新数据无法合并

### 安全合规

| 检查项 | 状态 |
|---|---|
| 不存储 API Key 原始值 | ✅ 所有方案均不存储密钥 |
| 不存储 AuthID/AuthIndex 原始值 | ✅ 方案 A 存储哈希，方案 B/C 存储非密密标识符 |
| 新字段经过凭据清理 | ⚠️ 需对 Label/Name 执行 `looksLikeCredential` 检查 |
| 不在普通模式暴露敏感数据 | ✅ 新字段为非密密标识符，可安全展示 |

---

## 建议路径

### 推荐方案：D（组合方案）

1. **短期**（最小改动）：实现方案 C，将 `AuthLabel` 作为独立维度存储和筛选。如果用户已为 API Key 配置了 Label，可立即实现按密钥筛选。

2. **中期**（完整方案）：同时实现方案 A，存储 `AuthIndex` 的 SHA-256 哈希作为 `CredentialHash`，确保即使没有 Label 也能唯一区分每个凭据。

3. **前端**：在仪表盘筛选栏添加"凭据"筛选器，下拉选项显示 `Label (哈希前8位)` 格式，兼顾可读性和唯一性。

### 不推荐

- **存储 API Key 原始值或可逆加密值**：违反插件安全边界
- **存储 AuthID/AuthIndex 原始值**：虽然非密密，但属于内部标识符，存在信息泄露风险
- **依赖 Source 字段区分**：Source 已被清理为提供商 URL，同提供商的多个 Key 值相同

---

## 结论

| 问题 | 答案 |
|---|---|
| 能否获取每次调用使用的 API Key？ | 技术上可以读取 `UsageRecord.APIKey`，但插件设计上不存储 |
| 能否按 API Key 筛选？ | 不能直接按密钥值筛选，但可以通过非密密标识符（Label/Hash）实现等价功能 |
| 是否需要修改 CLIProxyAPI？ | **不需要**。`UsageRecord` 已提供 `AuthIndex`，`host.auth.get_runtime` 已提供 `Label`/`Name`/`ID` |
| 改动规模 | 中等。涉及 8+ 个 Go 文件、数据库 schema 升级、前端 UI 更新 |

核心结论：**在不修改 CLIProxyAPI 的前提下，插件可以通过存储 AuthIndex 哈希和/或 Auth Label 作为新维度，实现按凭据（密钥）筛选用量数据的功能。但无法也不应获取和存储 API Key 的原始值。**
