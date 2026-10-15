# Monty / 千脑理论（Thousand Brains Project）学习笔记

> 本文整理自对 Numenta 发起的 **Thousand Brains Project** 开源项目 **Monty** 的源码精读，
> 并给出对 lightmem（LIMS 光明记忆）的借鉴与改造建议。
>
> - 源码路径若无特别说明，均相对 **Monty 仓库根目录**（本机克隆于 `C:\codeartswork\tbp.monty`，
>   来自 GitCode 镜像 `gh_mirrors/tb/tbp.monty`，MIT 许可）。
> - 本文档目的：为 lightmem 的**参考系组织 / 多信号融合 / 记忆组织**设计提供可参考的工程范式。
> - 免责：源码行号对应克隆时的 main 分支快照，仓库演进后可能变化，引用前请以当前代码为准。

---

## 0. 背景：千脑理论与 Monty

### 0.1 作者与理论

- **Jeff Hawkins（杰夫·霍金斯）**：Palm / Handspring 创始人，后转向神经科学，与 Donna Dubinsky 共同创办
  **Numenta**（2005）。
- 《On Intelligence》(2004，中译《人工智能的未来》) 提出 **HTM**（Hierarchical Temporal Memory）。
- 《A Thousand Brains: A New Theory of Intelligence》(2021，中译《千脑智能》) 提出 **千脑理论**。
- 核心主张：
  1. **皮质柱即完整模型**：新皮层约 15 万个皮质柱（cortical column），每个柱独立学习"整个世界"的模型，
     而非各学一部分再拼接。
  2. **参考系（Reference Frame）**：借用大脑的网格细胞 / 位置细胞机制，每个皮质柱记录"物体特征相对于自己
     的位置"。
  3. **通过运动学习（sensorimotor）**：智能体主动移动传感器（手、眼）探测物体，用"移动—预测—校正"学习
     物体三维结构。
  4. **投票（Voting）**：成千上万皮质柱各自给出假设，通过投票达成共识，解释大脑对噪声与遮挡的鲁棒性。
  5. **新旧脑理论**：旧脑负责情绪/本能/生存，新脑（新皮层）负责智能与知识；智能 = 学习世界模型。

### 0.2 作者的开源项目谱系

| 阶段 | 项目 | 说明 |
|---|---|---|
| HTM 时代 | **NuPIC**（`numenta/nupic`，Python） | 2013 年开源的 HTM 官方实现 |
| | **htm.core**（C++，社区维护） | 性能重写版 |
| | **nupic.torch**（PyTorch） | HTM 与深度学习结合 |
| **千脑理论** | **Thousand Brains Project — Monty** | 2024 年开源，MIT 许可，是千脑理论的**直接工程实现** |

### 0.3 Monty 是什么

- 一个**感觉—运动学习框架**：用可堆叠的"学习模块（Learning Module，≈皮质柱）" + 参考系 + 投票，
  做**物体/姿态识别**。
- 当前为 **early beta**，主要用于研究验证。
- Monty 与 lightmem 的领域差异（很关键，别当同类）：

| 维度 | Monty | lightmem (LIMS) |
|---|---|---|
| 输入 | 传感器流（视觉/触觉） | 文本、对话、工具结果 |
| 记忆单位 | 物体模型图（节点 pose+features，边=位移） | 结构化文本事实（五类 memory_kind） |
| 目标 | 识别物体与姿态 | 多租户共享的长期记忆存取 |
| 多租户/权限/投毒/双时态 | **没有** | 是核心（§4/§7） |

> 结论：**表示与融合机制可借鉴，安全与合规部分没有现成可抄。**

### 0.4 环境与可运行性（重要结论）

- 官方**只支持 Linux / x86_64**；依赖 **Python 3.8 + conda + `habitat-sim`**（仅 conda/Linux）。
  官方 getting-started 顶部明确警告：`This guide will not work on Windows`。
- Windows 的官方路线是 **WSL + Ubuntu + Miniconda**。
- 仓库另提供 **uv 路线**（`.python-version` = 3.13，`simulator_mujoco` extra，可跳过 Habitat），但官方标注为
  **实验性 / 暂不支持**（`uv.lock` prototype）。
- 本机与实验机为何跑不通：

| 环境 | 结论 |
|---|---|
| Windows 本机 | 虚拟化未开启 → WSL 不可用；Python 3.12 与 Monty 需要的 `numpy<=1.23.5` 冲突（numpy 1.23 无 3.12 wheel）；Habitat-Sim 无 Windows 支持 |
| Linux 机 192.168.0.86 | 磁盘仅约 **4.2G 可用**、内存 **4G 且无 swap**；官方 conda 环境（pytorch 1.13.1 + habitat-sim ≥5G）+ YCB 数据集装不下 |

- **因此本文改为源码精读**，不依赖运行环境即可学习机制。

### 0.5 面向初学者的 Demo / 教程

- **没有网页 / 一键 notebook 式 demo**：官方把真实 demo 列为未来计划（`docs/future-work/monty-demos.md` 仅一句占位）。
- 有**分级教程 + 可直接运行的实验配置**：
  - 入门组：`running-your-first-experiment`、`pretraining-a-model`、`running-inference-with-a-pretrained-model`、
    `unsupervised-continual-learning`、`multiple-learning-modules`
  - 进阶组（标注 `[No Habitat Required]`，对初学者更友好）：`using-monty-in-a-custom-application`、
    `using-monty-for-robotics`、`running-an-experiment-from-a-different-repository`
  - 配置目录：`src/tbp/monty/conf/experiment/tutorial/`，最轻的是 `first_experiment.yaml`
    → `python run.py experiment=tutorial/first_experiment`
- **免装环境的直观材料**：仓库 `docs/figures/how-monty-works/` 下有 11 个 gif
  （`sensor_moves.gif`、`object_moves.gif`、`voting_rf_transform.gif`、`burst_sampling.gif` 等）；
  官方 YouTube 亦有逐步讲解视频。

---

## 1. 仓库导航

### 1.1 目录地图

| 路径 | 作用 |
|---|---|
| `src/tbp/monty/frameworks/` | **核心**：models（学习模块/证据匹配）、agents、environments、motor、sensors、loggers |
| `src/tbp/monty/conf/experiment/` | Hydra 实验配置，跑 benchmark 用 |
| `src/tbp/monty/sensor_modules/` | 传感器模块（把原始感知转成统一 CMP 格式） |
| `docs/theory/` | 理论（WIP，README 让看论文） |
| `docs/how-monty-works/` | 实现原理，**学习重点** |
| `docs/overview/glossary.md` | 术语表，**先读** |
| `docs/figures/how-monty-works/` | 原理配图与动画 |
| `benchmarks/` | 评测结果与任务定义 |
| `run.py` / `run_parallel.py` | 入口脚本 |

