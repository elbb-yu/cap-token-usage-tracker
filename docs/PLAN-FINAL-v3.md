# 最终实施文档 v3：按客户端 API Key 分摊统计与无损换钥

> 项目：CAP Token Usage Tracker（CLIProxyAPI 插件）
> 目标分支：alpha
> 日期：2026-08-14
> 取代：PLAN-FINAL.md、PLAN-FINAL-v2.md 以及本文 v3.3 以前的设计
> 修订：v3.4（密钥代次、旧密文降级、真实宿主诊断）
> 状态：待实施；本轮只修订计划，不修改代码或数据库

---

## 一、结论与当前问题

### 1.1 必须达到的效果

1. 修改 `api_key_secret` 后，插件仍能打开原数据库，原有统计、请求历史、费用、时间、模型、来源和备注等非密文数据继续可读。
2. 旧密钥加密的 API Key 若无法由当前密钥解密，只将该 API Key 标记为“旧密钥不可用”，不得使数据库、插件初始化或整个查询失败。
3. 新密钥生效后，新请求必须使用新密钥对应的代次写入，并在有效完整模式会话中正常显示明文。
4. 未配置 `api_key_secret` 时继续使用默认值 `123456`；完整模式持续显示弱默认密钥警告，普通模式不暴露该状态。
5. `reset` 仍是显式清空全部统计的破坏性操作，但不再是换密钥流程的一部分。

### 1.2 已确认根因

当前实现把整个数据库绑定到单一 `crypto_key_id`：

- `persistence.go` 的 `ensureCryptoIdentity` 在当前配置密钥与数据库 identity 不一致时返回 `api_key_secret does not match the database crypto identity`。
- 初始化、reload、reconfigure 和 restore 都经过该检查，因此一次正常换钥会阻止整个插件启动，而不是只让旧密文不可解。
- 当前 `reset()` 会删除 `hours`、`requests`、API Key 备注、crypto identity 等全部状态。用 reset 处理换钥会造成数据丢失。
- 当前指纹由 `api_key_secret` 派生。密钥变化后，同一个客户端 Key 会产生不同指纹；只给密文增加 key ID 而不修改聚合、筛选和备注索引，仍会出现错配。
- 前端 `apiKeyDisplay(hash, key)` 把所有空明文统一显示成“明文不可用”，无法区分旧代无法解密、密文损坏、宿主未提供 Key、记录只有指纹、完整模式会话无效等情况。

### 1.3 “新调用仍显示明文不可用”的独立故障链

仓库内闭环测试直接构造带 `APIKey` 的 `pluginapi.UsageRecord`，只能证明插件内部加密、落库、查询和 Reveal 可以闭环，不能证明真实部署传入了客户端 Key。

CLIProxyAPI v7.2.129 的实际语义为：

- `UsageRecord.APIKey` 是调用 CLIProxyAPI 的客户端 Key，不是上游供应商密钥。
- Usage reporter 从请求 context 中的 Gin context 读取 `userApiKey`。
- context 不含 Gin context、认证中间件未设置 `userApiKey`、内部调用或其他非标准入口时，该字段可以合法为空。

因此新调用仍不可用至少有五种候选原因，实施时必须逐层验证：

1. 宿主发送的 `UsageRecord.APIKey` 本来就是空值。
2. 宿主实际加载的是旧 DLL/SO，或加载路径与开发目录不同。
3. 实际 `data_path` 不是正在检查的数据库。
4. 新记录已加密，但响应没有有效 `X-Full-Mode-Session`，后端执行了 Redact。
5. 后端返回了不同不可用状态，但前端把它们压成同一句占位文本。

本机仓库根目录 DLL 的时间早于当前 API Key 功能提交，不能把它视为当前源码产物；同时用户日志确实包含新版 identity 错误，所以真实运行产物很可能位于容器或其他插件目录。实施前必须确认宿主实际加载路径，不能仅检查仓库根目录文件。

---

## 二、不可破坏的约束

