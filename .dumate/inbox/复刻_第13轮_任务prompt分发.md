# 复刻 第13轮：任务分发 prompt（6 并行 + 路M）

> 日期：2026-09-13 ｜ 上游只读：`G:\github\deepseek-harness` @ `a305303422`（0.1.5-rc.2；HEAD 为 FreeBSD fork `9d9035b7c1`，其 +2 commit 仅涉 flock/rc.d 平台改动，不影响本轮 6 域）
> 编译器：`G:\dswork\duan-light-merge\light-merge`（环境变量 `LIGHT_MERGE`）；测试环境：`G:\dswork\duan-light-merge\lightharness`（branch main，第12轮收口 commit `69190bc` + web 三件套 `381a6cf` 已合入）
> 本轮定位：**第12轮遗留（工具注册/词汇注册）不动**；6 路均为「上游未复刻纯逻辑包」——fs 编辑器 / shell 环境 / goal 轮驱动 / session 日志增量 / 授权链 / GitHub webhook

---

## 〇、铁律（全部任务共用，违反即不合格）

1. **只改文件互斥表内文件**。新增一律：`src/<新模块>.light` + `examples/test_<新模块>.light` + `_antirun_<缩写>.py` + `_taskN_<域>_交付报告.md`。
2. **只读禁区**：`docs/功能对标/`（路M 统一回填）；`src/` 既有模块（互斥表点名的可读不可写）；`stdlib/`、`scripts/`、`运行.py`、`tests/` 一律不动。
3. **禁 Python 绕语言缺陷**：光明语言限制一律用光明语法绕法；新发现的缺陷登记到交付报告 §语言差异，**建议编号从 L-091 起**（L-085~L-090 已被第12轮占用），并写最小复现 `examples/_repro_L0xx.light`（绕法形态 rc=0）。
4. **每路只跑本路测试**（并行避免互扰）：`cd G:\dswork\duan-light-merge\lightharness; $env:LIGHT_MERGE="G:\dswork\duan-light-merge\light-merge"; python 运行.py examples/test_<模块>.light`，必须打印「test_<模块> PASS」且 rc=0。
5. **反跑判据**：`_antirun_<缩写>.py` 对目标 `src/` 模块做字节级备份→变异→跑测试→断言 rc=1（真红）→恢复→断言 rc=0（真绿）且逐字节一致；输出 `ALL OK`。判据项数见各任务书（要求 ≥3 项）。
6. **交付报告** `_taskN_<域>_交付报告.md` 含：上游对应表（文件/行号/核心语义）｜实现要点｜测试验证（PASS 输出）｜反跑结果（N/N）｜未移植项（宿主面登记）｜语言差异（新缺陷+建议编号）｜移交清单。
7. **中文回复**；工作区非本轮产物（README.md、examples/运行Web服务器.light、`_taskM_*`/`_inspect_*`/`_verify_*` 工具脚本）**不碰不提交**。
8. **已知语言缺陷绕法速查**（第12轮 L-085~L-090，本轮回规避）：`1e` 指数浮点字面量 → `解析JSON("...")`；正则花括号量词 `{n,m}` → 无花括号等价写法；`当前目录` 是内置映射名（发射为 `当前目录()`）→ 不用该名；`捕获 错误 as 变量` 不支持 → `尝试/捕获 错误:` + 布尔标志；字符串 `{标识符}` 触发插值 → 测试文本用 `序列化JSON(...)` 构造；字符串原语用编译器内置裸名（不 import 字符串 .light 模块）。另：L-030 空 字子串边界、L-043 禁止行内布尔链（`且/或` 拆顺序 `如果`）、L-084 `为` 字词法切碎（裸标识符避开 `为` 字）、L-089 双花括号 `{{...}}` 字面量安全。

---

## 路M（收口，本轮并行分发 6 路）

1. 查收 6 份交付报告；`git status` 核对文件互斥零越界。
2. 分路提交（commit 形如「任务N(<域>): <简述>(反跑N/N)」）。
3. 全量 CI：`python scripts/ci_test.py`，期望 **259+6 passed / 0 failed**（新增 6 个 test_ 文件）。
4. 回填：对标清单新增 **#86~#91**（映射：86=str-replace-editor / 87=shell-env / 88=goal-round-driver / 89=session-log-deepseek / 90=authorization / 91=webhook-github）；语言缺陷账 L-091 起登记（若有）；行为差异清单新增 R13-D 段。
5. 写 `_taskM_第13轮收口说明.md`；清理探针；present_files 交付。

