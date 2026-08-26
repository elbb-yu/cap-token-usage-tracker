# GLM 5.2 代码审查问题整改计划

## 1. 文档目的

本文档基于 `CODE_REVIEW.md` 和对当前代码的二次核验，形成可执行的整改计划。目标是优先处理能够被当前实现直接证明的风险，修复工程基线问题，并避免按照不成立的审查结论误改并发和数据库生命周期。

本文档只描述后续工作，不包含源码修改。

## 2. 复核结论

| 处置 | 项目 | 结论 |
|---|---|---|
| 近期整改 | S2、S3 | full-mode 分段上传缺少资源总量约束，存在认证后内存消耗风险 |
| 近期整改 | Q1 | 13 个 Go 文件未通过 `gofmt`，格式基线不一致 |
| 近期整改 | Q3（部分） | 后端中文模型占位符绕过前端 `model.untitled` 本地化 |
| 条件整改 | C3 | 汇率缓存刷新在互斥锁内执行网络请求，缓存失效时并发延迟放大 |
| 长期治理 | Q2、B6 | `dashboard.go` 体积和字符串替换流程带来维护风险 |
| 记录观察 | B4、B5、C2、S1、Q4、Q5、Q7 | 当前不构成紧急缺陷，可结合后续需求处理 |
| 不采纳 | B1、B2、B3、C1、Q6、Q8 | 当前不存在报告所述正确性问题，部分建议可能引入新风险 |

## 3. 实施原则

1. 各阶段独立提交，不混合资源限制、格式化和前端重构。
2. 先补测试或明确可观测断言，再修改共享并发状态。
3. 不改变数据库 schema、RPC schema、备份格式和 full-mode capability 认证边界。
4. 不为理论问题引入高复杂度抽象；优化必须有明确风险、指标或维护收益。
5. 涉及 `Store`、bbolt 句柄和 actor channel 的改动必须证明锁顺序与关闭语义，不能通过丢弃命令规避阻塞。

## 4. 第一阶段：限制 full-mode 分段上传资源

### 4.1 目标

阻止已取得 full-mode capability 的客户端通过大量 `begin` 请求、过大的 chunk 数声明或长期不提交的上传占用过多内存。

### 4.2 计划改动

主要涉及 `full_mode.go`、`full_mode_test.go`，必要时涉及 `lifecycle.go`。

1. 根据各 endpoint 的 `maxPayloadBytes` 动态计算单次上传允许的最大 chunk 数：

   ```text
   maxEncodedBytes = ceil(maxPayloadBytes * 4 / 3)
   maxChunks = ceil(maxEncodedBytes / fullModeUploadChunkSize)
   ```

   计算时为 Raw URL Base64 边界预留必要余量，但不得继续对 2 MiB 价格请求开放 16000 个 chunk。

2. 增加每个 session 的活跃 upload 上限，初始建议为 4。
3. 增加 runtime 全局活跃 upload 上限，初始建议为 32。
4. 达到配额时返回稳定的 JSON 错误和 `429 Too Many Requests`；如果实现语义更适合服务容量错误，可使用 `503 Service Unavailable`，但同一情形必须保持一致。
5. 创建 upload 前先清理过期项，再统计 session 和全局数量。
6. 撤销 full-mode session 时同步删除其全部 uploads。
7. 保留现有 chunk 长度、索引、session 所属关系和 commit 最终 payload 大小校验。
8. 不延长现有 15 分钟 TTL，不将 capability 或上传内容持久化。

### 4.3 测试计划

- 价格保存和同步 endpoint 按 2 MiB 上限拒绝过大的 chunk 数；
- restore endpoint 按 64 MiB 上限接受合法边界并拒绝超界；
- 同一 session 达到 upload 上限后，新 `begin` 被拒绝；
- 不同 session 达到全局上限后，新 `begin` 被拒绝；
- 过期 upload 清理后可以重新创建；
- session revoke 后对应 uploads 被立即删除；
- session A 不能上传或提交 session B 的 upload；
- 不完整、重复和乱序 chunk 的行为被测试固定；
- 合法价格保存、同步和数据库恢复流程不回归。

### 4.4 验收标准

- 不存在不受 session 数量和全局数量约束的 upload map 增长路径；
- 单个 upload 的最大声明容量与 endpoint payload 上限一致；
- 新增边界测试全部通过；
- `go test ./...`、`go vet ./...` 通过；
- full-mode 正常保存价格和恢复备份通过手工验证。