### 1.2 概念 → 代码映射

| 千脑理论概念 | Monty 里对应物 |
|---|---|
| 皮质柱 | Learning Module → `frameworks/models/evidence_matching/learning_module.py` |
| 大脑各区域互联（heterarchy） | CMP 协议 + 模块堆叠 |
| 参考系 | `graph_memory.py`（LTM） + `object_model.py`（网格） |
| 物体模型（带位置的特征图） | `object_model.py` 的 `GridObjectModel` |
| 投票 | `learning_module.py` 的 `send_out_vote` / `receive_votes` |
| 运动/探索策略 | `motor_policies.py`、`motor_system.py` |
| 特征@姿态（统一语言） | CMP，见 `src/tbp/monty/cmp.py` |

### 1.3 建议学习顺序

1. `docs/overview/glossary.md` → 建立术语
2. `docs/how-to-use-monty/getting-started.md` → 装环境、跑通
3. `docs/how-monty-works/implementation-overview.md` → 整体架构
4. `docs/how-monty-works/learning-module/evidence-based-learning-module.md` → 当前最优 LM
5. 读代码：先 `abstract_monty_classes.py`，再 `evidence_matching/` 下的 model / learning_module / graph_memory
6. 跑 benchmark（`conf/experiment/`）观察投票收敛

---

## 2. 源码精读

### 第 1 讲：内存里到底存了什么

LM 的记忆分**短期 buffer** 与**长期 graph memory**。长期记忆容器是 `EvidenceGraphMemory`
（`src/tbp/monty/frameworks/models/evidence_matching/graph_memory.py`），但它几乎只是"存/取/更新"的接口，
**真正的数据在 `GridObjectModel`**。

#### 1.1 三张稀疏表（核心）

`GridObjectModel.__init__`（`src/tbp/monty/frameworks/models/object_model.py`）定义三个 **4 维稀疏张量**：

| 字段 | 含义 | 维度含义 |
|---|---|---|
| `_observation_count` | 每个体素被观测几次 | 前 3 维 = 体素索引，第 4 维 = 计数 |
| `_feature_grid` | 每个体素内特征的**平均值** | 前 3 维 = 体素索引，第 4 维 = 特征向量 |
| `_location_grid` | 每个体素内位置的**平均值** | 前 3 维 = 体素索引，第 4 维 = xyz |

用稀疏张量省内存。这就是"参考系 + 网格"的落地：**物体被离散成 3D 体素网格，每个格子记住
"那里观测到什么特征、平均位置在哪、看过几次"。**

#### 1.2 从网格到图：top-k 体素

`_build_graph_from_grids` 只做一件事：**取观测次数最高的 k 个体素**（`_get_top_k_voxel_indices`，
k = `max_nodes`），把它们的平均位置/特征变成图的**节点**。

> 这是"constrained graph"的实现：观测多的区域留更多节点，平坦/少变区域被稀疏掉。
> 对应 `grid_types_blue_mug.png`、`graph_constrains.png`。

#### 1.3 一个硬约束：越界 >10% 就拒绝

`_update_grids` 先判断多少观测落在网格外，**若超过 10% 就抛 `GridTooSmallError`、整次更新作废**。
这实现了 `max_size` 约束——小模型装不下大物体，于是大物体靠"多个 LM 各自建不同分辨率模型"协作。
对应 `grid_with_same_size.png`。

#### 1.4 更新时先变换再入库

`update_model` 第一步是 `apply_rf_transform_to_points`：**把新观测从"身体参考系"变换到"模型参考系"**，
再并入网格。**每次学习都要先做坐标对齐**。

#### 1.5 假设空间

`Hypotheses`（`hypotheses.py`）是四个**平行数组**，每个下标 = 一个假设：

```
evidence  : 每个假设的当前证据分
locations : 该假设下，传感器在【模型参考系】中的位置
poses     : 该假设对应的 3×3 旋转矩阵
possible  : 该假设是否仍可能
```

每个物体（graph_id）各持有一份。**整段识别过程，本质就是在每个物体各自的假设空间里不断加减 evidence。**

---

### 第 2 讲：一次 matching step 发生了什么

入口 `matching_step`（`learning_module.py`）：

```
if 是纯位置步:            # is_location_only_step
    _displace_hypotheses  # 只平移假设，不改证据
    return
buffer 记录位移
_compute_possible_matches(...)
if 没有任何可能匹配: 终止 = "no_match"
```

核心在 `_update_possible_matches`：

1. **对每个已知物体分别更新证据**（`_update_evidence`），彼此独立，可选多线程并行。
2. 每次 `_update_evidence` 做三件事：
   - 若该物体还没有假设空间，先建空的；
   - **位移（displace）**：把每个假设的位置按"这一步移动了多少"推进（`_displace_hypotheses`）——**证据不变，只搬位置**；
   - **更新证据**（`hypotheses_updater.update_evidence`）：拿新观测特征与模型特征比，加减 evidence。
3. 全部物体更新完后：`_threshold_possible_matches` 过滤 → `_calculate_most_likely_hypothesis` 选 **MLH**。

> 关键直觉（对应 `evidence_update2.png`）：
> **"如果我在模型的位置 X、物体朝向 Y，那么移动 D 之后我应该到了 Z。"**
> 位移 + 旋转假设 = 新的搜索位置。

**对应动画**：`sensor_moves.gif` / `object_moves.gif`、`evidence_update1.png` / `evidence_update2.png`、`movement_transform.gif`。

**一句话**：记忆 = 体素网格三张表 + top-k 图；推理 = 在"物体×姿态×位置"假设空间里，
**用移动推进位置，用特征比对加证据**，谁证据高谁就是答案。

---

### 第 3 讲：证据分怎么算

每个假设的 evidence 由**姿态证据（pose）+ 特征证据（feature）**相加。

#### 3.1 特征证据：绝不给负分

核心在 `DefaultFeatureEvidenceCalculator.calculate`（`feature_evidence/calculator.py`）：逐节点算存储特征与观测特征的差异：

- **数值特征**：绝对差 `|stored - query|`
- **循环特征**（HSV 的色相 H）：取环上最小距离
- **分类特征**（`object_id`）：不等则为 1

然后**最关键的一行**：

```
feature_evidence = clip(tolerance - difference, 0, inf) / tolerance
```

含义：**差异 < tolerance 才给正证据，完美匹配 = 1，超过 tolerance = 0——永远不扣分**。
最后按 `feature_weights` 加权平均。`pose_vectors`/`pose_fully_defined` 被排除，由姿态证据单独处理。