1. 数据库可读性与 API Key 密文可解性必须解耦。密钥错误、缺失或变化不能阻断非密文数据读取。
2. 普通模式响应不得包含 API Key 明文、密文、指纹、代次、可用状态或相关配置状态。
3. 数据库和备份不得保存 API Key 明文；日志、诊断输出和测试失败信息也不得输出明文 Key 或配置密钥。
4. AES-GCM 密文不得进入聚合键。随机 nonce 会使相同明文产生不同密文，进入聚合键会拆分桶并可能覆盖计数。
5. 旧记录不得因换钥被删除、改成空记录或归零。无法解密只改变完整模式的展示状态。
6. 不能根据 MAC 地址、主机名、容器 ID 等设备信息派生密钥。本版密钥来源仍只有配置值，缺失时为默认 `123456`。
7. 已在聊天、日志或工单中出现过的自定义密钥应视为已暴露；正式部署换成新的强密钥，但任何文档和日志都不得重复旧值。

---

## 三、目标数据模型

### 3.1 从单一 identity 改为密钥代次

数据库升级到下一个 schema 版本（当前为 7，实施时使用 8），在 meta 中保存非秘密的代次注册表：

```go
type APIKeyCryptoGeneration struct {
    ID            uint64 `json:"id"`
    KeyID         string `json:"key_id"`
    HashVersion   string `json:"hash_version"`
    CreatedAtUnix int64  `json:"created_at_unix"`
}
```

建议 meta 字段：

- `api_key_crypto_generations`：代次列表，只含非秘密的密钥标识和指纹算法版本。
- `api_key_next_generation`：下一个代次编号。
- 原 `crypto_key_id`、`api_key_hash_version` 只用于 schema 7 迁移，迁移成功后不再充当全库锁。

注册表不得保存原始 secret、AES key、HMAC key 或任何可直接解密的材料。

### 3.2 活跃代次规则

1. 配置密钥为空：API Key 追踪关闭，不创建活跃代次；旧数据库仍正常打开和查询。
2. 配置密钥非空：派生 `KeyID`，在注册表中查找。
3. 已存在相同 `KeyID + HashVersion`：重新激活原代次。用户切回旧密钥后，该代次的旧密文可再次 Reveal，新写入也继续归入该代次。
4. 不存在相同 `KeyID + HashVersion`：原子创建新代次，并只将它用于之后的新写入。
5. 当前配置只提供一个解密 context。本版不要求保存历史 secret，也不要求后台重加密。

换钥后同一个客户端 Key 的 HMAC 指纹会改变。在旧 secret 不可用时，系统不能可靠证明两个代次中的 Key 是同一个值，因此不同代次必须作为不同的 API Key 身份展示，禁止猜测或自动合并。

### 3.3 每条记录的复合身份

API Key 的内部稳定引用必须从裸 `hash` 改为：

```text
APIKeyRef = generation_id + fingerprint
```

存储规则：

| 位置 | 保存内容 |
|------|----------|
| `hours` dimension key | `APIKeyGeneration` + `APIKeyHash`，不含密文 |
| `requests` value | `APIKeyGeneration` + `APIKeyHash` + 版本化密文 |
| 内存密文索引 | `map[APIKeyRef]ciphertext` |
| API Key 备注 | `map[APIKeyRef]label` |
| 完整模式筛选 | 使用不透明 `api_key_ref`，不能只使用裸 hash |

可以保留内部 `APIKeyHash` 字段，但前端筛选、备注和选项必须以复合引用为准，避免跨代次碰撞或串联错误。为兼容已有调用，旧 `api_key_hash` 查询参数只可在能唯一解析到一个保留期内引用时临时接受；有多个候选时返回明确的 400，不能任意选择。

### 3.4 密文版本与 AAD

- cipher version 属于每条密文，不固定绑定在 generation 上。generation 只表示密钥身份与指纹算法身份。
- schema 7 已存在的 cipher v1 不批量改写，仍按旧格式和旧 AAD 解密。
- 所有迁移后的新写入使用 cipher v2，包括用户切回旧 generation 后产生的新记录；AAD 至少绑定 `cipher_version + generation_id + fingerprint`。
- Reveal 根据记录的 generation 和 cipher version 选择解密路径。
- 把密文、指纹或代次互换时必须因 AAD 校验失败。
- 解密失败只返回该项状态，不修改磁盘中的原密文，以便用户以后切回正确密钥时恢复读取。