---

## 任务1（fs 编辑器域）：tool-str-replace-editor → `src/字符串替换编辑器.light`

**上游源**：`packages/fs/tool-str-replace-editor/src/index.ts`（531 行）。对标卡：#60 已点名但**未实现**（#60 光明模块仅有 路径规则.light），本轮收口。**命令集=view/create/str_replace/insert 四种（无 undo）**，命令参数枚举 `['view','create','str_replace','insert']`。

**复刻范围（核心语义）**：
1. **路径解析** `解析目标(路径)`：空串（trim 后）抛 `路径必须为非空字符串`；非绝对路径抛 `路径 X 不是绝对路径，应以 / 开头。是不是想写 /X？`（对齐上游文案要点）。光明侧用 文件系统 内建或入参解析器。
2. **视图命令 view**：`格式化文件视图(路径, 内容, 上限, 视图范围?)` —— 总行数提示 `这里有 X 行`；`view_range=[起, 止]` 校验（必须两整数/起 ∈ [1, 总行数]/止 ≤ 总行数/止=-1 到文尾/止 ≥ 起，错误文案对齐）；行号 `str(行号).rjust(6, " ")` + 两空格 + 行文本；超限截断附 `TRUNCATED_MESSAGE`（`<response clipped><NOTE>...`）。
3. **目录视图**：递归 2 层（排除隐藏 `.` 开头、`node_modules`、`__pycache__`），行格式 `d|f\t显示路径`，按路径码点排序（光明用 字符串比较），头部 `Here're the files...`。
4. **create 命令**：`file_text` 必填（缺省/空串报错文案）；目标已存在抛 `文件已存在: X。create 不能覆盖`；写内容返回 `新文件创建成功: X`。
5. **str_replace 命令**：`new_str` 为 `无` 抛 `参数 new_str 必须省略或为字符串`；`old_str` 必填且非空；`查找全部偏移`（indexOf 循环推进）；0 命中抛 `未执行替换，old_str 未在 X 中原样出现`（FS_EDIT_NOT_FOUND）；多命中抛 `未执行替换，old_str 在行 [n1, n2] 出现多次，请确保唯一`（FS_AMBIGUOUS_EDIT，行号用 行号映射）；单命中 → `前段+新+后段` 拼接写入，返回 `文件 X 已编辑成功`。
6. **insert 命令**：`insert_line` 必填；`new_str` 必填；行数校验 `insert_line ∈ [0, 总行数]`（错误文案对齐）；按行插入（split('\n') 三段拼接后 join 回 `\n`）。
7. **命令门禁与参数规则**：参数名 `command/path/file_text/insert_line/new_str/old_str`（对齐上游 schema，`view_range` 为 view 专属可选）；`requiredForCommand` 语义——缺省抛 `参数 X 是 Y 命令必填`、`allowEmpty=假` 时空串抛 `参数 X 是 Y 命令为空`；`file_text` 缺省则 create 抛（content 来自 requiredForCommand）；`new_str` 为 `无` 时 str_replace 抛（见 §5）。
8. **宿主面剔除**：`ctx.fs / cordis / sandboxPolicy / waterfall / emit / defineTool 注册` 全部入参化——文件内容、存在性、目录条目、版本号由测试/宿主注入；本模块只做「纯字符串/列表变换 + 校验 + 文案 + 命令呈现形状（card/title/kind/diffs/locations，供宿主渲染）」。

**测试要求**（`examples/test_字符串替换编辑器.light`，≥10 断言）：视图行号渲染、view_range 各校验错误、目录 2 层树过滤与排序、create 已存在拒绝、str_replace 唯一命中/未找到/歧义行号、insert 边界、命令参数枚举与必填门禁、截断尾注、命令呈现形状。测试用 序列化JSON 构造文本（L-089 绕法）。