#### 3.2 姿态证据：用"自定义距离"搜索

`_calculate_evidence_for_new_locations`（`hypotheses_displacer.py`）：

1. 先把观测的姿态向量按当前假设旋转过去（`rotate_pose_dependent_features`）。
2. 在图上找搜索位置的**最近 K 个节点**（KDTree）。
3. **不用圆形半径**，而用 `get_custom_distances` 结合表面法向与曲率——**沿表面方向搜得远、沿法向很快变远**；
   曲率决定"扁"还是"圆"。对应 `radius.png`。
4. 距离超过 `max_match_distance` 的节点被 mask，该假设证据置 **-1**。

#### 3.3 合并：取最大值，不是平均

`pose_evidence + feature_evidence` 后取 `np.max(...)`。

> 代码注释解释得很清楚：**只要搜索半径内有一个好匹配就给高分**，附近存在不匹配的点不影响——
> 这是对"特征快速变化区域 + 噪声位置"的鲁棒设计。

#### 3.4 累积：有界 vs 无界

```
evidence = past_evidence * past_weight + new_evidence * present_weight
```

若 `past_weight + present_weight == 1` → **有界**；默认两者都为 1 → **无界累积，旧证据永不遗忘**。
文档明确说"未来有界更合理，尤其 agent 换物体时旧证据应失效"——与 lightmem"遗忘要显式化"同构。

#### 3.5 别给"没希望"的假设浪费算力

`evidence_update_threshold`：默认 `x_percent_threshold` → 只更新"证据 ≥ 当前最高分 − 最高分·x%"的假设。

#### 3.6 假设从哪来

`_get_all_informed_possible_poses`（`hypotheses_updater.py`）：用观测的**表面法向 + 曲率方向**，
与模型每个节点存储的 `pose_vectors` 对齐，反推可能的旋转。因为曲率方向有 **180° 歧义**，每个位置通常得到
**2 个旋转假设**；平坦面/球面（两主曲率相等）则沿法向轴**采样 N 个**。对应 `evidence_init_all.png`。

---

### 第 4 讲：投票（多模块共识）

#### 4.1 发送

`send_out_vote`：证据经缩放（最高=1，最低=-1）后，凡超过 `vote_evidence_threshold` 的假设，
打包成 vote（`location` 相对身体 + `pose_vectors` + `confidence`），并附上本 LM 的 `sensed_pose_rel_body`。

#### 4.2 接收

`receive_votes` → `_update_evidence_with_vote`：

1. 把收到的 vote 位置建 KDTree。
2. 对本 LM 的每条假设位置，找最近 3 个 vote。
3. 取半径内**最大** vote 证据（`np.ma.max`），太远的被 mask。
4. 并入本 LM 证据：有界时加权平均，无界时累加。

#### 4.3 为什么需要参考系变换

vote 里的位置是**相对投票者身体**的。接收方要结合双方的 `sensed_pose_rel_body` 算出相对位移，
把 vote 转到**自己的参考系**才能比较——这就是 `voting_rf_transform.gif` 演示的"投票位置的参考系变换"。

> 一句话：**投票让"每个物体一个假设空间"从各自为战变成互相背书**，多个 LM 快速收敛到一致答案。

**四讲总览**：

```
观测 → 转移到模型参考系(update_model)
     → 每个物体：位移推进假设位置(_displace)
                → 姿态证据 + 特征证据(只加不减、半径内取max)
                → 累积 evidence(有界/无界由权重决定)
     → 多 LM 间投票(参考系变换后加热彼此假设)
     → 选 MLH、阈值筛 possible_matches
     → 姿态唯一 or 对称稳定 → 输出(物体, 姿态)
     → match / new_object / timeout
```

**贯穿始终的两条原则**：① **一切先对齐参考系**再比较；② **只加证据不扣分、取最佳匹配**。

---

### 第 5 讲：怎么判断"已经识别出来了"

#### 5.1 选出 MLH

`_calculate_most_likely_hypothesis`：对每个物体取 `argmax(evidence)` 拿本物体头号姿态假设，
再跨物体比，证据最高的即 **MLH**，返回 `{graph_id, location, rotation, scale, evidence}`。
记忆为空时退化成 `new_object0`。

#### 5.2 哪些物体还算"可能"

`_threshold_possible_matches` 按证据分布决定候选物体列表：

- 证据分散（`std > 0.1`）或最高证据 < 0：阈值 `th = max_ge − max_ge·x%`，只保留证据 > th 的物体；
- 记忆里只有 1 个物体：证据 ≥ 0 就算可能；
- 物体证据差不多：全部保留。

#### 5.3 姿态是否"唯一"

`_check_for_unique_poses` 要求**两条件同时成立**：

- **位置唯一**：所有可能位置到其中心最大距离 < `path_similarity_threshold`；
- **旋转唯一**：所有可能旋转与 MLH 旋转夹角 ≤ `pose_similarity_threshold`（用旋转矩阵迹 `arccos((tr−1)/2)`）。

#### 5.4 对称物体：用"连续稳定"代替"唯一"

圆柱、球类物体**本来就有多个等价姿态**。`_check_for_symmetry`：

- 比较"这一步的可能假设集"与"上一步的"重叠是否 **> 90%**；
- 重叠 → `symmetry_evidence += 1`，否则**清零**（必须**连续**累积）；
- 累积到 `required_symmetry_evidence` 即判定"对称"。

#### 5.5 收尾与终止

- `get_unique_pose_if_available`：**位置/旋转唯一** 或 **对称成立** → 输出 `(location, rotation, scale)`，
  写入 `detected_pose`；否则返回 None（"认出了物体但姿态未定"）。
- `update_terminal_condition`：若候选物体**不是恰好 1 个**，就把所有姿态假设 `possible` 清空（先不解析姿态），
  再交给父类按证据/步数决定 `terminal_state`。
- `set_detected_object` 决定结局：
  - `match` → 取候选里的物体 ID（**识别成功**）；
  - `no_match` / 无候选 → `new_object{n}`（**当新物体学进记忆**）；
  - 评估模式下超时但证据够 → 用 MLH，否则 `None`（不更新记忆）。

> 对应可视化：`possible_matches.png`（候选收缩）、`hyp_driven_example.png`（用假设驱动探索）。

---

### 第 7 讲：从零学习新物体与 Burst Sampling

#### 7.1 什么时候学新物体

收敛阶段 `set_detected_object` 返回 `new_object{n}` 时（见第 5 讲），LM 把本次 episode 收集到的位置/特征建成新模型。