---

## 四、schema 7 到 schema 8 的无损迁移

迁移必须在单个 bbolt 写事务或 staged 数据库中完成，失败时保留原数据库，不能做半迁移。

### 4.1 正常 schema 7 数据

若 schema 7 同时存在合法 `crypto_key_id` 和 `api_key_hash_version`：

1. 创建 generation 1，复用旧 KeyID 和 hash version；旧记录自身继续标记为 cipher v1。
2. 给所有含 API Key hash/cipher 的 `hours` 和 `requests` 记录补 `APIKeyGeneration=1`。
3. 将 `api_key_labels[hash]` 迁移为 `api_key_labels[ref(1,hash)]`。
4. 若当前配置密钥匹配 generation 1，则激活 generation 1。
5. 若当前配置密钥不匹配，创建 generation 2 并激活；generation 1 数据继续可读，但旧明文状态为 `generation_unavailable`。

### 4.2 identity 缺失或残缺

数据库含 API Key 数据但 identity 缺失、只有半项或格式非法时，不再拒绝整个数据库：

1. 把能结构化读取的旧 API Key 数据放入保留的 `unknown legacy generation`。
2. 统计和请求记录继续加载；相关密文返回 `identity_missing`，不尝试猜测密钥。
3. 当前配置密钥建立新的正常代次，之后的新请求照常加密。
4. 记录不含 API Key 的历史数据保持“未归属”，不能误标为解密失败。

该降级只适用于密码学 identity 问题。bbolt 页面损坏、bucket 缺失、无法解析的核心统计结构等真正的数据库结构损坏仍可阻止打开，并应返回不同错误。

### 4.3 重写聚合键

`hours` 的 key 是序列化维度。迁移增加 generation 时必须用“读取旧桶 -> 构造新维度 -> 合并 Counters -> 写新桶 -> 验证 -> 删除旧键”的事务化流程。即使理论上不会碰撞，也必须在发生目标键重复时累加 counters，禁止后写覆盖前写。

### 4.4 迁移后验证

事务提交前至少验证：

- 每个非空 APIKeyHash 都有 generation 或被明确标记为 unknown legacy。
- 每个 generation 引用都存在于注册表。
- label key、密文索引和筛选引用都使用同一复合编码。
- `hours` 总请求数和各 token/cost counter 在迁移前后完全一致。
- requests 数量、sequence、时间和非 API Key 字段完全一致。

---

## 五、运行时与重配置

### 5.1 写入路径

`handleUsage` 必须取得同一个不可变 runtime 快照，其中包含 Store、活跃 generation 和 crypto context：

1. `UsageRecord.APIKey` 为空时，不生成 hash 或密文；记录写入未归属桶。响应需要能将这种来源缺失与解密失败区分开。
2. API Key 非空且追踪启用时，用活跃代次的 index key 生成 fingerprint，再用同代次 encryption key 加密。
3. generation、fingerprint 和 ciphertext 作为一个整体交给 Store；加密失败时整条 usage 拒绝，不能只写 fingerprint。
4. Store 只验证 generation 已注册、字段组合合法，不要求它等于此刻的活跃 generation。

最后一条是并发换钥的关键：切换前已经取得旧快照的在途请求可以安全写入旧代次，切换后取得新快照的请求写入新代次，两者都不会污染彼此。

### 5.2 原子 reconfigure

换钥流程必须为：

1. 解析候选配置并派生候选 KeyID。
2. 通过 actor 命令在数据库中查找或原子创建对应 generation。
3. generation 持久化成功后，发布包含新配置、generation 和 crypto context 的 runtime 快照。
4. 任一步失败都保留旧 runtime；不得先发布新 context 再补数据库元数据。

不得再执行“当前 KeyID 必须等于数据库全局 KeyID”的检查，也不得要求 flush 后 reset。换钥失败不能删除或重写旧数据。

### 5.3 reload、prune 与 reset

- `reload` 加载所有代次的聚合和请求，构建 `map[APIKeyRef]ciphertext`；无法解密不属于 reload 错误。
- retention prune 按复合引用清理过期 ciphertext 和 labels，只删除已经不被任何保留期内 hours/requests 引用的项。
- `reset` 明确标注为“清空全部统计和 API Key 元数据”。它只用于用户主动清空数据库，不用于换钥、禁用追踪或修复 identity。