**反跑判据 3 项**：A 视图行号 6 位右对齐改 4 位→红；B str_replace 多命中不再抛歧义→红；C insert_line 上界放开（>总行数通过）→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/路径规则.light`（复用）；禁区=其余。

---

## 任务2（shell 环境域）：shell-env → `src/外壳环境.light`

**上游源**：`packages/shell/shell-env/src/index.ts`（181 行）+ `packages/shell/shell/src/types.ts`（DSH_ENV_PREFIX 引自 `packages/subprocess/subprocess/src/types.ts` = `'DSH_'`）+ `packages/util/home-paths/src/index.ts`（DSH_HOME_ENV=`'DSH_HOME'`、resolveDshHome 默认 `~/.dsh`）。

**复刻范围（核心语义）**：
1. **保留键**：`DSH_HOME`、`DSH_SHELL`、`DSH_SESSION_ID` 三键保留（contributor 禁拥）。
2. **键名校验**：`DSH_` 前缀 + 后缀正则 `^[A-Z][A-Z0-9_]*$`（光明用 ASCII 码点手写判定或正则工具）。
3. **注册表 register(贡献者)**：`名字` 非空唯一；`变量` 表逐键校验（前缀/后缀/保留键/描述非空/键未被他人拥用），错误文案对齐（`bash env contributor "X" ...` 中文化）；登记 contributors + keyOwners；返回注销函数（同步删除）。
4. **快照 collect(执行上下文)**：内置 `DSH_HOME=家目录`、`DSH_SHELL='1'`；执行上下文带 会话id 时补 `DSH_SESSION_ID=会话id`；遍历贡献者（按名字 localeCompare 排序）：`resolve(执行)` 返回表逐键校验——未声明键抛 `返回了未声明键`、非字符串值抛 `返回了非字符串值`；结果按键排序后**冻结**（光明 `冻结` 原语 L-083 已修，可用；或重绑新字典模拟不可变）。
5. **枚举 list()**：贡献者声明的扁平化（贡献者名/描述/键），按键排序；内置键不在 list（TODO 注释同步）。
6. **家目录解析**：`解析家目录(配置值?, 环境表?)`：显式配置 > `DSH_HOME` 环境变量 > `~/.dsh`（光明用 主目录路径.light 或入参）。
7. **宿主面剔除**：`ctx.effect / Cordis Service / ToolExecution` 全部入参化——注册/注销为普通段落；执行上下文为入参字典。

**测试要求**（`examples/test_外壳环境.light`，≥10 断言）：保留键拒绝、后缀非法键拒绝、描述空拒绝、重复贡献者名拒绝、键重复归属拒绝、collect 内置三键、贡献者排序、未声明键/非字符串拒绝、list 排序、注销后消失。

**反跑判据 3 项**：A 保留键 DSH_HOME 允许注册→红；B collect 去掉键排序→红；C 注销函数不删除→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/主目录路径.light`、`src/工具_bash.light`（可读参考）；禁区=其余。

---

## 任务3（goal 轮驱动域）：goal-round-driver（纯逻辑面）→ `src/目标轮驱动.light`

**上游源**：`packages/goal/goal-round-driver/src/index.ts`（525 行，宿主注入 agents/goals/sessions）+ `prompt.ts`（80 行纯渲染）。**只复刻纯逻辑面**（判定族 + 轮次决策状态机 + prompt 渲染），驱动循环/agent.followup/inbox/waterfall 剔除登记未移植。

**复刻范围（核心语义）**：
1. **来源判定**：`是目标轮来源(来源)` = `来源.kind == "goal" 且 来源.round > 0`；`同轮(来源, 轮身份)` = goalId/revision/round 三者相等；`同排队记录` = 同轮 + 内容逐项相等（光明 用 序列化JSON 比较或逐字段）。
2. **目标引用**：`取目标引用(目标视图)` = `{id, revision}`（对齐 GoalRef）。
3. **就绪判定**：`可驱动(状态)` = 非停止 且 代理空闲 且 无竞争排队（光明用入参布尔集）；`检查点后就绪` = 可驱动 且 无需检查点。
4. **轮次推进决策 `推进一轮(状态)`**（核心状态机）：
   - 有待处理尝试 → 清空尝试、置 需检查点+已请求、返回（让位）。
   - 当前目标未激活（phase ≠ active 或 activation ≠ armed）→ 返回。
   - `roundsStarted >= maxGoalRounds` → 返回 阻止块 `{码: "round-limit", 消息: "目标达到配置上限 X 轮"}`。
   - 否则 `轮 = roundsStarted + 1`，`内容 = 渲染目标轮提示(目标, 轮)`，构造用户消息（来源 `{kind:"goal", goalId, revision, round}`），建立 排队尝试 `{goalId, revision, round, 消息id, 内容, 阶段:"queued", 已取消:假, 已过期:假}`。
   - 入队失败（宿主抛错）→ 清空尝试 + 若目标仍活跃则返回 阻止块 `{码:"queue-failed", ...}`。
