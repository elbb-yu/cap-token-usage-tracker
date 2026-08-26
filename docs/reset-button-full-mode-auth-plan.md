# 重置按钮改造计划书：普通模式移除 + 完整模式会话鉴权

## 1. 背景与目标

仪表盘当前有两个页面：

- 普通模式：`GET /v0/resource/plugins/{pluginID}/dashboard`（`dashboardHTML`）
- 完整模式：`GET /v0/resource/plugins/{pluginID}/full-dashboard`（`fullDashboardHTML`，需通过管理密钥登录获取会话令牌）

重置按钮（`id="resetButton"`）目前同时出现在两个页面。点击后流程为：
`confirm()` 确认 → `prompt()` 输入 `reset` → 弹出 `resetDialog` 输入管理密钥 →
`POST /v0/management/plugins/{pluginID}/reset`（`Authorization: Bearer <key>`，正文 `{"confirm":"reset"}`）。

本次目标：

1. **普通模式移除重置按钮**：普通模式页面不再出现任何重置入口（按钮、对话框、脚本）。
2. **完整模式重置按钮增加鉴权**：只有已登录完整模式且持有有效完整模式会话（即已通过管理密钥鉴权）的用户才能执行重置。

## 2. 现状分析

### 2.1 重置相关代码位置

| 位置 | 内容 |
|---|---|
| `dashboard.go:272` | `resetButton` 按钮标记（两种模式共用模板） |
| `dashboard.go:287` | `resetDialog` 管理密钥确认对话框 |
| `dashboard.go:370` | `resetURL=managementBase+'/reset'` 及 `resetDialog`/`resetKeyInput` 变量声明 |
| `dashboard.go:603` | `askManagementKey()` 弹出密钥对话框 |
| `dashboard.go:605` | `resetStats()` 重置流程 |
| `dashboard.go:618` | `resetButton` 点击事件绑定 |
| `management.go:69` | 管理路由 `resetPath`（`/v0/management/.../reset`） |
| `management.go:319-323` | 路由分发到 `resetResponse` |
| `management.go:895-926` | `resetResponse`：校验 Content-Type 与 `{"confirm":"reset"}` 后调用 `store.Reset()` |

### 2.2 完整模式既有鉴权模式

完整模式已有成熟的能力令牌（capability）模式，备份、恢复、价格保存/同步均已迁移到该模式：

- 登录：普通页面输入管理密钥 → `POST /v0/management/.../full-mode/session`（由 CLIProxyAPI 宿主以管理密钥鉴权）→ 跳转到 `full-dashboard#session=<token>`。
- 会话校验：服务端 `validFullModeSession()`（`full_mode.go:57`），TTL 15 分钟，支持撤销。
- 受保护资源均为 `/v0/resource/.../full-mode/*` 路由，要求请求头 `X-Full-Mode-Session`；缺失或过期返回 401。
- 前端 `api()`（`dashboard.go:391`）对包含 `/full-mode/` 的 URL 自动附加 `X-Full-Mode-Session` 头。
- 资源路由可携带非 GET 方法与请求体：`/full-mode/api-key-labels`（PUT + JSON body）已在线上验证可行。

**结论**：重置应复用同一模式，新增 `/full-mode/reset` 资源路由，以会话作为鉴权凭据。持有有效会话本身即证明“已登录完整模式且通过管理密钥鉴权”。

## 3. 方案设计

### 3.1 后端

1. `registeredRoutes` 增加字段 `fullModeResetPath`，注册为
   `/v0/resource/plugins/{pluginID}/full-mode/reset`。
2. `registerManagement` 的 `Resources` 列表增加：
   `{Path: "/full-mode/reset", Description: "Capability-protected statistics reset."}`。
3. `dispatchManagement` 增加分支：

   ```go
   case routes.fullModeResetPath:
       if !strings.EqualFold(request.Method, http.MethodPost) {
           return methodNotAllowed(http.MethodPost), nil
       }
       if !r.validFullModeSession(fullModeSessionFromRequest(request)) {
           return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "full-mode session is missing or expired"}), nil
       }
       return r.resetResponse(request)
   ```

   会话校验通过后直接复用现有 `resetResponse`（Content-Type、`{"confirm":"reset"}`、`store.Reset()`、API Key 代次刷新逻辑全部不变），不重复实现重置逻辑。
4. **保留**原管理路由 `POST /v0/management/.../reset` 不变：README 已承诺直接调用 Management API 时重置路由继续可用，本次只改仪表盘入口。

