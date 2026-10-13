# eval/polaris — 北极星评测（R5-D）

> 任务书：`.dumate/inbox/_R5_检索能力升级并行任务书.md` §4（D 路）。
> 北极星：**真实语料 holdout 零 token 覆盖率 ≥ 0.95**。
> 本目录是独立 subpackage（`package main`），不 import eval 主包
> （`run_kpi.go` 是 `//go:build ignore` 单文件 main），不改动 kernel 语义。

## 1. 口径定义

### M1 零 token 覆盖率（北极星主指标）

```
m1_coverage = hits / N
```

- 一次「检索试验」= 一条 query 跑一次 `RecallAndFuse`（**无任何 LLM 生成调用，
  生成 token 恒为 0**——「零 token」指答案由记忆检索直接给出，不经生成）；
- `hit` = expected 事实文本在 **top-K 候选 fact** 中被 `matchFactV4` 判定命中
  （K 由 `-topk` 注入，默认 5；判定器与 `run_kpi.go` 的 fact 口径 v4 同源，
  见 `polaris_fact.go` 头部的移植说明）；
- 辅助口径（不覆盖主口径）：`hit@1`（top-1 即命中）、ID 命中率 / MeanRR
  （语料带 `expected_memory_ids` 时）。

### M2 控制面 token 占比

```
m2 = Σcontrol_tokens / Σ(control_tokens + data_tokens)
```

- **控制面**：query + 路由 prompt + 召回 prompt 的表达 token
  （指令模板见 `polaris_token.go`；当前 Go kernel 编排是代码 0 token，
  指令模板是「prompt 表达口径」占位，用于与 Python+Prompt 基线同口径对比——
  定位文档 §7 M2 / §8.3 分离度量）；
- **数据面**：top-K 召回候选记忆正文（fact 文本）token；
- token 估算为**确定性词元近似口径**：CJK 单字 1 token、ASCII 词 1 token、
  标点不计（相对比较口径，非计费口径）。

## 2. 用法

```sh
cd /tmp/duan-light-merge/lightmem/eval/polaris   # 0.88 工作区

# ① 生成代理语料（真实 holdout 未到位时的降级路径；全量 + 固定文件序，无采样）
go run ./gen_holdout_proxy.go -out ../dataset/holdout_proxy.jsonl

# ② 评测模式（M1/M2）：默认权重 R5-A 交付态、embedder auto（vector 优先）
go run ./eval/polaris/ -holdout ../dataset/holdout.jsonl -report polaris_r5d

# ③ 压缩对照模式（控制面压缩前后 token 对比，不检索）
go run ./eval/polaris/ -mode compress -input ../dataset/holdout.jsonl \
    -report polaris_compress

# ④ 覆盖率不降验证（query 先压缩再检索，复跑 M1）
go run ./eval/polaris/ -holdout ../dataset/holdout.jsonl -compress \
    -report polaris_r5d_compressed
```

单测：`go test ./eval/polaris/ -v`（token 估算 / 压缩规则 / fact 对拍 /
M1·M2 累计器，全纯函数确定性）。

## 3. 真实语料 holdout 接入协议（用户提供语料后）

把语料放到 `eval/dataset/holdout.jsonl`（NDJSON，一行一条，UTF-8，LF）：

```json
{"id":"h-001","query":"客户A的偏好是什么","expected":"客户A的偏好是周一",
 "expected_memory_ids":["mem-..."],"bucket":"real","tenant":"t-1","user":"u-1",
 "as_of":"2026-10-01","memory":{...kernel.Memory...}}
```

- `query` / `expected`（正确事实文本）必填；其余可选；
- `memory` / `hop1_memory` / `second_version`：逐条物化后查询（与
  `run_kpi.go` 同链路）；也可用 `-corpus <memories.jsonl>` 批量预载，
  item 只带 `query` + `expected`；
- `expected_memory_ids`：可选，提供才启用 ID 命中率 / MeanRR 口径；
- 对抗样本（投毒/注入）不要放进 holdout——北极星分母只含真实 query。

⚠️ **代理语料性质**：`holdout_proxy.jsonl` 由 `gen_holdout_proxy.go` 从既有
评测集生成，样本参与过权重调优（非严格 holdout），只用于预跑口径与压缩实验；
**北极星达标判定以真实语料 holdout 为准**。

## 4. 确定性保证（红线 5）

- `-now-fixed` 默认 `2026-10-13`：`rf.Now` 固定，杜绝 `time.Now()` 进入
  freshness 信号导致复跑不一致（报告 JSON 的 `generated_at` 只是元数据）；
- 压缩规则 = 固定词表 + 固定正则（无随机源、无 map 迭代序依赖）；
- 同语料 + 同权重 + 同 embedder 复跑，M1/M2 逐位一致；
- embedder：`auto` = 向量文件在位用 BGE-m3 静态向量（R5-A 产物
  `eval/vectors/`），否则退 char n-gram TF-IDF；`vector` / `tfidf` / `none`
  可显式指定。**不自动触发 27 分钟的 BGE 生成**（由 run_kpi.go 链路负责）。

## 5. 已知限制

- 压缩规则 R4（结构虚词 的/了 文中剔除）**已下线**：实测删「的」误伤
  「X的Y是什么（周三）」类带锚点查询（7/590 单跳样本 top-1 翻车），压缩率
  仅从 35.77% 升至 37.42%，按「覆盖率不降」验收口径放弃该规则
  （`compress_light.go` 头部有完整说明）；
- 中文多字停用词仅按**句尾**剔除，句中疑问结构（是否/怎么…句中）保留；
- 英文停用词表**不含 "a"/"an"**：RE2 `\b` 以 ASCII 词字符为界，中文实体名
  （客户A）旁的单个 ASCII 字母会被孤立成「单词」遭 "a" 误删（实测踩坑，
  `polaris_test.go` 锁定）；
- token 估算口径与任何商用 tokenizer 不保证一致，仅用于本包内的
  压缩前后 / 控制面数据面对比；
- 北极星达标判定必须以**真实语料 holdout** 为准；`holdout_proxy.jsonl`
  是合成代理（样本参与过权重调优），只用于预跑口径与压缩实验。

## 6. 文件清单

| 文件 | 职责 |
|---|---|
| `polaris_eval.go` | 主程序：语料加载、kernel 装配、逐条检索、报告输出 |
| `polaris_m1.go` | M1 口径（零 token 覆盖率 + 累计器） |
| `polaris_m2.go` | M2 口径（控制面 token 占比 + 累计器） |
| `polaris_token.go` | 确定性 token 估算 + 控制面/数据面账目 + 指令模板 |
| `polaris_fact.go` | fact 判定 v4 移植（与 run_kpi.go 同源） |
| `compress_light.go` | 控制面压缩规则（R1-R6）+ 压缩前后对比报告 |
| `gen_holdout_proxy.go` | 代理语料生成器（`//go:build ignore`） |
| `polaris_test.go` | 确定性单测 |