- 建模入口 `_build_graph`（`src/tbp/monty/frameworks/models/evidence_matching/graph_memory.py`）：
  新建 `GridObjectModel` → `build_model(locations, features)` → 存入 `models_in_memory[graph_id][input_channel]`；
  若 `GridTooSmallError` 则放弃（呼应第 1 讲的 10% 越界约束）。
- 加载预训练图用 `_add_graph_to_memory`（同文件）：加载后把稀疏张量重新 `coalesce()`。
- 对已有物体的追加观测用 `_extend_graph`（同文件）。

#### 7.2 为什么需要 Burst Sampling

- 问题：刚接触一个新物体/新朝向时，系统不知道"物体在哪、朝向如何"。传统做法
  `_get_all_informed_possible_poses` 只在曲率方向对齐下生成 **2 个**假设；遇到对称物体或曲率不明确时
  **假设太少、覆盖不足**。
- 若在每个节点上都穷举所有朝向，假设数会**爆炸**。
- Burst Sampling 的思路：**不确定时"爆发"生成一批假设，再用证据趋势淘汰**——类似粒子滤波的重采样，
  但是确定性实现。

#### 7.3 实现：`BurstSamplingHypothesesUpdater.update_evidence`

`src/tbp/monty/frameworks/models/evidence_matching/burst_sampling.py`，每步在假设已位移之后执行：

1. **`_sample_count`** 决定"保留几条旧、新建几条新"：
   - 新建数 = `round(节点数 × sampling_multiplier)`，按通道 cap 到 `num_hyps_per_node` 并取整；
   - 旧假设的保留/删除交给 `EvidenceSlopeTracker` + `deletion_trigger_slope`。
2. **`_sample_existing_hypotheses`**：按斜率**确定性**保留（不是随机重采样）。
3. **`_sample_new_hypotheses`**：**只从 top-k 特征匹配最高的节点**采样新假设——避免为低证据节点浪费算力。
4. 合并旧 + 新（`Hypotheses.concatenate`），并 `tracker.update` 记录证据。

- `sampling_burst_steps`：连续爆发若干步（每步递减），模拟"对不确定区域连续多采样"。
- 每个节点生成几条假设由 `_num_hyps_per_node` 决定：`pose_fully_defined` → 2；否则 → `umbilical_num_poses`。

#### 7.4 关键角色：`EvidenceSlopeTracker`

- 跟踪每个假设证据的**斜率（趋势）**，用**趋势**而非瞬时值决定删除
  （`src/tbp/monty/frameworks/models/evidence_matching/evidence_slope_tracker.py`）。
- 好处：避免"某一步偶然高分"的错误假设被保留；上升趋势的假设值得保留。
- 对应图示 `burst_sampling.gif`。

#### 7.5 与无监督学习的关系（官方 future-work）

- 现状：无监督下"新建参考系（模型）"的触发是"所有已知物体证据都为负、且超过最少步数"
  （`docs/future-work/learning-module-improvements/unsupervised-learning-with-burst-sampling.md`）。
- 问题：burst sampling 下这个条件**可能永远达不到**——因为 burst 总会制造新假设并产生正证据
  （不同物体的 1cm 局部补丁往往相似）。
- 官方建议：**用 burst sampling 的持续时间本身作为"该建新模型"的信号**。
- 状态：尚未默认启用。

---

### 第 8 讲：运动与探索策略

- LM **不决定"怎么动"**；`motor_system` + `motor_policies` 决定。但 LM 通过"候选"影响探索方向。
- 主要策略（`src/tbp/monty/frameworks/models/motor_policies.py`）：

| 类 | 作用 |
|---|---|
| `InformedPolicyRandomWalk` | 带少量信息的随机游走 |
| `PredefinedPolicy` | 预设轨迹（测试用） |
| `JumpToGoal` | 直接跳到目标 |
| `InformedPolicy` | 假设驱动：用当前假设规划下一个动作 |
| `NaiveScanPolicy` | 朴素扫描 |
| `SurfacePolicy` | 沿物体表面探索（保持接触，利用曲率） |
| `SurfacePolicyCurvatureInformed` | 曲率引导的表面探索 |

- **假设驱动的判别性动作**：LM 用 `get_top_two_mlh_ids` /
  `get_top_two_pose_hypotheses_for_graph_id`（`learning_module.py`）给出"两个还在竞争的候选"，
  motor 选择能区分它们的动作——这就是**主动感知（active sensing）**。
- 对应图示 `hyp_driven_example.png`、`policies.png`。

**小结**：识别的完整闭环是"**感知 → 假设 → 投票 → 收敛 → (识别成功 | 建新模型)**"，
而"下一步怎么动"由运动策略根据当前假设主动决定，让不确定性下降最快。

---

### 第 9 讲：参考系全景与变换（含投票在模型空间）

来源：`docs/how-monty-works/monty/reference-frame-transformations.md`。

#### 9.1 一个实验里并存 6 个参考系

| 参考系 | 含义 | Monty 是否已知 |
|---|---|---|
| Object rel. World | 物体在世界的朝向 | **未知**（模拟器配置） |
| Feature rel. Object | 物体上各特征的位姿 | **未知**（网格定义） |
| Sensor rel. World/Body | 传感器在世界/身体的位姿 | 本体感觉 + motor efference copy 估计 |
| Feature rel. Sensor | 特征相对传感器的朝向（表面法向+曲率） | 由深度图提取 |
| Feature rel. World/Body | `sensor_rel_world × feature_rel_sensor` | 可算 |
| **World rel. Model** | LM 假设的"模型朝向"（有多个假设） | **需推断**（`possible_poses`） |
| Feature rel. Model | `feature_rel_world × object_rel_model` | LM 用它把特征转进模型系 |

#### 9.2 三条关键变换

1. **保持输入恒定**：SM 把"传感器位姿"与"特征相对传感器位姿"合成，输出特征在**世界/身体公共参考系**下的位姿；
   于是**传感器 fixate 一个点时，无论从哪个角度看，送出的特征位姿都一样**（`sensor_moves.gif`）。
2. **应用姿态假设**：LM 用"world rel model"假设，把进来的特征旋转到模型参考系 → **任意朝向都能识别**（`object_moves.gif`）。
3. **位移也要变换**：传感器位移（两次位置差）同样要旋转到模型参考系，再更新"假设的模型内位置"
   （`movement_transform.gif`）。

#### 9.3 投票发生在"模型空间"

- 投票**直接在发送方模型 RF 与接收方模型 RF 之间翻译**；前提假设是**两个 LM 同时学习该物体**，参考系自然对齐
  （都是"特征相对世界"，同时学会对齐）。否则需**存一个位移**并在投票时一并应用。
