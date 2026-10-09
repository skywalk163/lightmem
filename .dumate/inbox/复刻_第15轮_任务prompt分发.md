# 第15轮复刻任务 Prompt 分发

> 日期：2026-09-13 ｜ 上游：`G:\github\deepseek-harness` @ a305303422（0.1.5-rc.2）
> 编译器：`G:\dswork\duan-light-merge\light-merge` ｜ 工作区：`G:\dswork\duan-light-merge\lightharness`（branch main）
> 前置：第14轮收口 `31d6a31`（对标97条 / 缺陷L-001~L-120 / pytest 276 passed）
> 本轮定位：**未覆盖域的深化补全**——前14轮已覆盖 core/session/goal/fs/web/typert 等大域的核心纯逻辑面，本轮聚焦 context/storage/attachment/schedule/preset/acp/hooks 等域中**尚未复刻的纯逻辑文件**（很多 index.ts/invariant.ts 是 Cordis 宿主装配面已剔除）。

---

## 一、铁律（8条，多轮延续，违反即打回）

1. **文件互斥**：只改本路「可写文件」表内文件；新增一律 `.light`；**禁止修改任何既有 `src/` 模块**（本轮所有任务都是新增模块，不碰既有代码）。
2. **禁 Python 绕语言缺陷**：遇到光明语言缺陷，**必须**登记缺陷账（编号见下方预分配）+ 写 `examples/_repro_Lxxx.light` 最小复现（修复/绕法形态 rc=0），**禁止**用 Python 脚本或宿主面绕过去。
3. **docs 归路M**：对标清单/缺陷账/行为差异清单由路M统一回填，各任务**不要**改 docs/。
4. **中文回复**：所有交付报告、代码注释、测试用例名用中文。
5. **反跑脚本自定位**：反跑脚本统一放 `lightharness/` 根目录，`BASE = os.path.dirname(os.path.abspath(__file__))`（第14轮教训：不要假设脚本在上级目录）。
6. **命名预检查**：新增模块名前先确认不与既有 `src/*.light` 冲突（第14轮任务6教训）。
7. **测试断言数**：每个新测试至少 10 个断言用例，覆盖正常/边界/异常三态。
8. **反跑判据3项**：每个任务必须写反跑脚本，含 A=正常输出、B=边界、C=变异立红（改一行源码测试必须红，恢复后绿）三项判据。

---

## 二、已知缺陷绕法速查（L-001~L-120，写代码前必读）

### 高频陷阱（最近3轮新增，最容易踩）

| 编号 | 缺陷 | 绕法 |
|---|---|---|
| **L-119** | L0 单字别名是静默保留字（59字：若/则/否/是/段/出/导/遍/对/跳/过/试/捕/抛/终/返/配/己/自/设/为/从/当/空/长/首/末/余/父/常/并/与/且/或/的/在/到/现/例/步/断/接/承/宏/引/掷/跃/非/真），误用报错指向「缩进」完全误导 | 标识符完全避开上表，用 `配置结果`/`目标表` 等 |
| **L-120** | 标识符可被关键字序列完全切分时仍被切碎（`错误己`→`错误`+`己`，而 `错误甲`正常） | 标识符避开可被关键字（含单字别名）全覆盖切分的组合 |
| **L-116** | `序列化JSON` 保留字典插入序不排序，与 `字典==`（键序无关）语义不一致 | 结构相等用 `甲==乙`；若必须走JSON先键规范化（排序重建字典）再序列化 |
| **L-095** | HMAC-SHA256 不可手写（stdlib SHA256只返回hex无原始字节接口），编译器侧缺陷 | .light层维持自洽链路，对外对拍留待宿主接线 |
| **L-084** | 标识符含关键字字样被切碎（L-120同族） | 同L-120 |

### 第13轮缺陷（D1-D9，授权链域）

