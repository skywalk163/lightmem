// Package bench 把三个公开基准（LongMemEval / LoCoMo / BEAM）的原始格式
// 统一解析为 LIMS kernel.Memory Schema（任务书 §3.2）。
//
// 红线：本包只做格式适配、字段映射与分桶标注，不调 LLM 生成/改写任何 query 或答案。
package bench

import "github.com/lightmem/kernel"

// Source 标识数据来源。
type Source string

const (
	SourceLongMemEval Source = "longmemeval"
	SourceLoCoMo      Source = "locomo"
	SourceBeam        Source = "beam"
)

// 分桶常量（与 eval 评测口径一致）。
const (
	BucketSingleHop  = "single-hop"
	BucketMultiHop   = "multi-hop"
	BucketTemporal   = "temporal"
	BucketOpenDomain = "open-domain"
)

// RawTurn 归一化对话轮次。
type RawTurn struct {
	Speaker string
	Text    string
	Ts      string // RFC3339 或原始日期文本
}

// RawQuestion 归一化问题（含答案与期望召回的记忆 id）。
type RawQuestion struct {
	ID                string
	Query             string
	Expected          []string // expected fact(s)
	ExpectedFact      string   // 主 expected fact（评测用）
	Bucket            string   // single-hop | multi-hop | temporal | open-domain
	IsTemporal        bool
	AsOf              string // 时序用，可空
	ExpectedMemoryIDs []string
}

// RawConversation 归一化对话：若干轮次 + 若干问题。
type RawConversation struct {
	ID     string
	Tenant string
	User   string
	Turns  []RawTurn
	// Questions 由 Parse 填充；ToQuestions 返回。
	Questions []RawQuestion
	// MemoryByID 由 Parse/适配器填充：问题期望召回的事实记忆（已映射到 LIMS Schema）。
	// ToMemories 把它导出为 []kernel.Memory。
	MemoryByID map[string]kernel.Memory
}

// Adapter 把单一基准的原始文件解析为统一中间表示，并导出为 kernel.Memory。
type Adapter interface {
	Source() Source
	// Parse 读取原始文件，返回归一化对话列表。
	Parse(path string) ([]RawConversation, error)
	// ToMemories 把对话轮次构造为 LIMS Memory（期望召回的事实记忆）。
	ToMemories(conv *RawConversation) []kernel.Memory
	// ToQuestions 返回该对话的问题集。
	ToQuestions(conv *RawConversation) []RawQuestion
}