5. **提示渲染 `渲染目标轮提示(目标, 轮)`**（对齐 prompt.ts）：`<goal_round>\nObjective: <序列化JSON(目标.客观)> \nRound: 轮/目标.最大轮数\n\n<固定指令三段：以当前工作区/工具结果/持久状态为准、先取证再宣称完成、未完成保持目标活跃>\n</goal_round>`，返回单块 `{type:"text", text}`。
6. **目标视图形状**：先读 `src/目标折叠.light` 投影字段（objective/maxGoalRounds/roundsStarted/phase/activation 对应中文键），对齐后把「当前目标」作入参（不接 目标折叠.light 内部）。
7. **宿主面剔除**：agents/sessions/fiber/logger/effect 全部入参化或剔除，登记未移植（agent 生命周期接线/checkpoint flush/竞争事件）。

**测试要求**（`examples/test_目标轮驱动.light`，≥12 断言）：来源判定（正轮/零轮/非 goal）、同轮比较、就绪栅栏（停止/忙/竞争）、轮次推进（新轮号=roundsStarted+1、round-limit 阻止、未激活返回）、queue-failed 阻止、提示渲染（objective 序列化/轮次/最大轮数/固定文本三段）。

**反跑判据 3 项**：A 轮次号改 roundsStarted→红；B round-limit 不再阻止→红；C 提示缺少 `</goal_round>` 收尾→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/目标折叠.light`（投影字段参考）；禁区=其余。

---

## 任务4（session 日志域）：session-log-deepseek → `src/会话日志增量.light`

**上游源**：`packages/session/session-log-deepseek/src/index.ts`（328 行）+ `types.ts`。宿主注入 sessions/deepseekLlmApiExtensions，**只复刻线格式翻译 + 水印折叠纯逻辑**，请求接线剔除登记。

**复刻范围（核心语义）**：
1. **线头翻译 `线头(会话头)`**：`{version, id: 字符串(id), createdAt}` + 可选键（cwd?/parentSession?(字符串化)/isSeeded→seedLength=继承事件数/origin?/delegationDepth?/agentPreset?——仅当存在时落键，对齐上游展开）。
2. **线事件翻译 `线事件(事件)`**：公共 `{seq, time, data}` + ignorable?；类型分发——`system/message|user/message|tool/result` 加 `surfaceOp`（线化）+ `sourceEventSeqs`?（数字列表）；`assistant/message` 加 surfaceOp；未知可忽略不透明记录保留 `{type, ignorable:真, surfaceOp?, sourceEventSeqs?}`；其余 `{type}` 原样。
3. **线化操作 `线化操作(操作)`**：`append` 原样；replace 展开 `{op:"replace", startSeq, endSeq}`。
4. **接受水印 `接受至(会话视图)`**（核心）：增量扫描事件表（起点=上次扫描数），只认 `session-log-deepseek/delivery-accepted` 事件；校验：`sessionFormatVersion` 安全整数且 ≥0 且 == 会话头版本（否则抛 畸形）；`throughSeq` 可构造序号且 < 事件自身 seq；`sessionId` 非空字符串且 == 会话 id；水印取最大通过序号；返回 `-1`（未接受前）；扫描点/水印缓存在会话视图外（光明用入参的 记忆表 字典）。
5. **增量贡献构造 `构造增量(会话视图, 接受点)`**：`afterSeq=接受点`、`throughSeq=末事件序号`、`events=接受点+1 起的 线事件 列表`；无事件返回 空。
6. **接受确认 `确认接受(会话视图, 通过序号)`**：追加 `{type:"session-log-deepseek/delivery-accepted", data:{sessionId, sessionFormatVersion, throughSeq}}`（事件追加由宿主/测试注入，光明会话结构以入参 事件表 为准——**不得改 src/会话.light 或 src/会话格式.light**）。
7. **会话视图入参**：光明侧不 import 会话 模块，测试以「事件表 + 头 + 记忆表」字典作入参（纯函数化）。

**测试要求**（`examples/test_会话日志增量.light`，≥12 断言）：线头可选键条件携带、线事件类型分发（三种带 surfaceOp/未知不透明保留）、replace 展开、水印折叠（多接受事件取最大/版本不符跳过/会话不符跳过/畸形抛错）、增量后缀起点、确认后水印推进、-1 初始。

**反跑判据 3 项**：A 水印不再取最大（改取首个）→红；B 线头 seedLength 无条件落键→红；C 畸形接受事件不再抛错→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/会话格式.light`、`src/会话.light`（形状参考）；禁区=其余。