### 3.2 前端：普通模式移除

在 `dashboard.go` `init()` 的普通模式 `strings.NewReplacer` 中增加替换对（与移除 pricing 按钮、导出菜单的既有做法一致），将以下内容替换为空：

1. `resetButton` 按钮完整标记（第 272 行中 `<button id="resetButton" ...>Reset</span></button>`）。
2. `resetDialog` 对话框整行标记（第 287 行）。
3. `askManagementKey()` 函数整行（第 603 行）。
4. `resetStats()` 函数整行（第 605 行）。
5. 事件绑定片段 `document.getElementById('resetButton').addEventListener('click',resetStats);`（第 618 行内）。

说明：

- 第 370 行 `resetDialog=document.getElementById('resetDialog'),resetKeyInput=document.getElementById('resetKeyInput')` 声明保留（取值为 `null`），与 `backupDialog` 在普通模式被移除后变量仍声明的既有先例一致，无副作用。
- 普通模式 locale 中的 `button.reset`、`reset.*` 键保留不删：`reset.cancel` 仍被普通模式保留的 `fullModeDialog` 取消按钮引用（`data-i18n="reset.cancel"`），其余键保留无害，避免 locale 分叉维护成本。

### 3.3 前端：完整模式改为会话鉴权

在完整模式 `strings.NewReplacer` 中增加替换对：

1. URL 切换：
   `var resetURL=managementBase+'/reset';` → `var resetURL=resourceBase+'/full-mode/reset';`
2. `askManagementKey()` 函数整行 → 空（完整模式不再需要二次输入管理密钥）。
3. `resetDialog` 对话框整行 → 空（同上，不再需要）。
4. `resetStats()` 替换为会话鉴权版本：

   ```js
   async function resetStats(){
     if(!fullModeEnabled||!fullModeSession){text('error',t('fullMode.keyRequired'));return;}
     if(!confirm(t('confirm.reset')))return;
     var typed=prompt(t('confirm.typeReset'));if(typed!=='reset')return;
     try{
       await api(resetURL,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({confirm:'reset'})});
       await load();
     }catch(error){text('error',error.message);}
   }
   ```

   - 不再携带 `Authorization` 头；`api()` 检测到 URL 含 `/full-mode/` 会自动附加 `X-Full-Mode-Session`。
   - 保留 `confirm()` + 输入 `reset` 的双重确认，作为防误触的 UX 保险。
   - 会话过期/缺失时服务端返回 401，错误信息显示在页面 `#error` 区域，用户需重新解锁完整模式（与备份/恢复行为一致）。
5. `resetButton` 标记与点击绑定在完整模式保留。

### 3.4 鉴权与安全要点

- 服务端是唯一鉴权边界：前端隐藏按钮只是 UX，真正的保护来自 `/full-mode/reset` 的会话校验。
- 完整模式会话仅能通过管理密钥保护的管理路由签发，因此“持有有效会话”蕴含“已通过管理密钥鉴权”。
- 会话 15 分钟过期、可撤销、令牌以 SHA-256 哈希存储、常量时间比较——全部复用现有机制，无新增密码学代码。
- 直接调用 Management API 的 `POST /reset`（管理密钥鉴权）保持不变，不影响脚本/自动化客户端。

## 4. 变更文件清单

| 文件 | 变更 |
|---|---|
| `management.go` | `registeredRoutes` 新字段；`registerManagement` 新资源；`dispatchManagement` 新分支 |
| `dashboard.go` | 普通模式移除替换对 ×5；完整模式替换对 ×4 |
| `management_test.go` | 资源数量 19→20；资源路径断言增加 `/full-mode/reset` |
| `full_mode_test.go` | 新增完整模式重置会话保护测试 |
| `dashboard_test.go` | 更新普通/完整模式标记与脚本断言（见第 5 节） |
| `README.md` | 中英文仪表盘操作说明、完整模式资源表、Management API 表说明更新 |

不新增依赖，不改动 bbolt 持久化格式，不改动 CSP。

## 5. 测试计划

### 5.1 后端单元测试

新增（建议放 `full_mode_test.go`，参照 `TestFullModeBackupAndRestoreRequireSession` 结构）：

1. 无会话 `POST fullModeResetPath` → 401，数据未清空。
2. 伪造/格式错误会话 → 401。
3. 有效会话但正文非 `{"confirm":"reset"}` → 400。
4. 有效会话 + 正确正文 → 200，统计清空（`stats.Summary` 归零）。
5. 会话撤销后 → 401；手工置为过期后 → 401。
6. 方法非 POST → 405。