- **L-091**：匿名段落内联调用语法限制 → 用命名段落或展开内联
- **L-092**：if 内段落提升导致作用域异常 → if 块内声明的变量提到外层
- **L-093**：闭包 nonlocal 不支持 → 用字典/列表做可变容器传递
- **L-094**：抛出异常丢失属性 → 用消息文本承载错误信息
- **L-096**：空字典布尔判断异常 → 显式用 `字典长度(甲) == 0`
- **L-101**：`回调` 是保留关键字 → 用 `处理器`/`监听器`/`观察器`
- **L-102**：不支持默认参数值 → 可选参数由调用方传 `空`，函数内判空
- **L-103**：可选字典键访问抛 KeyError → 用 `取可选(对象,"键")` 或 `字典包含键` 先检查

### 早期缺陷（L-001~L-016，大部分已修复，L-009工程层残留）

- L-009：工程层缺陷（编译器构建流程），不影响.light代码编写

> 完整缺陷账见 `docs/功能对标/语言缺陷账.md`，写代码前遇到不确定的语法先查账。

---

## 三、6 路任务详细 Prompt

### 任务1：会话引用域（context/session-reference）

**对标编号**：#98 ｜ **缺陷编号预分配**：L-121~L-122 ｜ **差异编号**：R15-D1

**上游源**：`G:\github\deepseek-harness\packages\context\session-reference\src\`
- `config.ts` — 会话引用配置（默认值/校验）
- `projection.ts` — 会话引用投影（从会话数据提取引用视图）
- `serialization.ts` — 会话引用序列化/反序列化
- `spill.ts` — 会话引用溢出策略（大引用截断/占位）
- `types.ts` — 会话引用类型定义
- `uri.ts` — 会话引用 URI 解析/构造（`session://...` 格式）

**剔除清单**（不复刻）：
- `index.ts` — Cordis 服务装配面（依赖 `@deepseek-ai/cordis` Context/Service）
- `tests/` 目录及 fixtures（上游测试，不照搬）
- `file-reference-local/` — 宿主面（文件系统搜索）
- `tmux-context/` — 宿主面（tmux 终端集成）

**注意**：`context/file-reference` 已覆盖 #30（grammar.ts），`context/time-context` 已覆盖 #31，`context/agent-instructions` 已覆盖 #32——本任务**只做 session-reference**，不重复。

**光明模块**：`src/会话引用.light`（新增，确认不与既有模块冲突）

**可写文件**（文件互斥表）：
- `src/会话引用.light`
- `examples/test_会话引用.light`
- `_antirun_t1_会话引用.py`
- `_task1_会话引用_交付报告.md`

**只读参考**（可 import，不可修改）：
- `src/会话格式.light`（复用词汇表/事件形状）
- `src/无损JSON.light`（JSON 序列化参考）
- `src/溢出.light`（#27 已覆盖溢出策略，参考实现形态）

**测试要求**：
- 至少 12 个断言用例，覆盖：
  - URI 解析/构造（正常/畸形/round-trip）
  - 配置默认值与校验（合法/非法字段）
  - 序列化/反序列化 round-trip（正常/空字段/大字段）
  - 投影提取（从模拟会话数据提取引用视图）
  - 溢出策略（大引用截断/占位替换/字节预算）
  - 类型判别（各种引用类型的形状校验）

**反跑判据3项**：
- A=正常：URI 构造→解析 round-trip 输出与预期一致
- B=边界：空会话引用序列化→反序列化不丢字段
- C=变异：把 `uri.ts` 的 URI 分隔符改错，测试必须立红（恢复后绿）

---

### 任务2：存储核心与JSON域（storage + storage-json）

**对标编号**：#99 ｜ **缺陷编号预分配**：L-123~L-124 ｜ **差异编号**：R15-D2