- 两传感器在不同位置（一个摸杯把、一个摸杯沿）时，接收方用**自己的 sensed pose** 与发送方附带的 sensed pose 求位移，
  把 vote 假设**平移 + 旋转**过去（`voting_rf_transform.gif`）。
- 直觉："**如果你在那边、你认为你在杯子的这个位置，那我应该在你的右下 x 处。**"

> **对 lightmem 的启发**：lightmem 只有"逻辑坐标"没有"位姿"，但 Monty 的教训是——**跨来源融合前必须先把所有来源
> 映射到共同空间**。这个"共同空间"在 lightmem 里就是统一 `reference_frame` + 实体消解。投票"依赖两套参考系对齐"
> 正是**跨 Agent 融合前必须做参考系对齐**（§3.14）的强论据，强化了建议 5。

---

### 第 10 讲：异构层级与组合性（Heterarchy）

来源：`docs/how-monty-works/connecting-lms-into-a-heterarchy.md`。

#### 10.1 为什么叫 heterarchy 而不是 hierarchy

- **不是纯前馈**：存在 **skip connection**（SM 或低层 LM 可直连高层 LM），很难按"处理层数"给 LM 定层。
  LM 更适合按"**谁和谁投票**（模型是否有重叠物体）"分组。
- **多种通信通道**：top-down（高层 LM 输出作为下级 LM 的偏置上下文）、lateral votes、recurrent 更新、goal。
- **每个 LM 都是完整 sensorimotor 系统**，可**直接输出 motor**——运动输出不属于层级末端。

#### 10.2 组合性（bottom-up）

- 发送 LM 的输出 **object_id 会成为接收 LM 模型里的一个 feature**；接收 LM 只拿到 **object ID + pose**，
  **拿不到 3D 结构**。
- 例：低层 LM 详细建模 tire → 成为高层 LM 的 car 模型中的一个特征；**tire 模型还能被 truck/bus 复用**。
- 即"**先学部件的详细模型，再组合成大模型**"，避免重学整车。

#### 10.3 自顶向下偏置（top-down）

- 高层识别出 car → 偏置低层去识别"部件"，类似**由过去共现学到的联想**（"以前在这个姿态收到 car 时我摸到 tire"）。
- top-down 不只传 object，还传 **pose**；作用是**借助更大场景/物体的上下文更快识别对象**。

> **对 lightmem 的启发**：
> - lightmem 的 L0–L3 分层应借鉴"**异构**"：允许**跳层引用**（L3 经验直接引用 L1 事实）、
>   **自顶向下偏置**（§3.8 goal / §4.4 Agent 目标影响低层检索优先级）、**横向投票**（§3.2 多 Agent 共享），
>   不强制严格自下而上。
> - **组合性**：L2 长期记忆可由 L1 片段"组合"而成（object_id 当 feature）——对应"程序性经验由多个事实组合沉淀"；
>   关键是**只组合摘要/ID，不搬运完整细节**（呼应"模型永不共享"与降低投毒面）。
> - **复用**：一个被规范化的实体可被多个上层记忆引用（`entity-alias` 位移边）。

---

### 第 11 讲：LM 的三种输出与第二种实现（位移匹配）

来源：`docs/how-monty-works/learning-module/learning-module-outputs.md`、`src/tbp/monty/frameworks/models/displacement_matching.py`。

#### 11.1 每步三种输出（都是 CMP Message）

1. **Pose + Features**：最可能的 object ID（作为 feature）+ 最可能 pose；可送另一 LM，或被实验框架读来判断终止条件与评估。
2. **Vote**：当前假设 + 似然 → 连接的其它 LM。
3. **Goal**：建议动作（目标位姿）。由 goal generator 依据**高层 goal + LM 内部状态/模型**选择；可被子 LM 拆成子目标，
   或由 motor 翻译成命令。例如"建议传感器移到一个能更快识别物体的位姿"。

#### 11.2 第二种 LM 实现：`DisplacementGraphLM`

- 用图中存储的 **displacement（位移）或 PPF（point pair features）** 做匹配，而非特征证据累积。
- `tolerance`：观测位移与存储位移多接近算匹配；`use_relative_len`：用 PPF 时可实现**尺度不变**。
- `_make_predictions` / `_predict_using_displacements` 用位移做预测，`_get_prediction_error` 算误差 →
  **不一致的假设被移除**（而非扣证据）。
- 对比：**EvidenceLM** = 特征证据累积、保留候选；**DisplacementLM** = 位移预测、不一致即剪枝。

> **对 lightmem 的启发**：
> - 两种 LM 对应 lightmem 的**两种检索融合策略**：证据式（§3.4 多信号 rank 累积、保留候选）与
>   **关系/位移式**（用记忆间关系预测下一个候选，不一致即剪枝）——后者正好落在"建议 1 的记忆位移边"上。
> - **goal 输出**对应 §3.8 GoalMemory / §4.4 Agent 主动记忆："LM 建议下一步动作以更快识别" ↔
>   "Agent 建议下一次检索/澄清动作以更快确认事实"。
> - 三种输出的**统一表示**对应 §4.1 统一 Schema 的多用途（写入/检索/目标）。

---

### 第 12 讲：评测体系与无监督推断

来源：`docs/overview/benchmark-experiments.md`。

#### 12.1 自定评测集（跟踪研究进度，非通用榜单）

- **短 suite**：10 个 YCB 物体（`10distobj` 差异大 / `10simobj` 相似）；**长 suite**：全部 77 个。
- **正交维度**：`base`（已知朝向）/ `randrot`（随机朝向）/ `noise`（传感器加噪）/ `rawnoise`（原始深度加噪）；
  `dist_agent` / `surf_agent`；`multi`（干扰物）；5LM（多传感器多模块）。
- **指标**：accuracy、numsteps、runtime；无监督用 `mean_objects_per_graph` / `mean_graphs_per_object`。
- 参数不追求最优精度，而是**速度与精度的平衡**，便于快速迭代。

#### 12.2 无监督推断（unsupervised inference）

- 不 reset 内部状态就**交换物体**，测试系统能否**放弃旧假设、开始对新物体累积证据**。
- 现实世界没有明确 episode 边界（物体可能被替换、遮挡、组合），所以需要这种能力。

> **对 lightmem 的启发**：
> - lightmem 的评测分桶可借鉴 Monty 的**正交维度切分**（数据分布 × 噪声 × Agent/传感器数 × 干扰项）与
>   **自定进度跟踪集**（而非盲目追通用榜单）。
> - **无监督推断** ↔ lightmem 的"**无明确会话边界时的记忆切换**"：话题/实体突变时，系统应放弃旧假设、开始新记忆
>   （与建议 3 的"参考系漂移"一致）。
> - **合并度指标**：`mean_objects_per_graph` / `mean_graphs_per_object` ↔ 可定义"每条记忆被多少实体引用 /
>   每个实体分散在多少条记忆"，用于**监控实体消解质量**（§3.14）。

