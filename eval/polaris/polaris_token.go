// 北极星评测子包（R5-D）：M1 零 token 覆盖率 + M2 控制面 token 占比 + 控制面压缩。
//
// 定位：独立 subpackage（package main），不 import eval 主包（run_kpi.go 是
// //go:build ignore 的单文件 main），也不改动 kernel 任何语义——只消费
// kernel 公开 API（StorageBackend / APIServer / RankFusion / Embedder）。
//
// 红线遵守：
//   - 不调 LLM：M1/M2 全部基于确定性字符串/日历推算与查表向量；
//   - 无随机源：token 估算、压缩规则、fact 判定均逐位确定；
//     polaris_eval 默认 -now-fixed=2026-10-13 固定基准时刻，杜绝 time.Now()
//     进入 freshness 信号导致复跑不一致；
//   - 只增不改：不改 Memory/RouterContext/RecalledCandidate 字段语义。
//
// 口径真源：.dumate/inbox/_R5_检索能力升级并行任务书.md §4（D 路）。
package main

// ------------------------------------------------------------------
// 确定性 token 估算口径（M1/M2 共用）。
//
// 背景：北极星指标要求把 token 拆成「控制面 / 数据面」分别计量
// （docs/光明记忆应用包生态与差异化定位_v1.0.md §8.3 分离度量）。
// 本包不调用任何 tokenizer 服务，采用**确定性词元近似口径**：
//   - CJK 表意字符（U+3400..U+9FFF 主区 + 扩展A + U+F900..U+FAFF 兼容）
//     每字 1 token（中文场景一字一义，表意密度即光明语言的压缩维度）；
//   - ASCII 字母数字连续段（word）每段 1 token；
//   - 空白与标点不计（指令模板的标点在压缩前后对称存在，不影响对比口径）。
//
// 该口径是**相对比较口径**（压缩前后、控制面/数据面占比），非计费口径；
// 同一输入必得同一输出（无随机源、无 map 迭代序依赖）。
// ------------------------------------------------------------------

// EstimateTokens 返回文本的确定性 token 估算值。
// 计数时机：ASCII 词首计 1、CJK 单字各计 1；状态切换只重置 inWord，不计数
// （词首已计过，词尾/标点处不得重复计——R5-D 首版曾在此双重计数，单测锁定）。
func EstimateTokens(s string) int {
	tokens := 0
	inWord := false
	for _, r := range s {
		switch {
		case isCJKRune(r):
			// CJK 单字成词：若前接 ASCII word，仅关闭词状态（该词词首已计）。
			inWord = false
			tokens++
		case isASCIIWordRune(r):
			if !inWord {
				tokens++
				inWord = true
			}
		default:
			// 空白/标点：仅关闭词状态，不计数。
			inWord = false
		}
	}
	return tokens
}

// isCJKRune 判定表意字符（中文/日文汉字/韩语汉字、扩展A、兼容区）。
func isCJKRune(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0xF900 && r <= 0xFAFF)
}

// isASCIIWordRune 判定 ASCII 字母数字（词元字符）。
func isASCIIWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// ------------------------------------------------------------------
// 控制面 / 数据面 token 账目（M2 口径）。
//
// 控制面：query + 路由 prompt + 召回 prompt（任务书 §D.3.2）。
//   当前 Go kernel 的编排是代码（0 token）；此处的路由/召回指令模板是
//   「prompt 表达口径」的占位表达，用于与 Python+Prompt 基线做同口径对比
//   （定位文档 §7 M2：编排/策略/配置表达所耗 token / 总 token）。
// 数据面：召回候选记忆正文（top-K fact 文本）。
// ------------------------------------------------------------------

const (
	// RouterDirectivePlain 路由指令（自然语言冗长形态，压缩前基线）。
	RouterDirectivePlain = "路由查询到场景与实体："
	// RecallDirectivePlain 召回指令（自然语言冗长形态，压缩前基线）。
	RecallDirectivePlain = "为以下查询检索相关记忆："
	// RouterDirectiveLight 路由指令（光明语言压缩形态）。
	RouterDirectiveLight = "路由："
	// RecallDirectiveLight 召回指令（光明语言压缩形态）。
	RecallDirectiveLight = "召回："
)

// TokenAccount 单条查询的 token 账目。
type TokenAccount struct {
	Query         string `json:"query"`
	QueryTokens   int    `json:"query_tokens"`
	ControlTokens int    `json:"control_tokens"` // query + 路由指令 + 召回指令
	DataTokens    int    `json:"data_tokens"`    // top-K 候选记忆正文
	TotalTokens   int    `json:"total_tokens"`   // control + data
}

// ControlPlaneText 返回控制面表达文本（路由指令 + query + 召回指令 + query）。
// compressed=false 用冗长指令模板；true 用光明语言短指令 + 压缩后 query。
func ControlPlaneText(query string, compressed bool) string {
	rd, cd := RouterDirectivePlain, RecallDirectivePlain
	q := query
	if compressed {
		rd, cd = RouterDirectiveLight, RecallDirectiveLight
		q = CompressQuery(query)
	}
	return rd + q + "\n" + cd + q
}

// ControlPlaneTokens 返回控制面 token 数（query + 路由/召回指令，含 query 两次出现：
// 一次路由一次召回——与 prompt 管线中 query 被两段指令各自包裹的形态一致）。
func ControlPlaneTokens(query string, compressed bool) int {
	return EstimateTokens(ControlPlaneText(query, compressed))
}