---

## 六、Reveal、状态模型与前端

### 6.1 后端明确状态

完整模式响应不能再让前端从空字符串猜测原因。每个 API Key 项使用固定枚举，例如：

| 状态 | 含义 | 前端建议文本 |
|------|------|--------------|
| `available` | 当前配置可解密且认证成功 | 显示备注，否则显示明文 |
| `source_missing` | 宿主 usage record 没有客户端 Key | 未提供客户端 Key |
| `generation_unavailable` | 有合法旧代密文，但当前未配置该代密钥 | 明文不可用（状态详情可说明旧密钥） |
| `ciphertext_missing` | 有复合指纹但没有可用密文 | 未保存明文密文 |
| `ciphertext_invalid` | base64、版本或 GCM/AAD 校验失败 | 密文损坏或不匹配 |
| `identity_missing` | 迁移自缺失 identity 的历史数据 | 历史密钥信息缺失 |

可以使用 `api_key_status` 加可选 `api_key_unavailable_reason`，但状态集合必须由后端定义。普通模式 Redact 时同时删除明文、ref、hash、generation 和状态字段。

`source_missing` 不能凭现有的“完全没有 API Key 字段”聚合行自动推断，因为历史未启用追踪、显式关闭追踪和宿主未提供 Key 目前都会落入同一未归属桶。若要准确展示该状态，schema 8 需新增非敏感的内部 capture 状态并进入聚合维度；否则 UI 只能统一显示“未归属”，诊断计数则在写入时单独记录。

### 6.2 聚合响应

- `APIKeyOption`、GroupStats、RequestDetail 和 CostResponse 使用同一 Reveal 规则。
- stats 从应用时间范围和其他筛选后的结果收集 `APIKeyRef`，再回填对应密文和状态，不能返回全库 Key 列表。
- 没有 API Key 引用的普通聚合行显示“未归属”，不能显示“明文不可用”。
- 单条密文失败只影响该项；同一响应的其他项目继续 Reveal。

### 6.3 前端规则

1. API Key 控件仍只存在于完整模式 dashboard。
2. 所有 stats/requests/costs 和 labels 请求继续携带 `X-Full-Mode-Session`。
3. 显示优先级为：用户备注 -> `available` 明文 -> 状态对应的明确占位文本。
4. 筛选和备注保存使用 `api_key_ref`，不再用裸 hash 作为唯一身份。
5. 默认 `123456` 时持续显示现有警告；换成强密钥后警告消失，但旧代不可用项仍按自己的状态显示。
6. 四种 locale 增加上述状态文案，不能把所有失败继续翻译为同一个“明文不可用”。

---

## 七、backup 与 restore

1. backup 保留所有代次注册表、复合指纹、密文和备注，不含 secret 或明文。
2. restore 先在 staged 文件上迁移和验证，再替换 live db。
3. restore 不再要求备份 identity 与当前配置密钥一致。密钥不匹配只会使相应旧代明文不可用。
4. 当前配置 KeyID 若匹配备份中的代次，则激活该代次；若不匹配，则在 staged 数据库中创建新代次供恢复后的新写入使用。
5. restore 失败必须保持 live db、Store handle、内存状态和查询结果不变。
6. 只有结构损坏、超出支持 schema、复合引用无法确定归属等完整性错误才拒绝 restore；单纯无法解密不是拒绝理由。

---

## 八、真实部署诊断计划

实施代码前先完成一次不泄密的基线诊断：

1. 从宿主日志或插件管理信息确认实际加载的 DLL/SO 绝对路径。
2. 记录产物 SHA-256、修改时间、文件大小和内嵌插件版本；不得仅依赖文件名。
3. 确认实际 `data_path` 和当前数据库绝对路径。
4. 对数据库做只读统计：各 schema/代次记录数、有 hash 无 ciphertext 数、有 ciphertext 数、无 source Key 数、各 Reveal 状态计数。
5. 在真实 HTTP 客户端请求链路中，仅记录 `APIKey present=true/false` 和长度区间，不记录明文、完整 hash 或密文。
6. 用有效完整模式 session 直接请求 stats/requests/costs，检查响应是否含 `api_key_ref`、状态和新代密文的 Reveal 结果。
7. 再用浏览器验证同一响应，排除前端缓存、旧 dashboard 资源或 session header 丢失。