---

## 任务5（授权链域）：authorization → `src/授权链.light`

**上游源**：`packages/credentials/authorization/src/index.ts`（534 行）+ `types.ts` + `invariant.ts`。宿主注入 credentials 事件，**只复刻注册表 + 校验链 + 单键单尝试状态机**；凭据读写/AbortSignal/事件广播剔除登记。

**复刻范围（核心语义）**：
1. **类型形状**：`方法 {id, label}`；`通知 {message, url?, code?}`；`选项 {id, label, description?}`；`提问 = {signal?} & (文本|机密|选择 三种)`；`状态 = authorized|cancelled`；`结算 = 状态|failed`；`结果 {status}`；`条目 {key, label, methods, inFlight}`（对齐 types.ts）。
2. **错误族**：`授权错误(消息, 码)`（对齐 AuthorizationError）；`授权被拒错误`（码 `DECLINED`，默认文案 `授权提示被拒绝`）。
3. **注册表 `注册流程(流程)`**：`key` 已注册抛 `授权流程 "X" 已注册`（码 DUPLICATE_FLOW）；登记 + 返回注销函数（注销时若该键在跑则置中止标记）。
4. **入口 `开始(请求)`**（核心校验链，顺序不可乱）：
   - 无流程 → 抛 NO_FLOW `没有为 "X" 注册授权流程`；
   - `方法 = 请求.方法 ?? 流程.方法[0].id`；方法不在流程方法集 → 抛 UNKNOWN_METHOD；
   - 键正在跑 → 抛 ALREADY_IN_FLIGHT `"X" 的授权尝试已在运行`；
   - 请求已中止 → 返回 `{status:"cancelled"}`（不占槽）；
   - 占槽 → 运行尝试 → 结算（authorized/cancelled/failed）→ 释放槽 → 广播结算（监听器失败隔离：同步抛/异步拒 记日志不改结果；码 INVARIANT 的失败在全部监听器跑完后重抛）。
5. **尝试 `尝试(流程, 方法, 中止标志, 交互)`**：中止竞态（中止→withdrawn）；观察 凭据已更新（提交标记）；`notify` 包装（交互抛错吞掉，fire-and-forget 语义）；`prompt` 包装（交互抛 被拒错误 → 记 已拒绝）；`流程.运行(会话)` 抛错分类——被拒错误→cancelled，其他→failed（向上抛）；运行返回后校验提交标记，未提交 → 抛 NOT_COMMITTED；提交且未中止 → authorized。
6. **中止模拟**：光明无 AbortSignal——请求的 中止 用入参布尔标志 + 回调（流程内检查）；取消 `取消(键)` 置中止标记。
7. **宿主面剔除**：`ctx.effect/events.dispatch/credentials 事件` 入参化——凭据更新观察用「提交回调表」模拟；广播用监听器表。

**测试要求**（`examples/test_授权链.light`，≥14 断言）：类型形状（三种提问/状态/结算）、NO_FLOW/UNKNOWN_METHOD/ALREADY_IN_FLIGHT/DECLINED/NOT_COMMITTED 全码、已中止直接 cancelled、单键单尝试拒绝、注销后流程消失、结算广播（failed 不改变调用方结果/INVARIANT 重抛）。