---

### 第 13 讲：恒定表征与不变性（Stable Representation）

来源：千脑理论概念 + `docs/how-monty-works/faq-monty.md`、`docs/how-monty-works/monty/reference-frame-transformations.md`。

#### 13.1 概念澄清（三个易混词）

| 概念 | 含义 |
|---|---|
| **不变性 invariance** | 输出对某变换不敏感（物体旋转后仍识别为同一物） |
| **等变性 equivariance** | 输出随输入变换而**相应**变换（视角变了，表征可预测地变） |
| **恒定表征 stable representation** | 在**以物体为中心的参考系**里，物体各部分的相对位置**固定不变**——与观察角度解耦 |

关键：**感知本身是等变的**（从不同角度看杯子，图像一直在变），但大脑把它投影到一个**不变的内部空间**后，
**同一物体始终落在同一组位置**上。

#### 13.2 机制：参考系 + 位置

- **传统答案（CNN）**：不变性靠**层级抽象**——逐层池化、感受野变大，最终某神经元"恰好"对旋转不变，隐式、靠数据学。
- **千脑理论的答案**：不变性靠**参考系**——皮质柱用**网格细胞**建立"以物体为中心"的坐标系，
  把特征放到"相对于物体的位置"上；传感器移动到哪都无所谓，特征**总落在模型的同一位置** → 恒定。
- 因此 Hawkins "**位置是智能的核心**"的本质是：**位置（参考系）是产生恒定表征的机制**。

#### 13.3 恒定表征是投票的前提（关键）

Monty 官方 FAQ（`docs/how-monty-works/faq-monty.md`）：

> 皮质柱用 features-at-locations 在 L3 建立一个"**more stable representation**"；
> 不同输入的柱要形成共识（consensus），**用稳定表征会容易得多**，
> 而不是用"时刻变化"的底层特征。

逻辑链：**表征恒定 → 不同皮质柱（不同感受器）才能对齐 → 才能互相投票达成一致**。

#### 13.4 Monty 的落地（证据）

- 节点必须存在"**物体中心、任意原点**"的公共参考系里（`object-models.md`）。
- SM 保证输入恒定："**Keeping Input to the LM Constant as the Sensor Moves**"（`reference-frame-transformations.md`）。
- LM 用姿态假设把特征转进模型系："the incoming features used for recognition **remain constant** as the object rotates"
  （同文档）。
- L3 的稳定表征支撑投票（`faq-monty.md`）。
- 与**第 9 讲**（6 参考系、world rel. model）是同一件事的两种说法：感知等变 → 变换到 object rel. model 后恒定。

#### 13.5 边界

- 书中的具体章节/页码**无法逐页核对**；但"稳定/恒定表征"确为全书核心（尤其讲参考系与投票的部分）。
- Monty 的**尺度不变性**仍是 future-work（`docs/future-work/learning-module-improvements/support-scale-invariance.md`）：
  "把恒定表征扩展到缩放"尚未完全解决。

#### 13.6 对 lightmem 的启发

- lightmem 的"恒定表征" = **记忆的稳定身份**：一个实体/事实无论从哪个**会话、Agent、表述**进入，
  都应落到**同一稳定标识**（幂等键 + §3.14 实体消解）。
- Monty 的"物体中心参考系" ↔ lightmem 需要"**以实体为中心的规范化空间**"，让不同表述对齐到同一位置。
- **恒定表征是投票（多来源融合）的前提**——所以"跨 Agent 融合前必须做参考系/实体对齐"（建议 5）
  不是可选项，而是**恒定表征能否成立的基础**。

---

## 3. CMP 消息与传感器模块（第 6 讲）

### 3.1 CMP 一句话定义

**"Features at a pose"（某个姿态下的特征）**（`docs/overview/architecture-overview/cortical-messaging-protocol.md`）。
所有组件（SM、LM、motor）都遵守它，因此可以**任意拼接**。pose = 3D 位置（相对身体）+ 3D 旋转；特征分两类：

- **形态学特征（morphological，随姿态变）**：`pose_vectors`（3×3 正交向量 = 表面法向 + 两个曲率方向）、
  `pose_fully_defined`、`on_object`
- **非形态学特征（non-morphological，不随姿态变）**：color、texture、curvature，或 LM 层的 `object_id`

### 3.2 `Message` 类

`Message`（`src/tbp/monty/cmp.py`）是 CMP 的落地，字段：
`location` / `morphological_features` / `non_morphological_features` / `confidence` / `pass_message` /
`sender_id` / `sender_type` / `process_features_in_lm`。

构造时 `_check_all_attributes` 强制校验：`pose_vectors` 必须 (3,3)、`pose_fully_defined` 必须 bool、
`location` (3,)、`confidence ∈ [0,1]`。辅助方法固定语义：

- `get_surface_normal()` = 第 0 个 pose 向量
- `get_curvature_directions()` = 第 1、2 个

### 3.3 同一个 `Message`，四种语义

| 发送方 | 语义 | 内容 |
|---|---|---|
| SM | **感知 percept** | 观测到的特征@姿态 |
| LM | **假设/最可能感知** | 物体 ID + 姿态 |
| LM→motor | **目标 goal** | 想去的位置/朝向 |
| LM↔LM | **投票 vote** | 只含候选姿态 + 物体 ID，**不含模态特征** |

> `Goal` 是 `Message` 的子类，允许 None 值，另带 `goal_tolerances`。

### 3.4 传感器模块 = transform 流水线

`SensorModule`（`src/tbp/monty/sensor_modules/sensor_module.py`）**自身不做具体处理**，
而是把 `SensorObservation` 依次过一串可配置的 `Transform`，产出 `Message` percept。

关键在 `update_state`：用 agent 身体位姿 + 传感器相对位姿，算出**传感器实际位姿**：

```
position = agent.position + rotate(agent.rotation, sensor.position)
rotation = agent.rotation * sensor.rotation
```

**这就是"相对身体"坐标的来源**——所有 pose 建立在身体公共参考系上。

### 3.5 数据进入 LM 的链路 + 关键开关

```
SM: observation → transforms → Message(percept)
   → (connectivity) → LM 收到 percepts，存 buffer
   → matching_step 用它更新证据
```

`matching_step` 里过滤：

```python
feature_percepts = [p for p in percepts if p.process_features_in_lm]
```

**只有 `process_features_in_lm=True` 的消息才参与证据更新**；否则只当"位置同步"（如纯移动步）。

