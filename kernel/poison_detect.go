// 投毒检测（§7.5.1，R15-A）：把「检测（置标签）」从「处置（降权/截断）」里分离出来。
//
// 真源：lightmem.md §7.5「记忆投毒防护」、§7.5.1「检测与处置两阶段」、§3.15 写策略插件。
//
// 为什么单独成文
//   - R14 及以前只有 PoisoningGuardPolicy（writepolicy.go:241）——它**只处置不检测**：
//     首行判 `m.Provenance != ProvInjectionSuspect` 即放行，而全库生产代码没有任何写路径
//     会置该标签，所以整条防线在实现上是空的。
//   - 本文件补上「检测」阶段：DetectInjection 是纯函数、确定性、无 I/O、无随机源；
//     MarkSuspect 只把判定落成既有 Schema 的标签与 confidence 处置。
//
// 确定性红线（与 forget_drift / edge_persist 同族）
//   - 不读 time.Now()、不调 rand、不遍历 map 求结果；Signals 末尾显式排序；
//   - 规则表按 ID 字典序静态声明，逐条独立求值，命中集合与打分不依赖求值顺序；
//   - DetectInjection 只读 meta.Existing（判定「同一注入模式重复写入」），
//     禁止读 meta.Now / meta.Actor 参与打分（time.Time 的 location 参与 ==，会引入时钟依赖）。
//
// 接入方式（关键）：注入检测器用 WithInjectionDetection 显式追加到既有注册表，
// **不改 DefaultWritePolicies()**。理由：DefaultWritePolicies() 的 6 条策略数与名称由
// writepolicy_test.go:46 的 TestDefaultWritePoliciesInstalled 逐位锚定，插入第 7 条等于改测试。
// 任务书 §A.2 明确授权此退路：「若接入导致任何既有测试输出变化，不得改测试，
// 改为由调用方显式注入该策略」。
package kernel

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// ------------------------------------------------------------------
// 严重度与判定结果
// ------------------------------------------------------------------

// InjectionSeverity 嫌疑强度。
type InjectionSeverity int

const (
	SevNone InjectionSeverity = iota // 未命中
	SevLow                           // 可疑（跨 Agent 传播、工具滥用、变体改写等）
	SevHigh                          // 明确越权（直接指令、系统提示泄露、角色劫持）
)

// String 返回稳定的文本形式（供报告与审计，不进 Schema）。
func (s InjectionSeverity) String() string {
	switch s {
	case SevLow:
		return "low"
	case SevHigh:
		return "high"
	default:
		return "none"
	}
}

// InjectionVerdict 单条事实的判定结果。纯值，可安全跨 goroutine 传阅。
type InjectionVerdict struct {
	Severity InjectionSeverity `json:"severity"`
	Score    float64           `json:"score"` // ∈[0,1]，确定性累加后 clamp
	Signals  []string          `json:"signals"`
	Quote    string            `json:"quote,omitempty"` // 命中片段，≤ maxEvidenceChars
}

// IsSuspect 是否命中任一规则。
func (v InjectionVerdict) IsSuspect() bool { return v.Severity > SevNone }

