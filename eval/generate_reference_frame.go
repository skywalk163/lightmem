//go:build ignore

// R4-B：reference_frame 变化评测集构造（任务书 §2 B.3.1）。
//
// 目的：构造「同一 query 主题、不同 reference_frame 坐标、期望命中不同记忆」的
// 对照样本，复检 signalReferenceFrame 信号在检索场景下是否具备区分度。
//
// 设计（纯模板替换，不调 LLM）：
//   - 30 组 × 2 条 = 60 条样本，bucket="reference_frame"。
//   - 每组 g 有三个互斥记忆（targetA / targetB / distractor），fact 形如
//     "客户X 的偏好是 周一"，主题几乎一致；唯一差异是 ReferenceFrame 坐标
//     （entity / project / scene）——这正是 R3-A §8 指出的「同 query 不同参考系」
//     场景，keyword/semantic/entity 信号无法区分，只有 reference_frame 能区分。
//   - 样本 A 查询端坐标 = targetA 坐标，期望命中 targetA；样本 B 同理。
//   - 若 reference_frame 信号无价值（权重 0），两组 top-1 只能靠 fact 值词元
//     随机，reference_frame 桶 top-1 命中率应显著低于 signalReferenceFrame 理论区分。
//
// 红线：
//   - 不调 LLM（纯模板替换）
//   - seed=42 固定采样（rand.New(rand.NewSource(42))）
//   - 不用 time.Now() / map 迭代序（本实现无随机源，seed 仅为占位）
//
// 运行（0.88 上）：
//
//	cd eval && go run ./generate_reference_frame.go -out ./dataset/reference_frame.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"

	"github.com/lightmem/kernel"
)

const (
	rfTenant = "eval-tenant"
	rfUser   = "eval-user"
	rfBucket = "reference_frame"
)

// 实体 / 主题 / 值 池（与合成集一致，确定性枚举）。
var rfEntities = []string{"客户A", "客户B", "客户C", "客户D", "客户E", "客户F", "客户G", "客户H", "客户I", "客户J"}
var rfTopics = []string{"偏好", "决策人", "预算", "产品", "服务"}
var rfValues = []string{"周一", "周二", "周三", "周四", "周五"}

// rfScene 三组参考系场景名（确定性，与 project 索引联动）。
var rfScenes = []string{"场景甲", "场景乙", "场景丙"}

// rfEvalItem 是 reference_frame 评测集样本结构。
// 与 run_kpi.go 的 EvalItem 对齐（后者新增 entity/project/scene/task 字段承载查询端坐标）。
type rfEvalItem struct {
	ID               string   `json:"id"`
	Query            string   `json:"query"`
	ExpectedMemoryID []string `json:"expected_memory_ids"`
	ExpectedFact     string   `json:"expected_fact"`
	Bucket           string   `json:"bucket"`
	IsTemporal       bool     `json:"is_temporal"`
	HasInjection     bool     `json:"has_injection"`
	HasPollution     bool     `json:"has_pollution"`
	Tenant           string   `json:"tenant"`
	User             string   `json:"user"`
	// 查询端 reference_frame 坐标（R4-B 新增），供 signalReferenceFrame 使用。
	Entity  string `json:"entity,omitempty"`
	Project string `json:"project,omitempty"`
	Scene   string `json:"scene,omitempty"`
	Task    string `json:"task,omitempty"`
	// Memory 是该样本的期望记忆（target）；Hop1/Hop2 是需一并物化的干扰记忆。
	Memory     *kernel.Memory `json:"memory"`
	Hop1Memory *kernel.Memory `json:"hop1_memory,omitempty"`
	Hop2Memory *kernel.Memory `json:"hop2_memory,omitempty"`
}

// rfSeed 返回固定 seed 的随机源（seed=42，红线 5）。
func rfSeed() *rand.Rand { return rand.New(rand.NewSource(42)) }

// rfMem 构造一条参考系记忆。
// fact 是记忆正文；rf 坐标由 entity/project/scene 决定，task 留空（保持三坐标纯差异）。
func rfMem(memID, fact, entity, project, scene string, layer kernel.MemoryLayer) *kernel.Memory {
	return &kernel.Memory{
		MemoryID:       memID,
		IdempotencyKey: memID,
		TenantID:       rfTenant,
		UserID:         rfUser,
		Visibility:     kernel.VisTenant,
		MemoryKind:     kernel.KindFact,
		Fact:           fact,
		ReferenceFrame: kernel.ReferenceFrame{
			Entity:  entity,
			Project: project,
			Scene:   scene,
		},
		Confidence: 0.9,
		Source:     "chat",
		Version:    1,
		Layer:      layer,
		Provenance: kernel.ProvNormal,
		Permission: kernel.Permission{Owner: rfUser},
	}
}