### 3.6 `sensed_pose_rel_body` 从哪来

`send_out_vote` 从 buffer 取 `buffer.get_current_pose(input_channel="first")`——即**最近一次 SM 感知的相对
身体位姿**。投票时靠它计算两个 LM 之间的位移，把 vote 从对方参考系**变换到自己参考系**。

### 3.7 一条重要边界：模型永不共享

协议文档明确写：

> **"At no point do we communicate structural model information between learning modules."**

即：LM 之间**只交换"特征@姿态/目标/投票"，永不共享各自模型结构**。
这对 lightmem 是直接可用的设计论据——**共享线索与姿态，不搬运完整记忆结构**，既省带宽也降低投毒攻击面。

---

## 4. 对 lightmem 的映射与改造建议

### 4.1 映射总表

| Monty 机制（源码依据） | lightmem 落点 | 关系 |
|---|---|---|
| graph memory：节点+**位移边**（`object_model.py`） | §3.2 ReferenceFrame | **可升级**（当前是扁平维度） |
| 特征证据"只加不减 + 取 max"（`calculator.py`、`hypotheses_displacer.py`） | §3.4 VoteFusion | **可借鉴算子** |
| 证据有界/无界 vs 换物体需失效 | §5.3 遗忘契约 | **lightmem 更完善**，反向验证 |
| buffer(STM)→LTM 的 episode 边界 | §2.2 / §3.5 L0→L1 | **可借鉴触发时机** |
| CMP "同一 Message 多语义"（`cmp.py`） | §4.1 统一 Schema | **同构** |
| 投票需参考系变换（`cortical-messaging-protocol.md`） | §3.2 visibility=shared_user | **关键补充** |

### 4.2 建议 1：ReferenceFrame 从"扁平坐标"升级为"带位移边的记忆图"（高价值）

- **Monty 依据**：graph memory 的节点是 `pose+features`，**边是 displacement**（从 A 到 B 需要的关系/动作）。
- **lightmem 现状**：§3.2 是一组独立维度，**记忆之间没有关系**。
- **具体改法**：新增边表，让记忆可"跳转"，关系类型建议 `causal / temporal / task-flow / entity-alias`。
- **收益**：支撑 §3.13 后台整合与**多跳检索**。
- **风险**：边会膨胀，需与 §5 容量预算一起治理（可只对 L2 长期记忆建边）。

### 4.3 建议 2：VoteFusion 融合算子借鉴"只加不减 + 相对容差"（中价值）

- **Monty 依据**：`feature_evidence = clip(tolerance − diff, 0, ∞) / tolerance`——**不匹配记 0，绝不给负分**；
  半径内**取 max 而非平均**。
- **lightmem 现状**：§3.4 已是 rank 融合，接近融合需 `(top1−top2)/top1 < 20%` 且 `top1 ≥ 0.6`。
- **具体改法**：
  1. 明确——**每个信号只贡献正向名次分，弱信号缺失记 0，不反向扣分**；
  2. 把 `20%` 显式定义为**相对容差（tolerance）**，超容差候选"记 0"而非"扣分"。
- **保留差异**：lightmem 的"绝对分下限 0.6 禁止融合"是 Monty 没有的，**应保留**。

### 4.4 建议 3：用"参考系漂移"补强遗忘排序（中价值，反向借鉴）

- **Monty 痛点**：evidence 默认**无界累积、不遗忘**，文档自承"换物体时旧证据应失效，有界更合理"。
- **lightmem 现状**：§5.3 已有"遗忘观测契约 + 衰减与失效解耦"，**比 Monty 完善**。
- **具体改法**：把 Monty 暴露的问题变成显式规则——**排序衰减不应只由时间驱动**；
  当 `reference_frame` 的 `entity/scene/task` 与当前查询显著不同时，即使记忆"很新"，排序分也应快速衰减。

### 4.5 建议 4：显式定义 L0→L1 的落库边界（低风险）

- **Monty 依据**：buffer 只存当前 episode，**episode 结束才 flush 进 LTM**。
- **lightmem 现状**：§3.5/§3.13 是"持续捕获 → 压缩 → 整合"，边界偏模糊。
- **具体改法**：定义显式 flush 事件——**会话结束 / 话题切换 / 任务完成 / L0 容量阈值**触发，而非随时写。

### 4.6 建议 5：把"投票参考系变换"制度化为多 Agent 共享的强制前置（高价值，安全相关）

- **Monty 依据**：vote 是"相对发送者身体"的，接收方必须变换到**自己**参考系；
  且**投票不是 bag-of-features，而依赖特征在世界的相对排布**。
- **lightmem 现状**：§3.2 `shared_user` 让同用户下多 Agent 共享，但**同一事实在不同 Agent 的
  entity/scene 语境下可能指代不同**。
- **具体改法**：**跨 Agent 融合前强制做实体消解**（§3.14 EntityLinker）作为"参考系对齐"；
  未对齐的记忆**只并列展示，禁止融合**。
- **收益**：直接加固 §7.5 投毒防护与共享安全。

### 4.7 建议 6：§3.4 的"候选池"显式化为查询级假设集（低价值，锦上添花）

- **Monty 依据**：每个物体维护假设空间（多 evidence），最后收敛。
- **具体改法**：把每次查询 top-k 结果显式记为一个"假设集"，允许"**触发澄清 → 再观测 → 收敛**"。

### 4.8 不建议照搬（重要边界）

Monty **完全没有**多租户、权限、投毒、双时态——所以 §3.2 硬过滤、§4 双时态、§7 安全**没有现成可抄，必须自研**。
反过来，Monty 的"**模型永不共享**"原则可作为 §3.2 / §7.5 的**设计论据**。

### 4.9 优先级建议

1. **建议 5**（参考系对齐，安全）与 **建议 2**（只加不减算子）——改动小、收益直接；
2. **建议 1**（记忆位移边）——价值最高但需配套存储与容量治理；
3. **建议 3/4**——规则层微调，低成本；
4. **建议 6**——可选增强。

---

### 4.10 对 lightmem 的进一步映射（第 9–12 讲）