// HasSignal 是否含指定信号 ID。
func (v InjectionVerdict) HasSignal(id string) bool {
	for _, s := range v.Signals {
		if s == id {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------
// 规则表
// ------------------------------------------------------------------

// InjectionRule 单条检测规则。ID 必须全局唯一且为稳定字符串（进审计与报告）。
type InjectionRule struct {
	ID     string // 规则 ID（审计用，稳定排序键）
	Kind   string // cn / en（语言族，供按族统计）
	Sev    InjectionSeverity
	Weight float64        // 贡献分，累加后 clamp 到 [0,1]
	Re     *regexp.Regexp // RE2，无回溯
}

// ruleHit 一次命中（每规则至多计一次）。
type ruleHit struct {
	id     string
	sev    InjectionSeverity
	weight float64
	match  string
}

// InjectionRuleCatalog 返回规则表只读副本，供评测 runner 与报告读取规则元数据。
// 返回副本以避免调用方改写包内规则表；rule 自身为值语义。
func InjectionRuleCatalog() []InjectionRule {
	rules := injectionRules()
	out := make([]InjectionRule, len(rules))
	copy(out, rules)
	return out
}

// injectionRules 返回规则表。
//
// 声明顺序即 ID 字典序（TestInjectionRuleTableSorted 锁定），
// 因此 matchRules 的输出天然有序，Quote 的平票仲裁只需比较 ID。
//
// 权重口径：高危直指令（en_direct_output_system / en_role_hijack）单独即 ≥0.70；
// 中文「输出系统提示词」单独 0.60——低于高危阈值，须叠加第二条信号或进入 IndirectMode
// 才落 High，用于压低「请输出系统时间」这类正常话术的误报。
func injectionRules() []InjectionRule {
	return []InjectionRule{
		// ---------- 中文 ----------
		{
			ID:   "cn_cross_agent_spread",
			Kind: "cn", Sev: SevLow, Weight: 0.25,
			Re: regexp.MustCompile(`(?:传播|共享|同步|转发|推送|扩散)[^，。！？]{0,4}?(?:所有|每个|全部|其余)(?: ?[Aa]gent| ?智能体| ?机器人| ?助手| ?租户| ?团队)`),
		},
		{
			ID:   "cn_direct_ignore_above",
			Kind: "cn", Sev: SevHigh, Weight: 0.70,
			Re: regexp.MustCompile(`(?:忽略|无视|跳过|覆盖|放弃)(?:以上|先前|之前|上述|之上|之前所有|所有|全部|系统|之前的|之上的)?(?:的)?(?:全部|所有)?(?:指令|指示|规则|要求|设定|约束|提示词|配置|策略)`),
		},
		{
			ID:   "cn_direct_output_system",
			Kind: "cn", Sev: SevHigh, Weight: 0.60,
			Re: regexp.MustCompile(`(?:输出|打印|显示|复述|展示|泄露|说出|透露|返回|写出|粘贴|念出|背诵)(?:你的|系统|完整|全部)?(?:的)?(?:提示词|指令|配置|设置|规则|原文|定义|全文)`),
		},
		{
			ID:   "cn_negation_system",
			Kind: "cn", Sev: SevHigh, Weight: 0.60,
			Re: regexp.MustCompile(`(?:请不要忘记|别忘记|千万别|请不要|请别|请勿|不要|不用|无需|不必)(?:忘记|记起|忽略|绕过|跳过|漏掉|遗漏|丢掉|隐瞒|掩盖|删除|修改|泄露|输出|篡改|覆盖|执行)?\s*(?:(?:以上|先前|之前|所有|系统|全部|之前的|上面的)\s*){0,2}(?:的)?(?:指令|指示|规则|要求|设定|约束|提示词|配置|策略)`),
		},
		{
			ID:   "cn_persist_override",
			Kind: "cn", Sev: SevHigh, Weight: 0.65,
			Re: regexp.MustCompile(`(?:记住以后|从现在起|从此刻起|今后|以后每次|以后都|永远|此后|接下来的)(?:一律|都要|必须|请)?(?:忽略|绕过|跳过|不要|无需|不必|改为|变成|输出|删除|泄露)`),
		},
		{
			ID:   "cn_relay",
			Kind: "cn", Sev: SevHigh, Weight: 0.65,
			Re: regexp.MustCompile(`(?:另一个? ?[Aa]gent|其他 ?[Aa]agent|上游 ?[Aa]agent|助手|机器人|同事|对方)(?:说|说道|告诉我|要求|回复|回复说|留言|表示|提到)[^。！？]{0,20}?(?:忽略|绕过|跳过|泄露|输出系统|输出你的|不要告诉用户|不要执行|篡改)`),
		},
		{
			ID:   "cn_role_hijack",
			Kind: "cn", Sev: SevHigh, Weight: 0.80,
			Re: regexp.MustCompile(`(?:你现在是|现在你是|假装你是|扮演成|变成|切换成|切换为|当作|冒充)(?:一个?|新的?|某种|某个|越狱)?(?:AI|人工智能|智能体|助手|机器人|管理员|开发者模式|客服系统|越狱模式|另一个助手)`),
		},
		{
			ID:   "cn_tool_abuse",
			Kind: "cn", Sev: SevLow, Weight: 0.40,
			Re: regexp.MustCompile(`(?:^|[，。！？：；、])(?:请|我要|让我|立即|马上|帮我|就|去)?\s*(?:执行|调用|运行|触发|跑)\s*(?:所有|任意|全部|每个|外部|未知|任意一个)\s*(?:的)?\s*(?:工具|函数|插件|脚本|命令|接口|API)`),
		},
		// ---------- 英文 ----------
		{
			ID:   "en_cross_agent_spread",
			Kind: "en", Sev: SevLow, Weight: 0.25,
			Re: regexp.MustCompile(`\b(?:propagate|share|sync|forward|push|broadcast|send|distribute|replicate)\s+[^.!?\n]{0,20}?(?:to\s+|towards\s+|with\s+)?(?:all|every|each|other|remaining)\s+(?:agent|agents|assistant|assistants|bot|bots|tenant|tenants|instance|instances)\b`),
		},
		{
			ID:   "en_direct_ignore_above",
			Kind: "en", Sev: SevHigh, Weight: 0.70,
			Re: regexp.MustCompile(`\b(?:ignore|disregard|override|forget|bypass|discard|skip|drop)\s+(?:all\s+)?(?:the\s+)?(?:previous|prior|above|earlier|former|system|your|these|those|original)\s+(?:instructions|prompts|rules|requirements|settings|configuration|constraints|context)\b`),
		},
		{
			ID:   "en_direct_output_system",
			Kind: "en", Sev: SevHigh, Weight: 0.75,
			Re: regexp.MustCompile(`\b(?:output|print|show|reveal|leak|repeat|recite|display|paste|dump|extract|give me|tell me)\s+(?:your\s+)?(?:the\s+)?(?:system\s+)?(?:prompt|instructions|rules|configuration|settings|developer message|system message|context)\b`),
		},
		{
			ID:   "en_negation_system",
			Kind: "en", Sev: SevHigh, Weight: 0.60,
			Re: regexp.MustCompile(`\b(?:do not|don't|never|no need to)\s+(?:forget|ignore|bypass|skip|delete|modify|leak|output|alter|override|execute)\s+(?:the\s+|all\s+|your\s+|any\s+)?(?:above\s+|previous\s+|prior\s+|earlier\s+|former\s+|original\s+|system\s+)?(?:instructions|rules|requirements|settings|constraints|configuration|prompt|context)\b`),
		},
		{
			ID:   "en_persist_override",
			Kind: "en", Sev: SevHigh, Weight: 0.65,
			Re: regexp.MustCompile(`\b(?:from now on|from this point on|starting now|going forward|always|forever|every time)\s+(?:you\s+must\s+|you\s+should\s+|please\s+|just\s+)?\s*(?:ignore|bypass|override|forget|skip|leak)\b`),
		},
		{
			ID:   "en_relay",
			Kind: "en", Sev: SevHigh, Weight: 0.65,
			Re: regexp.MustCompile(`\b(?:another|other|upstream|downstream)\s+(?:agent|assistant|bot)\b[^.!?]{0,40}?\b(?:ignore|bypass|override|leak|output your system|don't tell the user|do not execute|delete the user)\b`),
		},
		{
			ID:   "en_role_hijack",
			Kind: "en", Sev: SevHigh, Weight: 0.80,
			Re: regexp.MustCompile(`\b(?:you\s+are\s+now|act\s+as|pretend\s+(?:to\s+)?be|switch\s+to|become)\s+(?:a\s+|an\s+)?(?:new\s+|another\s+)?(?:AI|assistant|agent|bot|admin|developer\s+mode|DAN|jailbreak|evil\s+assistant|mode)\b`),
		},
		{
			ID:   "en_tool_abuse",
			Kind: "en", Sev: SevLow, Weight: 0.40,
			Re: regexp.MustCompile(`\b(?:execute|call|invoke|run|trigger)\s+(?:(?:all|any|every|external|unknown|other|unverified)\s+){1,2}(?:tool|tools|function|functions|plugin|plugins|script|scripts|command|commands|API|APIs|endpoint|endpoints)\b`),
		},
	}
}

// repeatSuspectExisting 是「已有条目已带投毒标签、同一模式被重复写入」的独立判定规则。
// 单独定义（不进 injectionRules 的 ID 排序域），理由：
//   - 它的求值依赖 meta.Existing 而非文本本身，与模式表语义不同源；
//   - 它不会成为独立于模式表的落标签依据（须 meta.Existing 已带嫌疑标签）。
var repeatSuspectExisting = ruleHit{
	id:     "repeat_suspect_existing",
	sev:    SevLow,
	weight: 0.30,
	match:  "existing memory already marked prompt_injection_suspect",
}

// ------------------------------------------------------------------
// 配置
// ------------------------------------------------------------------

// IndirectDetectMode 间接注入面开关。
//
// 「间接注入」指指令不是写在主 fact 里、而是嵌在被引用内容（source_evidence.snippet、
// 外部网页、另一 Agent 的回传）里。这类文本进入记忆系统时通常以「引文」形式出现，
// 攻击收益更高（用户不会逐字审阅引文），所以同一模式在此面上应给更强权重。
type IndirectDetectMode int

const (
	DetectIndirectOff  IndirectDetectMode = iota // 默认：只按主文本判定
	DetectIndirectOn                             // 显式按间接面判定（调用方判定该文本是引文/外部内容）
	DetectIndirectAuto                           // 存在 SourceEvidence.Snippet 时按 On 处理（策略层自动判定）
)

// DetectConfig 检测配置。零值即保守默认：不启用间接面、不启用重复写入判定。
type DetectConfig struct {
	IndirectMode IndirectDetectMode // 默认 DetectIndirectOff
	EnableRepeat bool               // 默认 false：读取 meta.Existing 做重复写入判定
}

// PoisonDetectConfig 处置配置（MarkSuspect 用）。
type PoisonDetectConfig struct {
	// HighSeverityConfidenceCap 高危命中时的 confidence 封顶值。
	// >0 生效，0 表示不设封顶（只落标签）。
	HighSeverityConfidenceCap float64
}

// DefaultDetectConfig 保守默认：直接检测开启、间接面与重复写入判定由调用方显式开启。
func DefaultDetectConfig() DetectConfig {
	return DetectConfig{IndirectMode: DetectIndirectOff, EnableRepeat: false}
}

// DefaultPoisonDetectConfig 与 writepolicy.go 的 PoisoningGuardPolicy 处置口径对齐（0.3 封顶）。
func DefaultPoisonDetectConfig() PoisonDetectConfig {
	return PoisonDetectConfig{HighSeverityConfidenceCap: 0.3}
}

// ------------------------------------------------------------------
// 确定性归一化与匹配
// ------------------------------------------------------------------

// 扫描范围：全量扫描，不设截断上限。
// 理由：RE2 无回溯、线性时间，检测成本与输入长度成线性；若截断，长文本尾部追加的注入
// 会变成确定性盲区——真实投毒正是这一形态，等于用漏检换性能，代价方向错误。
// 写入路径对 fact 长度已有约束，线性成本可预期；同输入恒得同输出。

// variantRunes 系统关键字改写变体：攻击者用这些不可见/等价字符打断 token 切分以绕过朴素过滤。
// 只要出现即计一条低危信号（variant_steganographic），不改变既有模式表的求值结果。
var variantRunes = []rune{'\u00a0', '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u3000'}

const variantSignalID = "variant_steganographic"

// hasVariantRunes 报告文本是否含任一改写变体字符。
func hasVariantRunes(s string) bool {
	for _, r := range s {
		for _, v := range variantRunes {
			if r == v {
				return true
			}
		}
	}
	return false
}

// matchRules 按规则表逐条求值，返回按规则 ID 字典序排列的命中集合（每规则至多计一次）。
func matchRules(input string) []ruleHit {
	rules := injectionRules()
	hits := make([]ruleHit, 0, len(rules))
	for _, r := range rules {
		if m := r.Re.FindString(input); m != "" {
			hits = append(hits, ruleHit{id: r.ID, sev: r.Sev, weight: r.Weight, match: m})
		}
	}
	return hits // 规则表已按 ID 排序，输出天然有序
}

// truncateRunes 按 rune 截断（不产生半个 rune，避免写入非法 UTF-8）。
func truncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func clamp01f(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// ------------------------------------------------------------------
// 检测
// ------------------------------------------------------------------

// DetectInjection 确定性检测一条事实文本中的提示注入模式。
//
// 输入只读；同输入同输出。禁止依赖 map 迭代序、time.Now、rand。
// meta 只用于读取 meta.Existing（重复写入判定），不参与时间/身份相关打分。
func DetectInjection(fact string, meta WriteMeta, cfg DetectConfig) InjectionVerdict {
	var v InjectionVerdict

	if fact == "" {
		return v // 空输入 → SevNone，Score 0
	}

	indirect := cfg.IndirectMode == DetectIndirectOn

	hits := matchRules(fact)
	if hasVariantRunes(fact) {
		hits = append(hits, ruleHit{id: variantSignalID, sev: SevLow, weight: 0.30, match: "invisible/variant character present"})
	}

	if cfg.EnableRepeat && meta.Existing != nil && meta.Existing.Provenance == ProvInjectionSuspect {
		hits = append(hits, repeatSuspectExisting)
	}

	if len(hits) == 0 {
		return v
	}

	// 打分与严重度：间接面抬高权重；已含高危即高危；否则按累加分判定。
	sev := SevLow
	score := 0.0
	for i := range hits {
		w := hits[i].weight
		if indirect {
			w *= 1.25
		}
		hits[i].weight = clamp01f(w)
		score += hits[i].weight
		if hits[i].sev == SevHigh {
			sev = SevHigh
		}
	}
	score = clamp01f(score)

	// 间接面：已命中即至少低危；累加分够高时升高危。
	if indirect {
		if sev == SevNone {
			sev = SevLow
		}
		if score >= 0.60 {
			sev = SevHigh
		}
	} else if sev == SevNone {
		sev = SevLow
	}

	signals := make([]string, 0, len(hits))
	for _, h := range hits {
		signals = append(signals, h.id)
	}
	sort.Strings(signals) // 显式排序：确定性红线
	if indirect {
		signals = append(signals, "indirect_context")
		sort.Strings(signals)
	}

	// Quote：权重最高的命中片段；平票时取 ID 字典序最小（hits 已按 ID 排序，严格大于比较即可）。
	best := hits[0]
	for _, h := range hits[1:] {
		if h.weight > best.weight {
			best = h
		}
	}

	v.Severity = sev
	v.Score = score
	v.Signals = signals
	v.Quote = truncateRunes(best.match, maxEvidenceChars)
	return v
}

// ------------------------------------------------------------------
// 落标签（处置）
// ------------------------------------------------------------------

// MarkSuspect 把判定落成 §7.5 的既有标签与 confidence 处置。
//
// 只改 m.Provenance 与 m.Confidence 两个字段，不触碰 Schema 其他字段；
// 无嫌疑判定或非高危封顶时可能不改动任何字段，此时返回 false。
// 该函数幂等：对已带标签的记忆重复调用结果不变。
func MarkSuspect(m *Memory, v InjectionVerdict, cfg PoisonDetectConfig) bool {
	if m == nil || !v.IsSuspect() {
		return false
	}
	changed := false
	if m.Provenance != ProvInjectionSuspect {
		m.Provenance = ProvInjectionSuspect
		changed = true
	}
	if cfg.HighSeverityConfidenceCap > 0 && v.Severity == SevHigh &&
		m.Confidence > cfg.HighSeverityConfidenceCap {
		m.Confidence = cfg.HighSeverityConfidenceCap
		changed = true
	}
	return changed
}

// ------------------------------------------------------------------
// 写策略接入（显式 opt-in，不改 DefaultWritePolicies）
// ------------------------------------------------------------------

// InjectionDetectorPolicy 在写入路径上做投毒检测并落标签。
//
// 设计要点：
//   - 本策略**只检测与置标签，从不拒绝写入**（Validate 恒返回 nil）。
//     拒绝、降权、截断证据的「处置」仍由 PoisoningGuardPolicy 承担——两阶段分离。
//   - ApplyGuard=true（默认）时，命中后立即内联调用既有 PoisoningGuardPolicy.Validate
//     完成处置。这样本策略在注册表中的**位置不影响正确性**：既可用 Register 追加到末尾，
//     也可插到 poisoning_guard 之前；后者会让 guard 幂等地再跑一次（confidence 已封顶、
//     snippet 已截断，重复执行无副作用）。
//   - 对无嫌疑文本恒为 no-op：DetectInjection 返回 SevNone 时不做任何写入，
//     由此保证既有写路径行为逐位不变。
type InjectionDetectorPolicy struct {
	DetectCfg  DetectConfig
	MarkCfg    PoisonDetectConfig
	ApplyGuard bool // 默认 true：命中后内联执行既有处置
}

// Name 返回策略名（进注册表与审计）。
func (p *InjectionDetectorPolicy) Name() string { return "injection_detection" }

// Validate 执行检测 → 落标签 →（可选）处置。恒返回 nil，不拒绝写入。
func (p *InjectionDetectorPolicy) Validate(ctx context.Context, m *Memory, meta WriteMeta) error {
	if m == nil {
		return nil
	}
	cfg := p.DetectCfg
	if cfg.IndirectMode == DetectIndirectAuto {
		if m.SourceEvidence != nil && strings.TrimSpace(m.SourceEvidence.Snippet) != "" {
			cfg.IndirectMode = DetectIndirectOn
		}
	}

	// TrimSpace：首尾空白不影响判定，去空白后同义文本得同判定。
	v := DetectInjection(strings.TrimSpace(m.Fact), meta, cfg)
	if !v.IsSuspect() {
		return nil
	}

	MarkSuspect(m, v, p.MarkCfg)
	if p.ApplyGuard && v.Severity == SevHigh {
		// 仅高危触发处置：低危只落标签、不动 confidence 与证据片段，
		// 避免把「可疑」与「确认越权」的干扰强度混为一谈。
		return (&PoisoningGuardPolicy{}).Validate(ctx, m, meta)
	}
	return nil
}

// WithInjectionDetection 把注入检测策略显式追加到既有注册表。
//
// 为什么不改 DefaultWritePolicies()：DefaultWritePolicies() 的策略数与名称由
// writepolicy_test.go 的 TestDefaultWritePoliciesInstalled 逐位锚定（6 条），
// 插入第 7 条会让既有测试失败，而本轮红线禁止改测试。
// 因此生产接线由调用方显式调用本函数（与 §3.15 的插件注册模型一致）。
//
// 注册位置说明：Register 追加到注册表末尾，即位于 poisoning_guard 之后。
// 这不会破坏处置语义——见 InjectionDetectorPolicy 注释的 ApplyGuard 说明。
func WithInjectionDetection(r *WritePolicyRegistry, detectCfg DetectConfig, markCfg PoisonDetectConfig) *WritePolicyRegistry {
	if r == nil {
		return nil
	}
	r.Register(&InjectionDetectorPolicy{
		DetectCfg:  detectCfg,
		MarkCfg:    markCfg,
		ApplyGuard: true,
	})
	return r
}

// NewInjectionAwareWritePolicyRegistry 返回「内置 6 条 + 注入检测」的注册表，
// 供生产接线一步完成；检测配置默认为保守值（间接面自动判定开启、重复写入判定开启）。
func NewInjectionAwareWritePolicyRegistry() *WritePolicyRegistry {
	r := NewDefaultWritePolicyRegistry()
	cfg := DefaultDetectConfig()
	cfg.IndirectMode = DetectIndirectAuto
	cfg.EnableRepeat = true
	return WithInjectionDetection(r, cfg, DefaultPoisonDetectConfig())
}
