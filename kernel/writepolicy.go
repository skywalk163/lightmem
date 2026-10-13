// WritePolicy 插件注册表（§3.15）
// 真源：lightmem.md §3.15「写策略插件」、§3.8 goal、§7.1 权限、§7.5 投毒防护、§4.1 幂等。
//
// 设计：核心写入路径只依赖 WritePolicyRegistry 接口，新增校验规则无需改动核心模块。
// 执行顺序由注册顺序（order）保证确定性；内置策略在 DefaultWritePolicies() 中给出。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PermissionLevelConfidential 机密级 permission_level（与 model.go/migration 注释口径一致）。
const PermissionLevelConfidential = "机密"

var allowedPermissionLevels = map[string]bool{
	"":                          true,
	"public":                    true,
	"internal":                  true,
	"confidential":              true, // 文档口径
	"secret":                    true,
	PermissionLevelConfidential: true, // 代码/迁移口径
}

// WriteMeta 写入上下文（供策略判定，不进 Schema）。
type WriteMeta struct {
	Actor    string    // 操作者（viewer）
	Now      time.Time // 写入时刻
	Existing *Memory   // 同 idempotency_key 的已存在版本（无则 nil）
}

// WritePolicy 单条写策略；Validate 返回 error 即拒绝写入。
type WritePolicy interface {
	Name() string
	Validate(ctx context.Context, m *Memory, meta WriteMeta) error
}

// WritePolicyRegistry 策略注册表（保证确定性执行顺序）。
type WritePolicyRegistry struct {
	policies map[string]WritePolicy
	order    []string
}

// NewWritePolicyRegistry 返回空注册表。
func NewWritePolicyRegistry() *WritePolicyRegistry {
	return &WritePolicyRegistry{policies: map[string]WritePolicy{}}
}

// Register 注册（或覆盖）策略；重复注册保持首次位置。
func (r *WritePolicyRegistry) Register(p WritePolicy) {
	if p == nil {
		return
	}
	name := p.Name()
	if _, ok := r.policies[name]; !ok {
		r.order = append(r.order, name)
	}
	r.policies[name] = p
}