**上游源**：
- `G:\github\deepseek-harness\packages\storage\storage\src\`
  - `backend.ts` — 存储后端接口（Backend 类型/读写删契约）
  - `error.ts` — 存储错误类型（NotFound/Conflict/IOError 等）
  - `registry.ts` — 存储后端注册表（按名注册/查找/默认后端）
- `G:\github\deepseek-harness\packages\storage\storage-json\src\`
  - `format.ts` — JSON 存储格式（行格式/版本头/记录形状）
  - `per-record-unit.ts` — 单记录存储单元（按记录读写/原子性语义）
  - `single-unit.ts` — 单文件存储单元（整个文件一个单元/批量读写）

**剔除清单**（不复刻）：
- `storage/index.ts` — Cordis 装配面
- `storage-json/atomic.ts` — 依赖 Node fs 原子写（宿主面）
- `storage-json/index.ts` — 装配面
- `storage-sqlite/` — 宿主面（SQLite 原生依赖）
- `storage-domain/` — **已覆盖 #59**，不重复
- `tests/contract.ts` — 上游测试契约，不照搬

**光明模块**：`src/存储核心.light`（新增）

**可写文件**：
- `src/存储核心.light`
- `examples/test_存储核心.light`
- `_antirun_t2_存储核心.py`
- `_task2_存储核心_交付报告.md`

**只读参考**：
- `src/存储域.light`（#59 已覆盖 storage-domain，参考域事件/不变量形态）
- `src/存储.light`（#14 早期覆盖，参考基础存储接口）
- `src/无损JSON.light`（JSON 序列化参考）
- `src/会话持久化JSONL.light`（#94 第14轮覆盖，参考 JSONL 行格式实现）

**测试要求**：
- 至少 14 个断言用例，覆盖：
  - 后端接口契约（读/写/删/存在性检查的形状）
  - 错误类型判别（NotFound/Conflict/IOError 的消息与码）
  - 注册表（注册/查找/默认后端/重复注册处理）
  - JSON 格式（行格式解析/版本头校验/记录形状 round-trip）
  - 单记录单元（按记录读写/不存在记录/覆盖写）
  - 单文件单元（批量读写/空文件/部分写入）
  - 内存后端实现（用字典模拟 Backend 接口，验证契约一致性）

**反跑判据3项**：
- A=正常：内存后端写入→读取 round-trip 数据一致
- B=边界：空存储单元批量读取返回空列表不报错
- C=变异：把 `error.ts` 的 NotFound 消息改错，测试必须立红（恢复后绿）

---

### 任务3：附件与反馈域（attachment + feedback）

**对标编号**：#100 ｜ **缺陷编号预分配**：L-125~L-126 ｜ **差异编号**：R15-D3

**上游源**：
- `G:\github\deepseek-harness\packages\attachment\attachment\src\`
  - `admission.ts` — 附件准入校验（类型白名单/大小限制/危险类型拦截）
  - `brand.ts` — 附件品牌（附件类型名称/图标/分类的纯数据）
  - `error.ts` — 附件错误类型（Rejected/TooLarge/Unsupported 等）
  - `request-projection.ts` — 附件请求投影（从请求提取附件元数据视图）
  - `types.ts` — 附件类型定义（Attachment/AttachmentRequest/AdmissionResult 等）
- `G:\github\deepseek-harness\packages\feedback\command-feedback\src\`
  - `index.ts` — 命令反馈核心（反馈收集/评分/汇总）
  - `types.ts` — 命令反馈类型
- `G:\github\deepseek-harness\packages\feedback\message-feedback\src\`
  - `index.ts` — 消息反馈核心（消息级反馈/点赞/点踩/注释）
  - `types.ts` — 消息反馈类型

**剔除清单**（不复刻）：
- `attachment/index.ts` — 装配面
- `attachment-local/` — 宿主面（压缩/编码/文件存储/图片处理/规范化，依赖 Node fs 和 sharp）
- `tests/` — 上游测试不照搬

**光明模块**：`src/附件反馈.light`（新增）

**可写文件**：
- `src/附件反馈.light`
- `examples/test_附件反馈.light`
- `_antirun_t3_附件反馈.py`
- `_task3_附件反馈_交付报告.md`

**只读参考**：
- `src/安全策略.light`（#61 已覆盖 credentials 脱敏，参考准入校验形态）
- `src/会话格式.light`（复用事件形状）
- `src/交互命令.light`（#73 已覆盖 interaction，参考命令处理形态）

**测试要求**：
- 至少 14 个断言用例，覆盖：
  - 附件准入校验（合法类型通过/危险类型拦截/超限拒绝/边界大小）
  - 附件品牌（类型名称查找/分类/未知类型默认值）
  - 附件错误（Rejected/TooLarge/Unsupported 的消息与码）
  - 请求投影（从模拟请求提取附件元数据/多附件/空附件）
  - 类型判别（Attachment/AttachmentRequest/AdmissionResult 形状校验）
  - 命令反馈（收集/评分1-5/汇总平均/空反馈）
  - 消息反馈（点赞/点踩/注释/切换状态/计数）

**反跑判据3项**：
- A=正常：附件准入校验→合法类型通过，返回 AdmissionResult 通过
- B=边界：0 字节附件准入校验不崩溃（按配置决定通过或拒绝）
- C=变异：把 `admission.ts` 的危险类型黑名单改错，测试必须立红（恢复后绿）

---

### 任务4：调度深化与任务守卫域（schedule深化 + jobs核心 + guard）

**对标编号**：#101 ｜ **缺陷编号预分配**：L-127~L-128 ｜ **差异编号**：R15-D4

**上游源**：
- `G:\github\deepseek-harness\packages\schedule\schedule\src\`
  - `projection.ts` — 调度投影（从调度记录提取视图/下次运行时间计算）
  - `transaction.ts` — 调度事务（批量更新/原子性语义/冲突检测）
  - `types.ts` — 调度类型定义（ScheduleRecord/AfterSchedule/AtSchedule/EverySchedule 等）
- `G:\github\deepseek-harness\packages\jobs\jobs\src\`
  - `brand.ts` — 任务品牌（任务状态名称/图标/分类纯数据）
  - `types.ts` — 任务类型定义（JobId/JobSnapshot/JobStart/JobRead/监听器类型等）
- `G:\github\deepseek-harness\packages\guard\repeat-tool-reminder\src\`
  - `index.ts` — 重复工具提醒（检测连续重复工具调用/生成提醒文案/冷却时间）
- `G:\github\deepseek-harness\packages\guard\timeout-policy\src\`
  - `index.ts` — 超时策略（工具超时时间计算/默认值/按工具类型差异化/超时文案）

**剔除清单**（不复刻）：
- `schedule/domain.ts` — **已覆盖 #29**（RFC3339编解码/调度记录折叠状态机），不重复
- `schedule/tools.ts` — 宿主面（`defineTool` 依赖 Cordis Context/Agent）
- `schedule/index.ts` — 装配面
- `schedule/invariant.ts` — Cordis 不变量安装器（宿主面）
- `schedule/persistence.ts` — 依赖存储后端（宿主面）
- `schedule/runtime.ts` — 依赖调度器运行时（宿主面）
- `schedule/client.ts` — 纯类型重导出（`export type * from './types'`），内容并入 types
- `jobs/index.ts` — Cordis Service 定义（宿主面）
- `jobs/invariant.ts` — Cordis 不变量安装器（宿主面）
- `jobs-local/` — **已覆盖 #53**，不重复
- `tool-jobs/` — **已覆盖 #53**，不重复

**光明模块**：`src/调度深化.light`（新增）

**可写文件**：
- `src/调度深化.light`
- `examples/test_调度深化.light`
- `_antirun_t4_调度深化.py`
- `_task4_调度深化_交付报告.md`

**只读参考**：
- `src/定时.light`（#29 已覆盖 schedule/domain.ts，参考调度记录/状态机形态）
- `src/任务系统.light`（#53 已覆盖 jobs-local/tool-jobs，参考任务状态机形态）
- `src/会话格式.light`（复用事件形状）

**测试要求**：
- 至少 14 个断言用例，覆盖：
  - 调度投影（从记录提取视图/计算下次运行时间/after/at/every 三种调度）
  - 调度事务（批量更新/空事务/冲突检测/部分失败处理）
  - 调度类型（ScheduleRecord 形状校验/三种调度类型判别/非法字段）
  - 任务品牌（状态名称查找/分类/未知状态默认值）
  - 任务类型（JobId 格式/JobSnapshot 形状/监听器类型判别）
  - 重复工具提醒（连续3次相同调用触发提醒/冷却期内不重复/不同工具不触发）
  - 超时策略（默认超时/按工具类型差异化/超时文案生成/边界值）

**反跑判据3项**：
- A=正常：every 调度投影计算下次运行时间与预期一致
- B=边界：空调度事务批量更新返回空结果不报错
- C=变异：把 `timeout-policy` 的默认超时值改错，测试必须立红（恢复后绿）

---

### 任务5：预设深化与设置技能角色域（preset深化 + settings + skill + persona）

**对标编号**：#102 ｜ **缺陷编号预分配**：L-129 ｜ **差异编号**：R15-D5

**上游源**：
- `G:\github\deepseek-harness\packages\preset\agent-presets\src\`
  - `composition-inventory.ts` — 预设组合清单（列出预设包含的插件/工具/提示段/投影单元）
  - `display.ts` — 预设显示（预设名称/描述/状态的显示格式化）
  - `types.ts` — 预设类型定义（PresetSpec/PresetMount/PresetMetadata 等）
- `G:\github\deepseek-harness\packages\preset\persona\src\`
  - `index.ts` — 角色定义（Persona 类型/角色名称/系统提示前缀/角色切换）
- `G:\github\deepseek-harness\packages\settings\settings\src\`
  - `redact.ts` — 设置脱敏（敏感设置键的脱敏/白名单/黑名单/脱敏文案）
  - `types.ts` — 设置类型定义（Settings/SettingsKey/SettingsValue 等）
- `G:\github\deepseek-harness\packages\skill\skill\src\`
  - `index.ts` — 技能注册（技能定义/技能注册表/按名查找/技能启用状态）
- `G:\github\deepseek-harness\packages\skill\skill-badge\src\`
  - `index.ts` — 技能徽章（技能徽章类型/徽章渲染/徽章状态）

**剔除清单**（不复刻）：
- `agent-presets/authoring.ts` — **已覆盖 #72**
- `agent-presets/discovery.ts` — **已覆盖 #72**
- `agent-presets/metadata.ts` — **已覆盖 #72**
- `agent-presets/mount.ts` — **已覆盖 #72**
- `agent-presets/preset.ts` — **已覆盖 #72**
- `agent-presets/session.ts` — **已覆盖 #72**
- `agent-presets/specifier.ts` — **已覆盖 #72**
- `agent-presets/index.ts` — 装配面（#72 标注为装配层未复刻）
- `agent-presets/invariant.ts` — Cordis 不变量安装器（宿主面）
- `settings/index.ts` — 装配面
- `settings/invariant.ts` — Cordis 不变量安装器（宿主面）
- `settings-file/` — 宿主面（文件持久化）
- `skill-filesystem/` — 宿主面（文件系统技能 watcher）
- `tool-skill/` — 宿主面（工具定义）

**光明模块**：`src/预设深化.light`（新增）

**可写文件**：
- `src/预设深化.light`
- `examples/test_预设深化.light`
- `_antirun_t5_预设深化.py`
- `_task5_预设深化_交付报告.md`

**只读参考**：
- `src/预设.light`（#72 已覆盖 agent-presets 7文件，参考预设 specifier/mount 形态）
- `src/安全策略.light`（#61 已覆盖 credentials 脱敏，参考 redact 形态）
- `src/交互命令.light`（#73 已覆盖 interaction/permission-presets，参考权限预设形态）

**测试要求**：
- 至少 14 个断言用例，覆盖：
  - 预设组合清单（列出预设包含的插件/工具/提示段/空预设/未知组件）
  - 预设显示（名称格式化/状态显示/描述截断/空描述）
  - 预设类型（PresetSpec 形状校验/PresetMount 状态判别/非法字段）
  - 角色定义（Persona 类型/角色名称查找/系统提示前缀/角色切换/未知角色）
  - 设置脱敏（敏感键脱敏/白名单通过/黑名单拦截/脱敏文案/空设置）
  - 设置类型（SettingsKey 判别/SettingsValue 类型校验/非法值）
  - 技能注册（注册/查找/启用状态/重复注册/未知技能）
  - 技能徽章（徽章类型/渲染/状态切换/空徽章）

**反跑判据3项**：
- A=正常：预设组合清单列出指定预设的所有组件与预期一致
- B=边界：空预设组合清单返回空列表不报错
- C=变异：把 `redact.ts` 的敏感键黑名单改错，测试必须立红（恢复后绿）

---

### 任务6：协议深化域——ACP + 钩子（acp深化 + hook-protocol深化）

**对标编号**：#103 ｜ **缺陷编号预分配**：L-130 ｜ **差异编号**：R15-D6

**上游源**：
- `G:\github\deepseek-harness\packages\acp\acp\src\`
  - `mcp.ts` — ACP 上的 MCP 协议消息（MCP 请求/响应/通知形状/序列化）
  - `model-control.ts` — ACP 模型控制消息（模型切换/参数调整/控制指令形状）
  - `updates.ts` — ACP 语义更新消息（提交更新/更新批次/更新确认形状）
- `G:\github\deepseek-harness\packages\hooks\hook-protocol\src\`
  - `detached.ts` — 分离钩子运行（detached run 的静默期/完成检测/结果回收）
  - `events.ts` — 钩子事件（钩子执行事件形状/事件序列化/事件判别）
  - `runner.ts` — 钩子运行器（钩子命令执行/输出解码/运行结果形状）
  - `types.ts` — 钩子协议类型（CommandHook/HookDialect/HookOutput/MatcherGroup 等）

**剔除清单**（不复刻）：
- `acp/content.ts` — **已覆盖 #37**（ACP 内容准入/投影），不重复
- `acp/codec.ts` — **已覆盖 #40**（ACP 转码/终止原因词表映射），不重复
- `acp/index.ts` — Cordis 服务装配面（依赖 Context/node:buffer/node:crypto/node:fs）
- `acp/session.ts` — ACP 会话管理（依赖 Cordis 会话/状态，宿主面）
- `hook-protocol/matcher.ts` — **已覆盖 #56**（matcher 甄别判定/正则编译），不重复
- `hook-protocol/codec.ts` — **已覆盖 #56**（钩子编解码），不重复
- `hook-protocol/merge.ts` — **已覆盖 #56**（限制性结果合并），不重复
- `hook-protocol/invariant.ts` — Cordis 不变量安装器（宿主面）
- `hook-protocol/index.ts` — 纯类型重导出（`export type {...} from './types'`），内容并入 types
- `hooks-claude-code/` — 宿主面（Claude Code 桥接）
- `hooks-codex/` — 宿主面（Codex 桥接）
- `runtime-diagnostics/invariants/` — Cordis 不变量安装器（宿主面，暂不纳入本轮）

**光明模块**：`src/协议深化.light`（新增）

**可写文件**：
- `src/协议深化.light`
- `examples/test_协议深化.light`
- `_antirun_t6_协议深化.py`
- `_task6_协议深化_交付报告.md`

**只读参考**：
- `src/acp内容.light`（#37 已覆盖，参考 ACP 内容准入形态）
- `src/acp转码.light`（#40 已覆盖，参考 ACP 转码/词表映射形态）
- `src/钩子协议.light`（#56 已覆盖 matcher/codec/merge，参考钩子协议形态）
- `src/钩子.light`（#10 已覆盖 hooks 事件机制，参考事件总线形态）
- `src/JSONRPC.light`（参考 JSON-RPC 消息形状，ACP 基于 JSON-RPC）

**测试要求**：
- 至少 14 个断言用例，覆盖：
  - ACP MCP 消息（请求/响应/通知形状/序列化 round-trip/非法消息判别）
  - ACP 模型控制消息（模型切换/参数调整/控制指令形状/未知控制类型）
  - ACP 语义更新消息（提交更新/更新批次/更新确认/空批次/部分失败）
  - 分离钩子运行（静默期检测/完成检测/结果回收/超时/空运行）
  - 钩子事件（执行事件形状/序列化/事件判别/未知事件类型）
  - 钩子运行器（命令执行结果形状/输出解码/成功/失败/空输出）
  - 钩子协议类型（CommandHook 形状/HookDialect 判别/HookOutput 校验/非法字段）

**反跑判据3项**：
- A=正常：ACP MCP 请求序列化→反序列化 round-trip 与预期一致
- B=边界：空 ACP 更新批次序列化不丢字段
- C=变异：把 `runner.ts` 的成功状态码改错，测试必须立红（恢复后绿）

---

## 四、路M 收口职责（第7路，由路M执行）

1. **查收6份交付报告**：核对每路的可写文件表、测试断言数、反跑3项判据、缺陷登记。
2. **6反跑全 ALL OK**：逐个运行6个反跑脚本，确认 A/B/C 三项判据全过（C判据必须真变异立红，恢复后绿——第13轮教训：反跑输出不能截断核验）。
3. **6测试单独跑全 PASS**：`python 运行.py examples/test_xxx.light` 每个 rc=0。
4. **文件互斥零越界检查**：确认6路均只新增文件，未修改任何既有 `src/` 模块。
5. **分路提交6个commit**：每路一个 commit，message 格式 `任务N(域): 简述（缺陷编号；反跑3/3）`。
6. **docs三件回填**：
   - 对标清单追加 #98~#103
   - 缺陷账追加本轮新缺陷（L-121~L-130，无新缺陷则编号空缺顺延）
   - 行为差异追加 R15-D1~D6
7. **全量CI**：`$env:LIGHT_MERGE="G:\dswork\duan-light-merge\light-merge"; python scripts/ci_test.py`，期望 pytest 282+ passed（276基线 + 6新测试 + N repro），smoke 5/5。
8. **收口提交 + 收口说明**：写 `_taskM_第15轮收口说明.md`，含6路交付摘要/CI结果/缺陷留档/行为差异/未移植项/教训。

---

## 五、编号预分配

| 类别 | 编号范围 | 说明 |
|---|---|---|
| 对标清单 | #98~#103 | 6路各1条 |
| 语言缺陷账 | L-121~L-130 | 预留10个，无新缺陷则空缺顺延（参照第14轮 L-104~L-115 空缺先例） |
| 行为差异 | R15-D1~D6 | 6路各1条 |

---

## 六、CI 期望

- **pytest**：282+ passed（第14轮基线 276 + 6 新测试 + N repro 文件）
- **smoke**：5/5 通过（test_会话/test_代理/test_工具/test_消息/test_流）
- **已知 flaky**：test_审批.light 在全量运行时偶发 rc=1（第14轮诊断，疑似系统资源竞争，单独跑全绿），若出现则复跑确认，不阻塞收口

---

## 七、分发说明

- 6 路任务**互相独立、无共享状态、无顺序依赖**，可并行分发。
- 每路执行者拿到对应「任务N」的完整 prompt（含上游源/剔除清单/可写文件/只读参考/测试要求/反跑判据）即可开工。
- 路M（第7路）在6路全部交付后执行收口。
- 执行者遇到语言缺陷**必须**登记（用预分配编号）+ 写 repro，**禁止**用 Python 绕。
- 反跑脚本统一放 `lightharness/` 根目录，路径自定位。