更新：

- `management_test.go:37` 资源数断言 19→20；`management_test.go:44` 路径列表加入 `/full-mode/reset`。
- `TestManagementStatsAndReset` 保持不变（验证管理路由重置不受影响）。

### 5.2 前端标记测试（`dashboard_test.go`）

- 普通模式必需片段列表移除 `"resetKeyInput.value=''"`、`"resetDialog.showModal()"`（约第 21-22 行）。
- 普通模式新增禁止片段：`id="resetButton"`、`id="resetDialog"`、`function resetStats`、`askManagementKey`。
- 完整模式：
  - 移除现有断言 `api(resetURL,{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+managementKey`（约第 647 行）。
  - 新增必需片段：`id="resetButton"`、`resourceBase+'/full-mode/reset'`、新版 `resetStats` 中的 `if(!fullModeEnabled||!fullModeSession)`、`body:JSON.stringify({confirm:'reset'})`。
  - 新增禁止片段：`askManagementKey`、`id="resetDialog"`。

### 5.3 手工验证

1. `go vet ./... && go test ./...` 全绿。
2. 构建（`build_dll.ps1` / `build.sh`）并加载到 CLIProxyAPI：
   - 普通模式页面：无重置按钮；DOM 中无 `resetDialog`。
   - 普通模式点击“完整模式”→ 输入管理密钥解锁 → 完整模式页面出现重置按钮。
   - 完整模式点击重置 → 确认 → 输入 `reset` → 成功清空并刷新。
   - 会话过期（等待 15 分钟或撤销）后点击重置 → 页面显示 401 错误。
   - 直接 `curl` 管理路由 `POST /v0/management/.../reset`（带管理密钥）仍可用。

## 6. README 文档更新

中文部分：

- “仪表盘操作”一节：从“普通模式和完整模式都支持”列表中删除“使用管理密钥和显式确认重置统计数据”，改为在完整模式说明中描述“完整模式下通过会话鉴权并重试确认后重置统计数据”。
- “完整模式资源”表新增行：`POST /v0/resource/plugins/cap-token-usage-tracker/full-mode/reset`。
- 保留 Management API 表中 `POST .../reset` 行及“直接调用 Management API 仍可使用管理密钥……”说明。

英文部分（第 442、460 行附近）做对应修改：重置从 “Both modes support...” 移至完整模式描述。

## 7. 风险与回退

| 风险 | 评估与对策 |
|---|---|
| 宿主不转发资源路由的 POST 请求体 | 低风险：`/full-mode/api-key-labels`（PUT + body）已验证非 GET 资源请求可用；测试与手工验证覆盖 |
| 会话 15 分钟过期影响操作 | 与备份/恢复一致：返回 401 提示重新解锁，行为可预期 |
| 依赖普通模式重置入口的第三方 | 仪表盘为唯一前端客户端；Management API 重置路由保留，自动化不受影响 |
| 替换对字符串与模板不同步 | `strings.NewReplacer` 未命中会静默保留原文，由 `dashboard_test.go` 的必需/禁止片段断言兜底 |

回退：全部改动集中在一次提交，`git revert` 即可完整回退。

## 8. 实施步骤

1. 后端：新增 `fullModeResetPath` 字段、资源注册与分发分支。
2. 后端测试：新增完整模式重置鉴权测试，更新注册数量断言，跑 `go test`。
3. 前端普通模式：新增 5 组移除替换对。
4. 前端完整模式：新增 4 组替换对（URL、函数、对话框、新 `resetStats`）。
5. 更新 `dashboard_test.go` 断言并跑全部测试。
6. 更新 README 中英文说明。
7. 构建 DLL/EXE，手工验证第 5.3 节场景。

## 9. 验收标准

1. 普通模式页面 HTML 中不含 `resetButton`、`resetDialog`、`resetStats`、`askManagementKey`。
2. 完整模式页面保留重置按钮；点击需通过 confirm + 输入 `reset` 双重确认。
3. 完整模式重置请求走 `/v0/resource/.../full-mode/reset` 并携带 `X-Full-Mode-Session`；无会话、会话过期或撤销时服务端返回 401 且不执行重置。
4. `POST /v0/management/.../reset`（管理密钥）行为不变。
5. `go vet ./...`、`go test ./...` 全部通过。
6. README 与新行为一致。
