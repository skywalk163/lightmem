# 第16轮复刻任务 Prompt 分发

> 日期：2026-09-13 ｜ 上游：`G:\github\deepseek-harness`（本地 HEAD 9d9035b7c1，基线 0.1.5-rc.2）
> 编译器：`G:\dswork\duan-light-merge\light-merge` ｜ 工作区：`G:\dswork\duan-light-merge\lightharness`（branch main）
> 前置：第15轮收口 `3dae212`（对标103条 / 缺陷L-001~L-130 / pytest 286 passed）
> 本轮定位：**上游剩余纯逻辑面的最后清剿**——前15轮已覆盖 core/session/goal/fs/web/typert/context/storage/attachment/feedback/schedule/jobs/guard/preset/settings/skill/acp/hooks/workflow 等大域，本轮聚焦 subagent 子代理核心（13纯逻辑文件）+ api 域纯逻辑类型/协议（网关流协议/会话控制器类型/控制器类型）+ 小域类型合并（plan/todo/terminal）。本轮后上游 packages/ 下可复刻的纯逻辑面基本清零，剩余均为 Cordis 宿主装配面/Node IO 面/进程外执行面。

---

## 一、铁律（8条，多轮延续，违反即打回）

1. **文件互斥**：只改本路「可写文件」表内文件；新增一律 `.light`；**禁止修改任何既有 `src/` 模块**。
2. **禁 Python 绕语言缺陷**：遇到光明语言缺陷，**必须**登记缺陷账（编号见预分配）+ 写 `examples/_repro_Lxxx.light` 最小复现（修复/绕法形态 rc=0），**禁止**用 Python 脚本绕。
3. **docs 归路M**：对标清单/缺陷账/行为差异清单由路M统一回填，各任务**不要**改 docs/。
4. **中文回复**：所有交付报告、代码注释、测试用例名用中文。
5. **反跑脚本自定位**：反跑脚本统一放 `lightharness/` 根目录，`BASE = os.path.dirname(os.path.abspath(__file__))`。
6. **命名预检查**：新增模块名前先确认不与既有 `src/*.light` 冲突（第14轮任务6、第15轮任务1先例）。
7. **测试断言数**：每个新测试至少 10 个断言用例，覆盖正常/边界/异常三态（小域类型任务至少 8 个）。
8. **反跑判据3项**：每个任务必须写反跑脚本，含 A=正常输出、B=边界、C=变异立红三项判据。

---

## 二、已知缺陷绕法速查（L-001~L-130，写代码前必读）

### 高频陷阱（最近3轮新增，最容易踩）

| 编号 | 缺陷 | 绕法 |
|---|---|---|
| **L-129** | 空列表/空字典判假（JS判真）+ `0==假`为真（JS `0===false`为假），真值语义与JS不一致 | 显式实现JS真值表函数，布尔判别用 `type(值)==bool` |
| **L-127** | 字典对象身份键不可表达，失败抛Python原生TypeError未包装 | 以归属令牌（字符串）作键 |
| **L-125** | `四舍五入` 银行家舍入(round-half-even)与JS Math.round(half-up)不一致，.5时差1 | `取整半数上(x)=向下取整(x+0.5)` |
| **L-123** | `设 字典[甲][乙] 为 值` 双层下标赋值静默失败，无报错 | 用 `字典设置(子字典,键,值)` 或先取子字典到局部变量再单层赋值 |
| **L-119** | L0单字别名静默保留字（59字全表），误用报错指向「缩进」 | 标识符完全避开上表 |
| **L-120** | 标识符可被关键字序列完全切分时仍被切碎 | 标识符避开可被关键字全覆盖切分的组合 |
| **L-116** | `序列化JSON` 键序敏感，与 `字典==` 语义不一致 | 结构相等用 `甲==乙`；若必须走JSON先键规范化 |
| **L-095** | HMAC-SHA256不可手写（编译器侧缺陷） | .light层维持自洽链路，对外对拍留待宿主 |

### 第13轮缺陷（授权链域 D1-D9）

- L-091 匿名段落内联限制 / L-092 if内段落提升 / L-093 闭包nonlocal / L-094 抛出丢属性 / L-096 空字典布尔 / L-101 `回调`保留字 / L-102 无默认参数 / L-103 可选键KeyError

> 完整缺陷账见 `docs/功能对标/语言缺陷账.md`。

---

## 三、6 路任务详细 Prompt

### 任务1：subagent 子代理核心（上）—— 目录/描述符/深度/错误/输出/收件箱

