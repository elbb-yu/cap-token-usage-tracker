# 前端下拉菜单交互实现说明

## 架构

仪表盘前端内嵌在 `dashboard.go` 的 `dashboardHTMLTemplate` 中，不依赖第三方前端框架。菜单交互分为两类：

- 已有自定义弹层：导出菜单、请求列菜单和维度列菜单。
- 渐进增强选择器：聚合粒度、来源、认证账号、请求筛选和完整模式价格计费方式。

普通模式会移除导出菜单及其脚本；通用交互代码必须允许相关 DOM 或函数不存在。

## 统一动画状态

所有下拉弹层使用 `.dropdown-surface`，状态为：

- `is-opening`：节点已取消 `hidden`，等待下一帧进入打开态。
- `is-open`：可见且可交互。
- `is-closing`：播放退场动画，完成后才设置 `hidden`。

默认动画参数：

```text
打开：160ms cubic-bezier(.2,.8,.2,1)
关闭：120ms cubic-bezier(.2,.8,.2,1)
位移：6px
缩放：0.98 -> 1
透明度：0 -> 1
```

向上展开时位移方向和变换原点反转。只动画 `opacity` 和 `transform`，不动画尺寸或布局位置。

`@media (prefers-reduced-motion: reduce)` 会缩短或取消可感知动画。

## 状态机

通用入口包括：

- `initializeDropdownSurface(menu)`
- `openDropdownSurface(menu, button, position)`
- `closeDropdownSurface(menu, button, restoreFocus, afterClose)`
- `finishDropdownClose(menu, token)`

每个菜单维护独立 token 和关闭计时器：

1. 打开时取消旧关闭计时器并递增 token。
2. 先取消 `hidden` 并完成定位，再在下一帧加入 `is-open`。
3. 关闭时立即将触发器 `aria-expanded` 更新为 `false`。
4. `transitionend` 在 `opacity` 结束后完成隐藏。
5. 兜底计时器保证缺少过渡事件时仍能完成关闭。
6. 延迟回调在修改 DOM 前校验 token，避免快速重开后被旧回调关闭。

全局只允许一个 `activeDropdown`。打开新菜单会关闭旧菜单；点击外部关闭但不抢焦点，`Escape` 关闭并将焦点返回触发器。

## 原生选择器增强

`enhanceSelect(select)` 保留原生 `<select>` 作为唯一值源，并生成：

- `role="combobox"` 触发按钮
- `role="listbox"` 菜单
- `role="option"` 选项

原生选择器使用视觉隐藏样式，不使用 `display:none`。选中增强选项后会更新原生 `.value`，再派发冒泡的 `change` 事件，因此现有筛选、偏好保存和价格收集逻辑无需改写。

支持的键盘交互包括：

- `ArrowDown` / `ArrowUp`
- `Home` / `End`
- `Enter` / `Space`
- `Escape`
- 字符串快速定位

活动项通过 `aria-activedescendant` 暴露，并在滚动区域内保持可见。

`syncEnhancedSelect(select)` 用于同步动态选项、语言变化、禁用状态和外部值变化。`enhanceDashboardSelects(root)` 负责批量增强。价格编辑器重绘后会再次增强新创建的计费模式选择器。

## 定位与视口

菜单使用 `position: fixed`，根据触发器矩形和当前视口空间决定向上或向下展开。宽度和高度受视口约束，长来源或模型列表在菜单内部滚动。

请求列和维度列菜单在窗口尺寸变化时重新定位。关闭结束后才复位临时位置，避免退场过程中跳动。

## 模式边界

- 普通模式包含日期范围、来源、认证账号、聚合粒度、请求筛选和列控制。
- 完整模式额外包含导出菜单和价格计费方式选择器。
- 日期范围弹层和时间选择器使用各自的动画状态机，但遵循相同的打开、关闭、焦点和减少动态效果原则。
- 对话框、价格 Context Tier 折叠区和图表提示框不属于下拉菜单。

## 回归测试

`dashboard_test.go` 覆盖：

- `.dropdown-surface` 的打开和关闭状态。
- `transitionend` 与关闭计时器兜底。
- `prefers-reduced-motion`。
- combobox/listbox/option ARIA 关系。
- 方向键、Home、End 和原生 `change` 事件。
- 动态来源、认证账号和价格选择器同步。
- 普通模式删除导出脚本后，日期范围仍可打开。

验证命令：

```powershell
go test ./...
go vet ./...
```

修改菜单逻辑时还应在桌面端和窄屏下检查快速连续点击、菜单互斥、外部点击、`Escape`、焦点返回、向上展开、长列表滚动和各主题对比度。