## 5. 第二阶段：建立 Go 格式化基线

### 5.1 目标

统一当前 Go 源码格式，并防止后续再次提交未格式化文件。

### 5.2 计划改动

1. 对仓库 Go 文件统一执行 `gofmt -w`。
2. 单独审查格式化 diff，确认只有 Go 标准格式变化。
3. 在现有 CI 或验证脚本中加入 `gofmt -l` 只读检查。
4. CI 检查应使用对应平台可执行的命令，不能依赖 PowerShell 专有语法运行在非 Windows job 中。

PowerShell 本地检查示例：

```powershell
$files = Get-ChildItem -Filter '*.go' -File | ForEach-Object { $_.FullName }
$unformatted = & gofmt -l -- $files
if ($unformatted) { $unformatted; exit 1 }
```

### 5.3 验收标准

- `gofmt -l` 无输出；
- 格式化提交不包含行为修改；
- CI 在出现未格式化 Go 文件时失败；
- `go test ./...`、`go vet ./...`、`go build ./...` 通过。

## 6. 第三阶段：修复模型占位符国际化

### 6.1 目标

避免后端返回的简体中文占位符成为聚合 key 或费用模型名，使英文、繁体中文和俄文界面均通过 `model.untitled` 显示本地化文本。

### 6.2 设计决策

优先保留空模型值，将展示责任交给前端已有的 `modelName()`。如果 API 消费方要求稳定非空值，则使用与语言无关的机器标识并由前端显式映射；不能继续使用任一语言的展示文案作为数据 key。

实施前确认：

- 聚合、费用和请求响应中的空 `model` 已被调用方支持；
- 空模型保持 unpriced，不会意外匹配价格；
- 筛选、排序、CSV 和 PNG 导出均使用本地化展示名称；
- 历史数据库不需要迁移。

### 6.3 计划改动

统一检查以下路径：

- 聚合响应中的未命名模型；
- 请求列表中的未命名模型；
- cost summary、model breakdown 和 missing prices；
- 价格解析对空模型的处理；
- 前端表格、图表、筛选与导出。

请求结果中的“成功/失败”暂不在本阶段改动，因为当前前端已有 `translateRawResult()` 本地化兼容。未来若改为稳定枚举，应通过明确的 API 兼容方案实施。

### 6.4 测试与验收

- 空模型在四种 locale 下显示对应的 `model.untitled`；
- 空模型不会错误匹配价格；
- 聚合、费用和请求列表使用一致分组语义；
- 筛选和导出不出现后端注入的简体中文；
- 已有非空模型不受影响；
- 不引入数据库迁移。

## 7. 第四阶段：按指标优化汇率缓存刷新

### 7.1 启动条件

本阶段不是立即必做项。满足以下任一条件后再实施：

- 监控或日志证明缓存失效时存在明显并发等待；
- 汇率 endpoint 延迟影响仪表盘主要流程；
- 产品要求刷新期间立即返回 stale cache；
- 上游请求频率需要更严格的合并和退避控制。

### 7.2 推荐方案

使用 singleflight 或显式 `refreshing + wait channel` 状态，保持同一时刻最多一个上游请求：

1. 锁内检查 fresh cache、stale cache、retry backoff 和刷新状态；
2. 上游 HTTP 请求在锁外执行；
3. 有 stale cache 时，可按产品要求立即返回旧值并后台刷新；
4. 无 cache 时，并发调用等待同一个刷新结果；
5. 刷新完成后锁内原子更新 cache、TTL、retryAfter 和 lastError；
6. 保留现有重定向、响应大小、超时和错误脱敏限制。

不得只把 `fetch()` 移到锁外而不做请求合并，否则缓存过期时可能形成请求风暴。

### 7.3 测试与验收

- 大量并发首次请求只触发一次上游调用；
- fresh cache 不触发网络请求；
- stale cache 行为符合选定策略；
- 超时后 retry backoff 生效；
- 成功和失败时所有等待者均能结束；
- `go test -race ./...` 通过；
- 不存在等待 goroutine 泄漏。

## 8. 第五阶段：治理 dashboard 模板维护风险

### 8.1 定位

这是长期可维护性工作，不与安全或并发修复混合。现有 `panic` 是编译期内嵌资源不一致时的 fail-fast，本阶段不以“改成运行时 500”为目标。

### 8.2 分步计划