// Remove 移除策略。
func (r *WritePolicyRegistry) Remove(name string) {
	if _, ok := r.policies[name]; !ok {
		return
	}
	delete(r.policies, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Names 返回按注册顺序的策略名。
func (r *WritePolicyRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// ValidateAll 按注册顺序依次执行策略；首个错误即返回。
func (r *WritePolicyRegistry) ValidateAll(ctx context.Context, m *Memory, meta WriteMeta) error {
	if r == nil {
		return nil
	}
	if meta.Now.IsZero() {
		meta.Now = time.Now().UTC()
	}
	for _, name := range r.order {
		p := r.policies[name]
		if p == nil {
			continue
		}
		if err := p.Validate(ctx, m, meta); err != nil {
			return &PolicyError{Policy: name, Err: err}
		}
	}
	return nil
}

// PolicyError 包裹某策略的拒绝原因。
type PolicyError struct {
	Policy string
	Err    error
}

func (e *PolicyError) Error() string { return "writepolicy " + e.Policy + ": " + e.Err.Error() }
func (e *PolicyError) Unwrap() error { return e.Err }

// ErrIdempotentNoop 表示幂等键已存在，写入应被忽略（只增不覆盖，§4.1）。
// 调用方应把该错误视为「成功空操作」而非失败。
var ErrIdempotentNoop = errors.New("idempotency_key already exists: no-op (only-add, §4.1)")

// IdempotencyLookup 可选后端接口：按 idempotency_key 查已存在记忆。
type IdempotencyLookup interface {
	FindByIdempotencyKey(ctx context.Context, tenant, user, key string) (*Memory, error)
}

// DefaultWritePolicies 返回任务书 §4.3 要求的 6 个内置策略。
func DefaultWritePolicies() []WritePolicy {
	return []WritePolicy{
		&GoalConstraintsPolicy{},
		&PermissionPolicy{},
		&RedactionPolicy{},
		&PoisoningGuardPolicy{},
		&IdempotencyPolicy{},
		&FeedbackPolicy{},
	}
}

// NewDefaultWritePolicyRegistry 返回已装载内置策略的注册表。
func NewDefaultWritePolicyRegistry() *WritePolicyRegistry {
	r := NewWritePolicyRegistry()
	for _, p := range DefaultWritePolicies() {
		r.Register(p)
	}
	return r
}

// ------------------------------------------------------------------
// 内置策略 1：GoalConstraintsPolicy（§3.8）
// ------------------------------------------------------------------

// GoalConstraintsPolicy 校验 goal 类记忆的约束/KPI/状态。
type GoalConstraintsPolicy struct{}

func (p *GoalConstraintsPolicy) Name() string { return "goal_constraints" }

var validGoalStatus = map[string]bool{"active": true, "paused": true, "completed": true, "expired": true}

func (p *GoalConstraintsPolicy) Validate(_ context.Context, m *Memory, _ WriteMeta) error {
	if m == nil || m.MemoryKind != KindGoal {
		return nil
	}
	gd := m.GoalDetail
	if gd == nil {
		return errors.New("goal memory requires goal_detail")
	}
	if len(gd.Constraints) == 0 {
		return errors.New("goal_detail.constraints must be non-empty")
	}
	if gd.KPI == nil || gd.KPI.Target <= 0 {
		return errors.New("goal_detail.kpi.target must be > 0")
	}
	if !validGoalStatus[gd.Status] {
		return fmt.Errorf("goal_detail.status %q invalid (want active|paused|completed|expired)", gd.Status)
	}
	return nil
}

// ------------------------------------------------------------------
// 内置策略 2：PermissionPolicy（§7.1）
// ------------------------------------------------------------------

// PermissionPolicy 校验权限三元组与密级。
type PermissionPolicy struct{}

func (p *PermissionPolicy) Name() string { return "permission" }

func (p *PermissionPolicy) Validate(_ context.Context, m *Memory, _ WriteMeta) error {
	if m == nil {
		return errors.New("nil memory")
	}
	if strings.TrimSpace(m.Permission.Owner) == "" {
		return errors.New("permission.owner required")
	}
	if !allowedPermissionLevels[m.PermissionLevel] {
		return fmt.Errorf("permission_level %q invalid", m.PermissionLevel)
	}
	return nil
}

// ------------------------------------------------------------------
// 内置策略 3：RedactionPolicy（敏感信息脱敏/降密级）
// ------------------------------------------------------------------

// RedactionPolicy 命中敏感模式时打机密级密级（可扩展为截断脱敏）。
type RedactionPolicy struct {
	Mask bool // true 时额外对 snippet 做脱敏
}

func (p *RedactionPolicy) Name() string { return "redaction" }

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	rePhone = regexp.MustCompile(`(?:^|\D)(1[3-9]\d{9})(?:\D|$)`)
)

func (p *RedactionPolicy) Validate(_ context.Context, m *Memory, _ WriteMeta) error {
	if m == nil || m.MemoryKind != KindFact {
		return nil
	}
	if reEmail.MatchString(m.Fact) || rePhone.MatchString(m.Fact) {
		m.PermissionLevel = PermissionLevelConfidential
		if p.Mask && m.SourceEvidence != nil && m.SourceEvidence.Snippet != "" {
			m.SourceEvidence.Snippet = reEmail.ReplaceAllString(m.SourceEvidence.Snippet, "[email-redacted]")
			m.SourceEvidence.Snippet = rePhone.ReplaceAllString(m.SourceEvidence.Snippet, "[phone-redacted]")
		}
	}
	return nil
}

// ------------------------------------------------------------------
// 内置策略 4：PoisoningGuardPolicy（§7.5）
// ------------------------------------------------------------------

// PoisoningGuardPolicy 对疑似提示注入的写入降权并截断证据。
type PoisoningGuardPolicy struct{}

func (p *PoisoningGuardPolicy) Name() string { return "poisoning_guard" }

const maxEvidenceChars = 200

func (p *PoisoningGuardPolicy) Validate(_ context.Context, m *Memory, _ WriteMeta) error {
	if m == nil || m.Provenance != ProvInjectionSuspect {
		return nil
	}
	if m.Confidence > 0.3 {
		m.Confidence = 0.3
	}
	if m.SourceEvidence != nil && len([]rune(m.SourceEvidence.Snippet)) > maxEvidenceChars {
		m.SourceEvidence.Snippet = string([]rune(m.SourceEvidence.Snippet)[:maxEvidenceChars])
	}
	return nil
}

// ------------------------------------------------------------------
// 内置策略 5：IdempotencyPolicy（§4.1 只增不覆盖）
// ------------------------------------------------------------------

// IdempotencyPolicy 幂等：同 idempotency_key 已存在则忽略本次写入。
type IdempotencyPolicy struct{}

func (p *IdempotencyPolicy) Name() string { return "idempotency" }

func (p *IdempotencyPolicy) Validate(_ context.Context, m *Memory, meta WriteMeta) error {
	if m == nil {
		return nil
	}
	if strings.TrimSpace(m.IdempotencyKey) == "" {
		return nil // 未提供幂等键：交由后端回退为 memory_id（保持既有行为）
	}
	if meta.Existing != nil {
		return ErrIdempotentNoop
	}
	return nil
}

// ------------------------------------------------------------------
// 内置策略 6：FeedbackPolicy（§3.11 反馈闭环）
// ------------------------------------------------------------------

// FeedbackPolicy 反馈闭环：拦截 feedback_records，验证并执行回写规则。
// 规则：
//   - rejected：confidence 降权（已降 0.1），标记 correction-candidate
//   - adopted：confidence 升权（+0.05），提升后续排序
//   - edited：记录版本历史，标记 superseded_by
type FeedbackPolicy struct{}

func (p *FeedbackPolicy) Name() string { return "feedback_loop" }

// minConfidence 置信度下限，防止降权后为负。
const minConfidence = 0.0

func (p *FeedbackPolicy) Validate(_ context.Context, m *Memory, _ WriteMeta) error {
	if m == nil || len(m.FeedbackRecords) == 0 {
		return nil
	}
	// 遍历所有反馈记录，执行回写规则
	for _, fb := range m.FeedbackRecords {
		switch fb.Status {
		case FbRejected:
			// rejected：降权，但不低于下限
			m.Confidence = clamp01(m.Confidence - 0.1)
			if m.Confidence < minConfidence {
				m.Confidence = minConfidence
			}
		case FbAdopted:
			// adopted：升权，但不超过 1.0
			m.Confidence = clamp01(m.Confidence + 0.05)
		case FbEdited:
			// edited：记录版本历史，标记 superseded_by（由 update 端点处理）
			// 此处仅确保 confidence 不因编辑而降低
			if m.Confidence < 0.5 {
				m.Confidence = 0.5
			}
		}
	}
	return nil
}