**反跑判据 3 项**：A 校验链顺序颠倒（ALREADY_IN_FLIGHT 提前）→红；B NOT_COMMITTED 不再抛→红；C 结算广播去掉 INVARIANT 重抛→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/凭据.light`、`src/认证.light`（可读参考）；禁区=其余。

---

## 任务6（webhook 域）：webhook-github → `src/网络钩子GitHub.light`

**上游源**：`packages/webhook/webhook-github/src/{body.ts, handler.ts, index.ts, types.ts}`（258 行）。宿主注入 webServer/webhookRuntime/credentials，**只复刻请求校验链 + 签名验证 + payload 归一**；路由注册/真实 HTTP 流剔除登记（光明 web服务器.light 宿主面）。

**复刻范围（核心语义）**：
1. **错误族**：`网络钩子HTTP错误(状态, 消息)`，状态 ∈ {400, 401, 405, 413, 415, 503}（对齐 WebhookHttpError）。
2. **内容长度校验**：`解析内容长度(值?)`——十进制正则 `^(0|[1-9]\d*)$`（不符→400 `无效的 Content-Length`）；超安全整数→413 `请求体过大`；缺省→无。
3. **有界体读取 `读有界UTF8体(流, 上限)`**（纯逻辑面）：声明长度 > 上限→413；逐块累计 > 上限→413；最终返回拼接文本（UTF-8 解码由宿主/测试注入）。
4. **必需头 `必需头(头表, 名)`**：`headersDistinct` 语义——值列表长度恰 1 且非空 trim，否则 400 `缺少 X 头`（对齐 missing 文案）。
5. **内容类型 `是JSON内容类型(值?)`**：`application/json`（小写比较）+ 至多一个 `charset=utf-8|"utf-8"` 参数，否则 415 `内容类型必须为 application/json`。
6. **方法门禁**：非 POST → 405 `方法不允许`（setHeader allow: POST 语义并入返回码）。
7. **签名验证 `验证签名(秘密, 体, 签名)`**（核心）：`sha256=<64 位十六进制>` 前缀校验 + **HMAC-SHA256 时间安全比较**（光明 `从 哈希 导入 HMAC_SHA256`；恒定时间比较用逐字节异或累计或固定长循环）；不匹配/格式错 → 401 `无效的 webhook 签名`（octokit verify 失败吞错语义=仅返回布尔）。
8. **payload 归一 `解析载荷(体)`**：JSON 解析失败 → 400 `请求体不是有效 JSON`；非对象（含数组/null）→ 400 `GitHub webhook 载荷必须是 JSON 对象`；无损 JSON 快照校验（光明 `解析JSON` + 类型判定；无损语义参照任务5 线格式）。
9. **配置校验 `校验配置(配置)`**：`source` 非空且 trim 后不变；`path` 以 `/` 开头、非根、无尾斜杠、无 `?`/`#`（错误文案对齐）。
10. **交付构造 `构造交付(源, 交付id, 事件名, 载荷, 接收时间)`**：`{kind:"github", source, deliveryId, event:{name, payload}, receivedAt}`（对齐 VerifiedWebhookDelivery）。

**测试要求**（`examples/test_网络钩子GitHub.light`，≥12 断言）：内容长度 400/413、必需头 400、内容类型 415、方法 405、签名验证（正确/错误秘密/格式坏/前缀错）、payload 400 三形态、配置校验（尾斜杠/query/非根）、交付构造形状。

**反跑判据 3 项**：A 签名比较改普通相等（非恒定时间）→红（用失败注入断言）；B 必需头允许多值（长度>1 通过）→红；C 内容类型放行无 charset 之外参数→红。

**文件互斥**：可写=本任务 4 个新文件；只读=`src/webhook会话.light`（可读参考）；禁区=其余。

---

## 验收与移交

1. 每路交付物：`src/<模块>.light` + `examples/test_<模块>.light`（PASS + rc=0）+ `_antirun_<缩写>.py`（N/N ALL OK）+ `_taskN_<域>_交付报告.md`。
2. 语言缺陷：新发现一律登记 §语言差异（建议 L-091 起）+ `examples/_repro_L0xx.light`（绕法形态 rc=0），**不得写 docs/ 账本**（路M 统一）。
3. 本轮目标模块清单（新增 6 src）：
   - 任务1 `src/字符串替换编辑器.light`（对标 #86）
   - 任务2 `src/外壳环境.light`（对标 #87）
   - 任务3 `src/目标轮驱动.light`（对标 #88）
   - 任务4 `src/会话日志增量.light`（对标 #89）
   - 任务5 `src/授权链.light`（对标 #90）
   - 任务6 `src/网络钩子GitHub.light`（对标 #91）
4. 移交路M：6 份报告 + 反跑 3/3 起 + 全量 CI 期望 259+6 passed。
5. 遗留延续（本轮不接）：工具注册表接入点（ask_user_question/查询工具）、approval/* 词汇注册、宿主面（agent 接线/HTTP 路由/凭据事件）——均在各自报告 §未移植项登记。