若宿主实际派发的 `UsageRecord.APIKey` 为空，插件不能凭空恢复客户端 Key。该情况需要在 CLIProxyAPI 的对应入口补齐 `userApiKey` 传播，或在插件 UI 明确显示来源缺失；不能伪装成解密失败。

---

## 九、逐文件实施范围

| 文件 | 计划改动 |
|------|----------|
| `sensitive.go` | crypto context 增加 generation；保留 cipher v1 解密，新增 cipher v2；Reveal 返回结构化状态而不是只清空字符串 |
| `aggregate.go` | Dimensions 增加 generation/ref 或等价字段；usageFilter 使用复合引用；APIKeyOption 和各响应携带完整模式状态 |
| `request_log.go` | RequestDetail 持久化 generation/capture 状态；逐项 Reveal 不影响其他记录 |
| `persistence.go` | schema 8、代次注册表、schema 7 无损迁移、复合 ciphertext/label 索引、无损 reload/prune/restore |
| `lifecycle.go` | reconfigure 先持久化代次再原子发布 runtime；handleUsage 使用 generation 与 crypto 一致快照 |
| `management.go` | `api_key_ref` 鉴权和校验；旧 hash 参数的有限兼容；敏感响应状态统一出口 |
| `full_mode.go` | labels API 改用复合引用；只返回非秘密的 tracking/default-secret 状态 |
| `dashboard.go` | 按状态显示明确文案；筛选和备注使用 ref；保留默认密钥警告；确保 session header |
| `config.go` | 默认仍为 `123456`；显式空值仅关闭新写入追踪，不影响旧库打开 |
| `locales/*.json` | 为旧代不可用、来源缺失、密文缺失/损坏、identity 缺失等增加四语文案 |
| `*_test.go` | 迁移、换钥、并发、真实 ABI/HTTP 链路、前端状态、backup/restore 和无泄密测试 |

---

## 十、实施顺序

1. 添加只读诊断和 fixture，固定 schema 7 数据在迁移前的统计总数、请求数、labels 和密文分布。
2. 定义 schema 8 generation/ref/status 编码及严格校验函数，先补序列化测试。
3. 实现 staged schema 7 -> 8 迁移和计数不变验证，暂不开放新写入。
4. 改造 Store reload、query、prune、labels、backup/restore，使它们可处理多代数据。
5. 实现 reconfigure 代次注册与 runtime 原子发布，再接入新写入。
6. 改造 Sensitive Reveal、管理接口和完整模式前端状态。
7. 加入真实 CLIProxyAPI HTTP/插件 ABI 集成测试和部署产物核验。
8. 完成竞态、故障注入、备份内容扫描、浏览器验证后再发布。

迁移和查询必须先于新写入上线。不能先产生 schema 8 数据，再补旧库迁移或前端兼容。

---

## 十一、测试计划

### 11.1 数据保留与换钥

- schema 7 使用密钥 A 写入包含 hours、requests、cost、label 的 fixture；以密钥 B 启动后，所有非密文统计与迁移前完全一致。
- 密钥 B 下，A 代记录为 `generation_unavailable`；新请求使用 B 代且 Reveal 成功。
- 再切回 A，A 代旧记录恢复 Reveal；B 代变为不可用；任何记录均未被改写或删除。
- 显式空 secret 后数据库仍能打开，旧记录可查，新请求不保存 API Key；再启用强密钥时创建或恢复对应代次。
- 换钥流程不调用 reset，不删除 labels，不改变 since/lastUsed/request sequence。

### 11.2 迁移完整性

- migration 前后逐项比较请求数、token counters、费用输入、时间、模型、来源和失败状态。
- 聚合键目标碰撞时 counters 累加而非覆盖。
- identity 缺失/半项时降级为 unknown legacy，数据库仍可读。
- 核心 bucket 损坏、非法 JSON、未来 schema 仍明确失败，不能被“密文可降级”掩盖。
- 迁移中任意写入失败时原库字节保持不变，重试可成功。