// genReferenceFrame 生成 30 组 × 2 = 60 条 reference_frame 变化样本。
//
// 每组 g：
//   - ent = 实体池[g % 10]；topic = 主题池[(g/10) % 5]；
//   - vA/vB/vC = 值池[(g+i) % 5]（i=0,1,2，三者互不相同）；
//   - targetA/B/C 分别落在 project[0/1/2] + scene[0/1/2]；
//   - 样本 A 查询端坐标 = targetA 坐标 → 期望 targetA；样本 B 同理。
//
// 全部确定性（无随机源），seed 仅占位。
func genReferenceFrame() []rfEvalItem {
	var out []rfEvalItem
	for g := 0; g < 30; g++ {
		ent := rfEntities[g%len(rfEntities)]
		topic := rfTopics[(g/len(rfEntities))%len(rfTopics)]
		v := rfValues[g%len(rfValues)]

		projA, projB, projC := fmt.Sprintf("项目%d", g%3), fmt.Sprintf("项目%d", (g+1)%3), fmt.Sprintf("项目%d", (g+2)%3)
		sceneA := rfScenes[g%len(rfScenes)]
		sceneB := rfScenes[(g+1)%len(rfScenes)]
		sceneC := rfScenes[(g+2)%len(rfScenes)]

		// 三条记忆的 fact **完全一致**（如 "客户A的偏好是周一"）：
		// keyword/semantic/entity 三路信号在三条候选上同分，无法区分；
		// 只有 signalReferenceFrame（entity/project/scene 坐标）能区分 target 与干扰项。
		// 这是对 R3-A §8「reference_frame 信号无区分度」的严格复检。
		fact := fmt.Sprintf("%s的%s是%s", ent, topic, v)

		// 每条样本使用独立 memory id（后缀 qA/qB），避免评测器逐条写入时
		// 同组 qB 的 target 与 qA 共享同一 id 触发 ErrIdempotentNoop 被 continue 跳过。
		suffixA := "qA"
		suffixB := "qB"

		// memory id 命名刻意让 target 的字典序**最大**（Z > A/B）：
		// 若 reference_frame 权重为 0，三条候选融合分完全相同，RRF 退化为按 MemoryID 升序，
		// target 必然 miss——这样 reference_frame=0.0 时评测集给出「信号无效」的明确证据，
		// reference_frame>0 时 target 命中，二者形成严格对照。
		idAA := fmt.Sprintf("ref-%03dZ-%s", g, suffixA) // target（期望）
		idAB := fmt.Sprintf("ref-%03dA-%s", g, suffixA) // distractor
		idAC := fmt.Sprintf("ref-%03dB-%s", g, suffixA) // distractor
		idBA := fmt.Sprintf("ref-%03dZ-%s", g, suffixB) // target（期望）
		idBB := fmt.Sprintf("ref-%03dA-%s", g, suffixB) // distractor
		idBC := fmt.Sprintf("ref-%03dB-%s", g, suffixB) // distractor

		// 样本 A 的三条记忆：target（期望）/distA/distB（干扰），fact 完全一致，
		// 仅 ReferenceFrame 坐标不同（project/scene 错开）。
		targetA := rfMem(idAA, fact, ent, projA, sceneA, kernel.LayerL1)
		distA1 := rfMem(idAB, fact, ent, projB, sceneB, kernel.LayerL1)
		distA2 := rfMem(idAC, fact, ent, projC, sceneC, kernel.LayerL1)

		// 样本 B 的三条记忆：targetB（期望）/distB1/distB2（干扰）。
		targetB := rfMem(idBA, fact, ent, projB, sceneB, kernel.LayerL1)
		distB1 := rfMem(idBB, fact, ent, projA, sceneA, kernel.LayerL1)
		distB2 := rfMem(idBC, fact, ent, projC, sceneC, kernel.LayerL1)

		// 样本 A：查询端坐标 = targetA 坐标；query **不**嵌入 project 名，
		// 使 keyword/semantic/entity 三路信号在三条候选上得分完全一致，
		// 只有 signalReferenceFrame 能区分 targetA 与干扰项（R3-A §8 复检核心）。
		out = append(out, rfEvalItem{
			ID:               fmt.Sprintf("ref-%03d-qA", g),
			Query:            fmt.Sprintf("%s的%s是什么", ent, topic),
			ExpectedMemoryID: []string{idAA},
			ExpectedFact:     fact,
			Bucket:           rfBucket,
			IsTemporal:       false,
			HasInjection:     false,
			HasPollution:     false,
			Tenant:           rfTenant,
			User:             rfUser,
			Entity:           ent,
			Project:          projA,
			Scene:            sceneA,
			Task:             "",
			Memory:           targetA,
			Hop1Memory:       distA1,
			Hop2Memory:       distA2,
		})
		// 样本 B：查询端坐标 = targetB 坐标。
		out = append(out, rfEvalItem{
			ID:               fmt.Sprintf("ref-%03d-qB", g),
			Query:            fmt.Sprintf("%s的%s是什么", ent, topic),
			ExpectedMemoryID: []string{idBA},
			ExpectedFact:     fact,
			Bucket:           rfBucket,
			IsTemporal:       false,
			HasInjection:     false,
			HasPollution:     false,
			Tenant:           rfTenant,
			User:             rfUser,
			Entity:           ent,
			Project:          projB,
			Scene:            sceneB,
			Task:             "",
			Memory:           targetB,
			Hop1Memory:       distB1,
			Hop2Memory:       distB2,
		})
	}
	return out
}

func main() {
	_ = rfSeed() // 占位：本实现无随机源，seed=42 仅为红线合规声明。
	outPath := flag.String("out", "./dataset/reference_frame.jsonl", "输出 JSONL 路径")
	flag.Parse()

	items := genReferenceFrame()
	if len(items) < 50 {
		fmt.Fprintf(os.Stderr, "FATAL: reference_frame 样本数 %d < 50，任务书 B.5 验收不通过\n", len(items))
		os.Exit(1)
	}

	f, err := os.Create(*outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *outPath, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			fmt.Fprintf(os.Stderr, "encode %s: %v\n", it.ID, err)
			os.Exit(1)
		}
	}
	fmt.Printf("R4-B: reference_frame 评测集已生成 -> %s（%d 条，seed=42，纯模板确定性）\n",
		*outPath, len(items))
}