**对标编号**：#104 ｜ **缺陷编号预分配**：L-131~L-132 ｜ **差异编号**：R16-D1

**上游源**：`G:\github\deepseek-harness\packages\subagent\subagent\src\`
- `catalog.ts`（156行）— 子代理目录/注册表（按名注册/查找/列举/默认子代理）
- `descriptor.ts`（323行）— 子代理描述符（SubagentDescriptor 类型/字段/校验/构造）
- `depth.ts`（51行）— 深度限制/计算（最大深度/当前深度/深度超限判定）
- `error.ts`（15行）— 子代理错误类型（SubagentError/错误码）
- `assistant-output.ts`（75行）— 助手输出形状/处理（AssistantOutput 类型/输出归一/文本提取）
- `inbox.ts`（70行）— 收件箱/消息队列（Inbox 类型/入队/出队/积压判定）

**剔除清单**（不复刻，均为宿主面或归任务2）：
- `index.ts`（660行，cordis+child_process 装配面）
- `client.ts`（8行，纯类型重导出，内容并入 types）
- `child-agent.ts`（280行，cordis+child_process 宿主面）
- `continuation-activation.ts`（854行，cordis 宿主面）
- `continuation.ts`（550行，cordis+node 宿主面）
- `control.ts`（157行，cordis 宿主面）
- `invariant.ts`（92行，cordis 不变量安装器）
- `lifecycle.ts`（270行，cordis+node 宿主面）
- `list-children.ts`（408行，cordis+child_process 宿主面）
- `out-of-process.ts`（259行，node+child_process 宿主面）
- `projection.ts` / `projection-types.ts` / `types.ts`（child_process 类型引用，归任务2或剔除）
- `continuation-messages.ts` / `control-types.ts` / `internal.ts` / `run-settlement.ts`（归任务2）
- subagent 其他子包（subagent-acp/claude-code/codex/dsh-sdk/fork-in-process/in-process-driver/spawn-in-process 均为宿主面；tool-subagent/tool-subagent-control 归任务2或剔除）

**光明模块**：`src/子代理核心.light`（新增，命名预检查确认不与既有模块冲突）

**可写文件**（文件互斥表）：
- `src/子代理核心.light`
- `examples/test_子代理核心.light`
- `_antirun_t1_子代理核心.py`
- `_task1_子代理核心_交付报告.md`

**只读参考**（可 import，不可修改）：
- `src/代理团队.light`（#93 第14轮，参考团队/名册/生命周期形态）
- `src/异步代理.light`（早期覆盖，参考代理状态机形态）
- `src/会话格式.light`（复用事件形状/类型）

**测试要求**：
- 至少 12 个断言用例，覆盖：
  - 子代理目录（注册/查找/列举/重复注册处理/默认子代理/未知子代理）
  - 描述符（SubagentDescriptor 形状校验/字段必填/构造/非法字段）
  - 深度限制（最大深度/当前深度递增/深度超限判定/边界值0/1）
  - 错误类型（SubagentError 消息/码/各种错误场景）
  - 助手输出（AssistantOutput 形状/文本提取/空输出/多段输出归一）
  - 收件箱（入队/出队/积压判定/空收件箱/顺序保证）

**反跑判据3项**：
- A=正常：子代理目录注册→查找→列举输出与预期一致
- B=边界：空收件箱出队不崩溃（返回空或标记空）
- C=变异：把 `depth.ts` 的最大深度值改错，测试必须立红（恢复后绿）

---

### 任务2：subagent 子代理核心（下）+ model-selection —— 续传消息/控制类型/内部/运行结算/模型选择

**对标编号**：#105 ｜ **缺陷编号预分配**：L-133~L-134 ｜ **差异编号**：R16-D2

**上游源**：
- `G:\github\deepseek-harness\packages\subagent\subagent\src\`
  - `continuation-messages.ts`（154行）— 续传消息（ContinuationMessage 类型/消息构造/序列化/消息判别）
  - `control-types.ts`（148行）— 控制类型（ControlMessage/控制指令类型/控制结果形状）
  - `internal.ts`（110行）— 内部工具（内部辅助函数/常量/类型转换）
  - `run-settlement.ts`（75行）— 运行结算（RunSettlement 类型/结算状态/结果汇总/失败处理）
- `G:\github\deepseek-harness\packages\subagent\tool-subagent\src\`
  - `model-selection.ts`（196行，纯逻辑无cordis依赖）— 模型选择（模型选择逻辑/候选模型排序/选择策略/回退机制）
  - `model-selection-state.ts`（81行，纯逻辑）— 模型选择状态（选择状态机/状态转换/当前模型追踪）

**剔除清单**（不复刻）：
- subagent 核心宿主面文件（同任务1剔除清单）
- `tool-subagent/index.ts`（707行，cordis+defineTool 宿主面）
- `tool-subagent/invariant.ts`（50行，cordis 不变量安装器）
- `tool-subagent/list-models.ts`（113行，cordis+defineTool 宿主面）
- `tool-subagent/model-selection-settings.ts`（101行，cordis 宿主面）
- `tool-subagent-control/`（全部 cordis+defineTool 宿主面）
- subagent 其他子包（全部宿主面）

**光明模块**：`src/子代理续传.light`（新增）

**可写文件**：
- `src/子代理续传.light`
- `examples/test_子代理续传.light`
- `_antirun_t2_子代理续传.py`
- `_task2_子代理续传_交付报告.md`

**只读参考**：
- `src/子代理核心.light`（任务1，可 import 描述符/错误/深度等类型）
- `src/代理团队.light`（#93，参考活动流/生命周期形态）
- `src/LLM相关模块`（参考模型选择/模型配置形态，如有）

**测试要求**：
- 至少 12 个断言用例，覆盖：
  - 续传消息（ContinuationMessage 形状/构造/序列化round-trip/消息类型判别/空消息）
  - 控制类型（ControlMessage 形状/控制指令类型/控制结果形状/非法控制指令）
  - 内部工具（辅助函数/常量/类型转换/边界输入）
  - 运行结算（RunSettlement 形状/结算状态转换/结果汇总/成功/失败/部分失败）
  - 模型选择（候选模型排序/选择策略/回退机制/空候选/首选模型不可用）
  - 模型选择状态（状态机转换/当前模型追踪/状态重置/非法转换）

**反跑判据3项**：
- A=正常：续传消息构造→序列化→反序列化 round-trip 与预期一致
- B=边界：空候选模型列表的模型选择不崩溃（返回空或默认回退）
- C=变异：把 `model-selection.ts` 的首选模型优先级改错，测试必须立红（恢复后绿）

---

### 任务3：api 网关协议 —— 流协议/远程错误码/远程事件/类型

**对标编号**：#106 ｜ **缺陷编号预分配**：L-135~L-136 ｜ **差异编号**：R16-D3

**上游源**：
- `G:\github\deepseek-harness\packages\api\gateway\src\`
  - `stream-protocol.ts`（407行，纯逻辑无cordis/node依赖）— 流协议（StreamProtocol 消息类型/帧格式/序列化/反序列化/流控制/心跳/错误帧）
  - `remote-error-codes.ts`（35行，纯逻辑）— 远程错误码（错误码表/错误码到消息映射/错误类别判别）
- `G:\github\deepseek-harness\packages\api\remotes\src\`
  - `remote-events.ts`（36行，纯逻辑）— 远程事件（RemoteEvent 类型/事件判别/事件构造）
  - `types.ts`（19行，纯逻辑）— 远程类型（Remote 相关类型定义）

**剔除清单**（不复刻）：
- `gateway/index.ts`（1204行，cordis+node 宿主面）
- `gateway/stream-server.ts`（224行，node 宿主面）
- `gateway/types.ts`（159行，cordis 类型引用，宿主面）
- `remotes/index.ts`（167行，cordis+node 宿主面）
- api 其他子包（session-controller 归任务4，workspace/settings 归任务6，其余宿主面）

**光明模块**：`src/网关协议.light`（新增）

**可写文件**：
- `src/网关协议.light`
- `examples/test_网关协议.light`
- `_antirun_t3_网关协议.py`
- `_task3_网关协议_交付报告.md`

**只读参考**：
- `src/JSONRPC.light`（早期覆盖，参考 JSON-RPC 消息形状/序列化形态）
- `src/无损JSON.light`（参考 JSON 序列化）
- `src/流.light`（早期覆盖，参考流控制/帧格式形态）

**测试要求**：
- 至少 12 个断言用例，覆盖：
  - 流协议消息（StreamProtocol 帧格式/序列化round-trip/消息类型判别/数据帧/心跳帧/错误帧）
  - 流控制（序列号/确认/窗口/流控状态/乱序处理）
  - 远程错误码（错误码表查找/错误码到消息映射/错误类别判别/未知错误码）
  - 远程事件（RemoteEvent 形状/事件判别/事件构造/事件序列化/空事件）
  - 远程类型（Remote 类型形状/类型判别/非法值）
  - 协议边界（空帧/超大帧/畸形帧/版本不兼容）

**反跑判据3项**：
- A=正常：流协议数据帧构造→序列化→反序列化 round-trip 与预期一致
- B=边界：空负载的心跳帧序列化不丢字段
- C=变异：把 `remote-error-codes.ts` 的某个错误码改错，测试必须立红（恢复后绿）

---

### 任务4：api 会话控制器纯逻辑 —— 类型/助手流/远程事件

**对标编号**：#107 ｜ **缺陷编号预分配**：L-137~L-138 ｜ **差异编号**：R16-D4

**上游源**：`G:\github\deepseek-harness\packages\api\session-controller\src\`
- `types.ts`（611行，纯逻辑无cordis/node依赖，大文件）— 会话控制器类型（Session/会话状态/会话视图/会话列表/会话快照/事件类型/命令类型/投影类型/媒体引用/文件引用等全套类型定义）
- `assistant-stream.ts`（102行，纯逻辑）— 助手流（AssistantStream 类型/流事件/流状态/增量合并/文本提取）
- `remote-events.ts`（14行，纯逻辑）— 远程事件（会话控制器远程事件类型/判别）

**剔除清单**（不复刻，均为 cordis/node 宿主面）：
- `agent.ts`（529行，cordis+node）
- `catalog.ts`（67行，cordis）
- `commands.ts`（655行，cordis+node）
- `control.ts`（210行，cordis）
- `file-references.ts`（42行，cordis）
- `history.ts`（427行，cordis）
- `index.ts`（417行，cordis+node）
- `list.ts`（329行，cordis）
- `media-references.ts`（77行，cordis+node）
- `model-selection-projection.ts`（83行，cordis）
- `skill-catalog.ts`（109行，cordis）
- `client/` 子目录（全部宿主面）

**光明模块**：`src/会话控制类型.light`（新增）

**可写文件**：
- `src/会话控制类型.light`
- `examples/test_会话控制类型.light`
- `_antirun_t4_会话控制类型.py`
- `_task4_会话控制类型_交付报告.md`

**只读参考**：
- `src/会话.light`（#1 早期覆盖，参考会话类型/状态形态）
- `src/会话格式.light`（参考事件形状/消息节点类型）
- `src/网关协议.light`（任务3，可 import 流协议/远程事件类型）

**测试要求**：
- 至少 12 个断言用例（types.ts 611行大文件，类型丰富），覆盖：
  - 会话类型（Session 形状/会话状态枚举/状态转换/非法状态）
  - 会话视图/列表/快照（视图形状/列表分页/快照形状/空会话）
  - 事件类型（会话事件判别/事件形状/事件序列化/各种事件类型）
  - 命令类型（会话命令判别/命令形状/命令参数校验/非法命令）
  - 投影类型（会话投影形状/投影计算/空投影）
  - 媒体引用/文件引用（引用形状/引用判别/多引用/空引用）
  - 助手流（AssistantStream 形状/流事件/增量合并/文本提取/空流）
  - 远程事件（远程事件判别/事件形状/事件构造）

**反跑判据3项**：
- A=正常：会话类型构造→序列化→反序列化 round-trip 与预期一致
- B=边界：空会话快照的类型判别不崩溃（返回空或标记空）
- C=变异：把 `types.ts` 的某个会话状态枚举值改错，测试必须立红（恢复后绿）

---

### 任务5：小域类型合并（plan/todo/terminal）—— 计划模式/待办/终端类型

**对标编号**：#108 ｜ **缺陷编号预分配**：L-139 ｜ **差异编号**：R16-D5

**上游源**：
- `G:\github\deepseek-harness\packages\plan\plan-mode\src\`
  - `types.ts`（47行，纯逻辑）— 计划模式类型（PlanMode 枚举/计划状态/计划配置/计划步骤类型）
  - `client.ts`（10行，纯类型重导出 `export type * from './types'`）— 内容并入 types
- `G:\github\deepseek-harness\packages\todo\tool-todo\src\`
  - `types.ts`（47行，纯逻辑）— 待办类型（TodoItem 类型/待办状态/优先级/待办列表/过滤条件）
  - `client.ts`（10行，纯类型重导出）— 内容并入 types
- `G:\github\deepseek-harness\packages\terminal\terminal\src\`
  - `types.ts`（177行，纯逻辑）— 终端类型（TerminalSession 类型/终端状态/输出格式/尺寸/光标位置/终端事件）

**剔除清单**（不复刻，均为 cordis/defineTool/node 宿主面）：
- `plan/index.ts`（464行，cordis+defineTool）
- `plan/invariant.ts`（48行，cordis 不变量安装器）
- `todo/index.ts`（223行，cordis+defineTool）
- `todo/invariant.ts`（103行，cordis 不变量安装器）
- `terminal/index.ts`（476行，cordis）
- `terminal-bash/`（全部 node/cordis 宿主面：config/index/sanitize/session）

**光明模块**：`src/小域类型.light`（新增）

**可写文件**：
- `src/小域类型.light`
- `examples/test_小域类型.light`
- `_antirun_t5_小域类型.py`
- `_task5_小域类型_交付报告.md`

**只读参考**：
- `src/定时.light`（#29，参考调度/计划状态机形态）
- `src/任务系统.light`（#53，参考任务/待办状态机形态）
- `src/终端PTY.light`（早期覆盖，参考终端类型/输出形态）

**测试要求**：
- 至少 10 个断言用例（小域类型任务，文件较小但类型丰富），覆盖：
  - 计划模式（PlanMode 枚举/计划状态转换/计划配置形状/计划步骤类型/非法计划模式）
  - 待办（TodoItem 形状/待办状态转换/优先级排序/待办列表过滤/空待办/已完成过滤）
  - 终端（TerminalSession 形状/终端状态转换/输出格式/尺寸校验/光标位置/终端事件判别）
  - 三域类型互不干扰（同模块内命名空间隔离/类型判别准确）

**反跑判据3项**：
- A=正常：待办事项构造→状态转换→过滤输出与预期一致
- B=边界：空终端会话的尺寸校验不崩溃（返回默认值或标记空）
- C=变异：把 `plan/types.ts` 的某个 PlanMode 枚举值改错，测试必须立红（恢复后绿）

---

### 任务6：api 控制器类型合并（workspace/settings）—— 工作区文件/设置/工作区控制器类型

**对标编号**：#109 ｜ **缺陷编号预分配**：L-140 ｜ **差异编号**：R16-D6

**上游源**：
- `G:\github\deepseek-harness\packages\api\workspace-files\src\`
  - `types.ts`（166行，纯逻辑）— 工作区文件类型（FileChange 类型/变更类型/文件快照/文件列表/过滤条件/变更事件）
- `G:\github\deepseek-harness\packages\api\settings-controller\src\`
  - `types.ts`（39行，纯逻辑）— 设置控制器类型（Settings 类型/设置键/设置值/设置变更事件/凭证类型）
- `G:\github\deepseek-harness\packages\api\workspace-controller\src\`
  - `types.ts`（128行，纯逻辑）— 工作区控制器类型（Workspace 类型/工作区状态/目录选择/工作区事件/Feed 类型）

**剔除清单**（不复刻，均为 cordis/node 宿主面）：
- `workspace-files/index.ts`（479行，cordis+node）
- `workspace-files/changes.ts`（113行，cordis）
- `settings-controller/index.ts`（343行，cordis+node）
- `settings-controller/credentials.ts`（155行，cordis）
- `workspace-controller/index.ts`（123行，cordis）
- `workspace-controller/commands.ts`（186行，cordis）
- `workspace-controller/directory-picker.ts`（174行，cordis）
- `workspace-controller/feed.ts`（185行，cordis）
- `runtime-diagnostics/invariants/index.ts`（200行，cordis 不变量安装器，宿主面）
- api 其他子包的 client/ 子目录（全部宿主面）

**光明模块**：`src/控制器类型.light`（新增）

**可写文件**：
- `src/控制器类型.light`
- `examples/test_控制器类型.light`
- `_antirun_t6_控制器类型.py`
- `_task6_控制器类型_交付报告.md`

**只读参考**：
- `src/存储.light`（#14 早期覆盖，参考文件/存储类型形态）
- `src/安全策略.light`（#61，参考凭证/设置脱敏形态）
- `src/工作区相关模块`（如有，参考工作区类型形态）

**测试要求**：
- 至少 10 个断言用例，覆盖：
  - 工作区文件（FileChange 形状/变更类型枚举/文件快照/文件列表过滤/变更事件判别/空变更）
  - 设置控制器（Settings 形状/设置键判别/设置值类型/设置变更事件/凭证类型/空设置）
  - 工作区控制器（Workspace 形状/工作区状态转换/目录选择结果/工作区事件/Feed 类型/空工作区）
  - 三域类型互不干扰（同模块内命名空间隔离/类型判别准确）

**反跑判据3项**：
- A=正常：文件变更构造→变更类型判别→过滤输出与预期一致
- B=边界：空设置对象的键枚举不崩溃（返回空列表）
- C=变异：把 `workspace-files/types.ts` 的某个变更类型枚举值改错，测试必须立红（恢复后绿）

---

## 四、路M 收口职责（第7路，由路M执行）

1. **查收6份交付报告**：核对每路的可写文件表、测试断言数、反跑3项判据、缺陷登记。
2. **6反跑全 ALL OK**：逐个运行6个反跑脚本，确认 A/B/C 三项判据全过（C判据必须真变异立红，恢复后绿；反跑输出不能截断核验——第13轮教训）。
3. **6测试单独跑全 PASS**：`python 运行.py examples/test_xxx.light` 每个 rc=0。
4. **文件互斥零越界检查**：确认6路均只新增文件，未修改任何既有 `src/` 模块。
5. **分路提交6个commit**：每路一个 commit，message 格式 `任务N(域): 简述（缺陷编号；反跑3/3）`。
6. **docs三件回填**：
   - 对标清单追加 #104~#109
   - 缺陷账追加本轮新缺陷（L-131~L-140，无新缺陷则编号空缺顺延）
   - 行为差异追加 R16-D1~D6
7. **全量CI**：`$env:LIGHT_MERGE="G:\dswork\duan-light-merge\light-merge"; python scripts/ci_test.py`，期望 pytest 292+ passed（286基线 + 6新测试 + N repro），smoke 5/5。
8. **收口提交 + 收口说明**：写 `_taskM_第16轮收口说明.md`，含6路交付摘要/CI结果/缺陷留档/行为差异/未移植项汇总（本轮后上游纯逻辑面基本清零）/教训。

---

## 五、编号预分配

| 类别 | 编号范围 | 说明 |
|---|---|---|
| 对标清单 | #104~#109 | 6路各1条 |
| 语言缺陷账 | L-131~L-140 | 预留10个，无新缺陷则空缺顺延（参照第14/15轮空缺先例） |
| 行为差异 | R16-D1~D6 | 6路各1条 |

---

## 六、CI 期望

- **pytest**：292+ passed（第15轮基线 286 + 6 新测试 + N repro 文件）
- **smoke**：5/5 通过
- **已知 flaky**：test_审批.light（第14轮发现）偶发 rc=1，单独跑全绿，疑似系统资源竞争——若出现则复跑确认，不阻塞收口

---

## 七、本轮特殊性说明

1. **上游纯逻辑面基本清零**：本轮6路覆盖后，上游 `packages/` 下可复刻的纯逻辑面（无 cordis/node/child_process/defineTool 依赖的类型/协议/状态机/计算逻辑）基本全部复刻完毕。剩余文件均为 Cordis 宿主装配面（Context/Service/不变量安装器）、Node IO 面（fs/path/child_process/net）、进程外执行面（spawn/fork/子代理驱动）、工具定义面（defineTool）——这些需要宿主接线，不在.light纯逻辑复刻范围内。
2. **小域类型合并**：任务5（plan/todo/terminal）和任务6（workspace/settings控制器）是上游剩余的零散纯逻辑类型文件，每域只有1-2个纯逻辑文件，合并成任务以保证工作量。
3. **api 域拆分**：api 域大部分是控制器/客户端/传输层宿主面，但 gateway/stream-protocol（407行）和 session-controller/types（611行）是较大的纯逻辑文件，单独成任务。
4. **subagent 拆分**：subagent 核心有13个纯逻辑文件，拆成任务1（目录/描述符/深度/错误/输出/收件箱，6文件）和任务2（续传消息/控制类型/内部/运行结算/模型选择，6文件），均衡并行。

---

## 八、分发说明

- 6 路任务**互相独立、无共享状态、无顺序依赖**，可并行分发。
- 每路执行者拿到对应「任务N」的完整 prompt（含上游源/剔除清单/可写文件/只读参考/测试要求/反跑判据）即可开工。
- 路M（第7路）在6路全部交付后执行收口。
- 执行者遇到语言缺陷**必须**登记（用预分配编号）+ 写 repro，**禁止**用 Python 绕。
- 反跑脚本统一放 `lightharness/` 根目录，路径自定位。
