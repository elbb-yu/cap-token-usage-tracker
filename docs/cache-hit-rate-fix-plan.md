# 缓存命中率实现说明

## 当前口径

仪表盘按 Token 计算缓存命中率：

```text
缓存命中率 = 有效缓存读取 Token / 输入 Token * 100
```

`input_tokens` 按当前 CLIProxyAPI 用量口径表示完整输入上下文，已经包含缓存读取部分，因此分母不能再次加上缓存读取 Token。

前端实现位于 `dashboard.go` 的 `cacheHitRate(input, cacheRead)`：

- 输入和缓存读取值先转换为非负数。
- 输入为 0 时返回 `0`，避免零除。
- 正常结果使用 `cacheRead / input * 100`。
- 缓存读取大于输入时封顶为 `100`。

## 兼容字段

有效缓存读取 Token 由 `cacheReadTokens(point)` 选择：

1. `cache_read_tokens > 0` 时使用 `cache_read_tokens`。
2. 否则回退到历史兼容字段 `cached_tokens`。

这两个字段不会相加，避免双计。

## 聚合规则

趋势图先在当前时间桶内累加：

- 输入 Token
- 有效缓存读取 Token

完成累加后再计算该桶的命中率。实现不会平均各请求、模型或子时间桶的百分比。

同一聚合结果用于页面趋势、悬浮提示和 PNG 导出，因此这些展示保持一致。模型隐藏或下钻后，命中率按当前可见数据重新聚合。

## 边界示例

| 输入 Token | 有效缓存读取 Token | 结果 |
|---:|---:|---:|
| 100 | 0 | 0% |
| 100 | 25 | 25% |
| 100 | 100 | 100% |
| 0 | 10 | 0% |
| 100 | 150 | 100% |

当 `cache_read_tokens=0`、`cached_tokens=40`、`input_tokens=100` 时，兼容回退结果为 40%。

## 数据影响

该逻辑只计算前端派生展示值，不修改：

- 用量资源接口字段
- bbolt 持久化格式
- 历史记录
- 请求级 `cache_hit` 布尔值
- 模型费用计算

## 回归测试

`dashboard_test.go` 的 `TestDashboardCacheHitRateUsesInputTokenDenominator` 固定以下契约：

- 分母只使用输入 Token。
- 不允许恢复 `input + cacheRead` 的旧公式。
- `cache_read_tokens` 优先、`cached_tokens` 回退。
- 时间桶先累加 Token，再计算比例。

验证命令：

```powershell
go test ./...
go vet ./...
```

如果上游未来引入“输入 Token 不含缓存”的新计费口径，应在数据模型中显式记录口径并按来源选择公式，不能无条件修改当前分母。