### 11.3 并发与原子性

- reconfigure 与并发 Record：每条记录的 generation、fingerprint、ciphertext 必须来自同一快照。
- 已取得旧快照的在途请求在换钥后仍可写旧代，不触发全库 identity 错误。
- 新 generation 持久化失败时旧 runtime 继续服务；发布后不会出现 generation 未注册的记录。
- `go test -race ./...` 覆盖 Record、Query、Reveal、prune、labels、restore、reconfigure 和 Close。

### 11.4 Reveal 与鉴权

- 普通模式 JSON 不含 key/ref/hash/generation/status，带 `api_key_ref` 或旧 hash 筛选但无 session 时查询前 403。
- 完整模式分别断言 available、generation_unavailable、ciphertext_missing、ciphertext_invalid、identity_missing 和未归属。
- 单项密文损坏只影响该项；其他 rows、requests、costs 正常返回。
- 篡改 generation、fingerprint、ciphertext 或 cipher version 均因 AAD/格式校验失败。
- 无效或过期 full-mode session 不得被前端误报成旧密钥问题。

### 11.5 真实宿主链路

- 通过 CLIProxyAPI 正常 HTTP 认证入口发请求，断言插件收到非空客户端 `UsageRecord.APIKey`，但测试日志不打印其值。
- 覆盖无 Gin context、无 `userApiKey` 和内部调用，断言它们进入未归属/来源缺失路径而非生成假 Key。
- 验证宿主加载产物 hash 与本次构建一致，验证真实 `data_path` 中的新 request 含当前 generation 和 ciphertext。
- 用真实完整模式 session 请求 API，并用浏览器检查筛选、表格、备注和状态文本。

### 11.6 命令与安全检查

```powershell
gofmt -w <本次修改的 Go 文件>
go build ./...
go test ./...
go test -race ./...
git diff --check
```

另外对测试数据库和 backup 做二进制/字符串扫描，确认不含测试 API Key 明文和配置 secret。所有诊断日志只允许输出 presence、generation ID、状态计数和产物 hash。

---

## 十二、验收标准

1. 修改为任意有效新密钥后，已有数据库可打开，插件正常返回 capabilities；不再出现“secret 不匹配导致整个 reconfigure 失败”。
2. 换钥前后的统计总数、请求历史、费用、备注和其他维度完全保留；换钥过程不需要 reset。
3. 旧代密文不能解密时主文案显示“明文不可用”，后端返回明确的 `generation_unavailable` 状态；切回对应旧密钥后可再次读取。
4. 新密钥下产生的真实宿主请求使用新 generation 写入，并在完整模式正常显示明文。
5. 宿主未提供客户端 Key、密文缺失、密文损坏、identity 缺失和 session 无效均能被区分，不能统一显示“明文不可用”。
6. 普通模式不包含任何 API Key 敏感字段或可用状态，也不能通过筛选参数形成旁路。
7. 默认密钥仍为 `123456`，完整模式持续警告其不保护数据库/备份泄露；普通模式不显示该警告。
8. backup/restore 支持多代数据和不匹配的当前密钥；无法解密不是拒绝 restore 的理由。
9. schema 7 -> 8 迁移原子且计数不变，损坏数据与不可解密数据有不同错误语义。
10. `go build ./...`、`go test ./...`、可用平台的 `go test -race ./...`、真实 HTTP/ABI 集成和浏览器验证全部通过。

---

## 附录 A：明确废止的旧规则

以下 v3.3 规则全部废止，实施时不得保留为隐藏分支：

- “数据库只能绑定一个 `crypto_key_id`”。
- “不同密钥启动/reconfigure 必须失败”。
- “换密钥前必须 reset”。
- “restore identity 必须与当前配置完全一致”。
- “无法 Reveal 时只清空 APIKey，让前端统一显示明文不可用”。
- “裸 APIKeyHash 足以作为跨换钥周期的唯一备注和筛选身份”。

新的原则是：**密钥变化创建或激活代次；数据库始终读取；只有当前缺少解密材料的密文按项降级。**