1. 为模板派生契约补测试：所有 start/end marker 必须唯一存在，普通和完整模式必须包含或排除预期控件。
2. 梳理 HTML、CSS、JavaScript 和 locale 注入边界。
3. 评估用 `//go:embed` 嵌入独立静态文件，并在构建期生成两种模式产物。
4. 将模式差异改为结构化配置或模板条件，逐步减少精确长字符串替换。
5. 对生成 HTML 做关键契约测试，并完成桌面与窄屏视觉回归。

### 8.3 约束与验收

- 不改变 CSP、安全头和 full-mode 认证边界；
- 普通模式不得残留受保护操作脚本；
- 不引入用户运行时必须安装的前端工具；
- 构建产物可复现；
- 模板标记变化能在测试或构建阶段失败；
- 四种语言、两种模式和主要视口完成回归验证。

## 9. 不采纳或暂不整改项目

### 9.1 B1：cost 查询持有 `stateMu.RLock`

当前锁保证 cost scan 期间 `s.db` 不被 restore 关闭和替换。慢扫描可能推迟 restore/close，但不存在已证明的循环等待。

不直接缩短锁范围。若指标证明等待不可接受，应先设计数据库句柄引用计数、代际句柄或显式 scan lease，不能在 snapshot 返回后解锁并继续使用可能被关闭的 `s.db`。

### 9.2 B2：Close 在 channel 满时死锁

buffered channel 满时，`Close` 等待 actor 消费槽位；actor 完成当前命令后会继续消费，不构成报告描述的锁环。

不采纳 `select + default` 丢弃 close command。关闭命令必须可靠送达，否则可能出现 `closed=true`，但 actor、数据库和 lease 仍存活。

### 9.3 B3：`saturatingInt64Sum` 负数溢出

函数在溢出判断前已将两个非正数归零，不存在报告所述负数右操作数问题。可按可读性需要补注释，但不作为 Bug 修复。

### 9.4 C1：restore 回滚后 `costGeneration` 未递增

回滚成功后 Store 层替换 db 句柄并清空 cost cache；后续查询重新扫描旧库，不会复用错误缓存。generation 递增属于语义强化，不是当前一致性缺陷。

### 9.5 其他低优先级项目

- B4：保留 `float64` 费用计算，在展示边界统一舍入；只有明确财务核算需求时才评估定点数。
- B5：`PriceSyncMetadata` 当前没有额外业务校验规则，不单独整改。
- C2：未经 ABI 级取消或生命周期设计，不增加可能导致 use-after-free 的 shutdown 超时跳过。
- S1：凭据识别保持纵深防御定位，安全边界仍是上游不把凭据写入 Source。
- Q4：默认后缀列表很小，不引入 trie 或正则；如需消除重叠顺序影响，可按长度降序规范化。
- Q5、Q7：作为随相关模块修改时顺手处理的可读性和去重工作，不单独扩大改动范围。
- Q6、Q8：原报告正文已确认不存在竞态或生命周期错误，从缺陷清单删除。

## 10. 建议提交顺序

1. `test: cover full-mode upload resource limits`
2. `fix: bound full-mode staged upload resources`
3. `style: apply gofmt baseline`
4. `ci: enforce gofmt checks`
5. `fix: localize unnamed model presentation`
6. `perf: coalesce exchange-rate refreshes`，仅在第四阶段启动条件满足时实施
7. dashboard 模板治理拆分为独立重构系列

每个行为修改提交必须包含对应测试；格式化提交不得夹带逻辑修改。

## 11. 整体验证清单

每个阶段至少执行：

```powershell
go test ./...
go vet ./...
go build ./...
```

涉及并发状态时增加：

```powershell
go test -race ./...
```

最终发布前验证：

- 普通模式和完整模式仪表盘均可加载；
- full-mode session 创建、撤销和过期行为正确；
- 价格保存、models.dev 同步、备份和恢复流程正确；
- 四种语言的未命名模型显示正确；
- 数据库备份格式和现有数据库继续可用；
- 热重载 handover 和正常 shutdown 不回归；
- 工作树不包含意外生成的可执行文件或其他构建产物。

## 12. 完成定义

本计划完成需同时满足：

- 第一至第三阶段完成并通过验收；
- 第四阶段基于指标明确决定实施或暂缓，并记录依据；
- 第五阶段至少完成模板契约测试，后续拆分有独立任务跟踪；
- 所有采纳项均有代码、测试或明确记录支撑；
- 所有不采纳项均未被误改；
- CI 中测试、vet、build 和格式检查全部通过。