| Monty 机制（新增学习） | lightmem 落点 | 建议 |
|---|---|---|
| 6 参考系 + 变换链；投票在模型空间且依赖"参考系对齐" | §3.2 / §3.14 | 跨来源/跨 Agent 融合前，必须先映射到统一 `reference_frame` 并做实体消解（强化建议 5） |
| Heterarchy：跳层连接、top-down 偏置、横向投票、每层皆可驱动 motor | §2 分层（L0–L3） | 不强制严格自下而上；允许**跳层引用**、**goal 自顶向下偏置检索优先级**、**横向投票** |
| 组合性：object_id 当 feature（tire→car），只传 ID+pose | §3.13 后台整合 | 高层记忆**只组合摘要/ID**，不搬运完整细节；被规范化实体可**多上层复用** |
| LM 三种输出（pose+features / vote / goal）统一为 Message | §4.1 / §3.8 / §4.4 | 统一 Schema 承担多用途；**goal ↔ Agent 主动记忆/澄清** |
| DisplacementLM：位移预测、不一致即剪枝 | §3.4 | 与证据式**并存**：增加"关系/位移式"检索（落在建议 1 的记忆位移边上） |
| 评测正交维度 + 无监督推断 + 合并度指标 | §3 评测 | **分桶评测**；增加"**记忆切换**"测试；用**合并度指标**监控实体消解质量 |

**一句话**：Monty 给 lightmem 的最大方法论启示是"**先对齐参考系、再比较与融合**"——无论是传感器、投票还是跨 Agent 共享，
都必须先把不同来源映射到共同空间，否则融合就是"张冠李戴"。

---

## 5. 待落实的 lightmem.md 草稿（3 段）

> 以下草稿**尚未落盘**到 `lightmem.md`，供评审后再决定插入。建议标为 `v1.6`；三段均不触碰 §4.1 单一真源样例。

### 草稿 1 —— 插入位置：`§3.2 参考系坐标模块` 之后（新起 `§3.2.1`）

```markdown
### 3.2.1 记忆位移边（Memory Displacement Edges，拟新增 v1.6）

**职责：** 在扁平坐标之外，为记忆之间建立带类型、带权重的"位移边"，支撑多跳召回与关系推理。

**依据：** 吸收《千脑智能》参考系思想——Monty 的物体图中，节点是"特征@姿态"，边是 displacement。

**边字段（单一真源）：**

| 字段 | 说明 |
|---|---|
| `from_memory_id` / `to_memory_id` | 边的两端（ULID） |
| `relation` | `causal` / `temporal` / `task-flow` / `entity-alias` |
| `weight` | `[0,1]`，进入 §3.4 软排序 |
| `created_at` | 边建立时间（只增） |

**约束（安全）：** 位移边只用于软排序与多跳召回，不得跨越 §3.2 硬过滤边界；多跳设跳数上限 N（默认 2）
与权重衰减；L1 默认不建边，仅 L2 长期记忆建边。
```

### 草稿 2 —— 插入位置：`§3.2.1` 之后（或并入 `§3.14 实体链接`）

```markdown
### 3.2.2 跨 Agent 参考系对齐（强制前置，拟新增 v1.6）

**职责：** 跨 Agent 共享的记忆融合前，强制先做"参考系对齐"，防止同一实体在不同上下文中被误合并。

**规则：**

- 凡 `visibility ∈ {shared_user, tenant, cross_tenant}` 的跨 Agent 融合，必须先经 §3.14 实体消解对齐
  `entity/scene/task`。
- **未对齐的记忆只并列展示，禁止融合、禁止互相覆盖**；对齐失败时降级为"待人工确认"。

**依据：** Monty 中 vote 是"相对发送者身体"的，接收方必须变换到自己的参考系；且投票依赖特征在世界的相对排布。
```

### 草稿 3 —— 插入位置：`§3.4 投票融合模块` 之后（新起 `§3.4.1`）

```markdown
### 3.4.1 只加不减与相对容差（融合算子，拟新增 v1.6）

**职责：** 明确各融合信号的计分方向与容差语义，避免弱信号反向拖累整体。

**规则：**

- **只加不减：** 每个融合信号只贡献正向名次分；信号缺失或不匹配时记 `0`，不得记负分。
- **相对容差：** §3.4"接近融合"的 `(top1 - top2)/top1 < 20%` 中的 `20%` 明确定义为相对容差；
  超出容差的候选按 `0` 计（不扣分）。
- **保留绝对下限：** `top1 ≥ 绝对分下限（默认 0.6）` 为融合硬门槛，低于下限禁止融合。

**依据：** Monty 特征证据 `clip(tolerance - diff, 0, inf) / tolerance`（不匹配即 0），
且搜索半径内取 max 而非平均。
```

---

## 6. 附录

### 6.1 术语表（精简）

| 术语 | 含义 |
|---|---|
| Learning Module (LM) | 学习模块，对应皮质柱 |
| Sensor Module (SM) | 传感器模块，把原始观测转成 CMP 消息 |
| CMP | 皮质消息协议，"features at a pose" |
| Reference Frame | 参考系，记录特征相对位置 |
| Graph Memory | LTM，物体模型图（节点 + 位移边） |
| GridObjectModel | 体素网格 + 三张稀疏表 + top-k 图 |
| Hypotheses | 每个物体的假设空间（evidence/location/pose/possible） |
| MLH | Most Likely Hypothesis，最可能假设 |
| Displacement | 位移，两次观测间的位置变化 |
| Vote | LM 间的横向投票（候选姿态 + 物体 ID） |
| Episode / Step / Epoch | 一次物体交互 / 一次移动+观测 / 一个训练轮次 |

### 6.2 关键图示索引（`docs/figures/how-monty-works/`）

| 图 | 对应机制 |
|---|---|
| `sensor_moves.gif` / `object_moves.gif` | 传感器/物体移动与观测变化 |
| `evidence_update1.png` / `evidence_update2.png` | 证据随移动累积 |
| `movement_transform.gif` | 位移的参考系变换 |
| `voting_rf_transform.gif` | 投票的参考系变换 |
| `burst_sampling.gif` | 突发放采样（新物体建模） |
| `grid_types_blue_mug.png` / `graph_constrains.png` | 网格与约束图 |
| `grid_with_same_size.png` | 网格尺寸约束 |
| `radius.png` / `search_radius.png` | 搜索半径与自定义距离 |
| `evidence_init_all.png` | 假设初始化 |
| `possible_matches.png` | 候选收缩 |
| `hyp_driven_example.png` | 假设驱动探索 |
| `overview_diagram.png` / `scaling.png` | CMP 拓扑与扩展方式 |

### 6.3 参考

- 书籍：Jeff Hawkins，《千脑智能》（A Thousand Brains, 2021）
- 项目：Thousand Brains Project — Monty（MIT 许可）
- 仓库内文档：`docs/overview/glossary.md`、`docs/how-monty-works/learning-module/`、
  `docs/overview/architecture-overview/cortical-messaging-protocol.md`
- 关联设计文档：本仓 `lightmem.md`（§3.2 参考系、§3.4 投票融合、§4.1 统一 Schema、§5.3 固有价值）