// R21-A MCP 工作流工具定义与实现（§3.2 冻结表 6 个：goal_create / goal_progress /
// goal_state / plan_list / step_add / summary_add）
package kernel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------
// 6 工作流工具
// ------------------------------------------------------------------

func toolGoalCreateMeta() MCPTool {
	return MCPTool{
		Name: "lightmem_goal_create",
		Description: "建计划：把一条工作计划沉淀为 goal 类记忆。" +
			"goal_detail 必填（含 status/constraints/kpi/deadline/progress），缺失会 422。" +
			"默认 visibility=tenant（共享大脑语义）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact":            map[string]any{"type": "string", "description": "目标描述（一句话，自包含）。"},
				"goal_detail":     map[string]any{"type": "object", "description": "必填：{status,constraints[],kpi{metric,target,cadence},deadline,progress}。"},
				"visibility":      map[string]any{"type": "string", "description": "可选：默认 tenant。"},
				"tenant_id":       map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":         map[string]any{"type": "string", "description": "可选：用户 ID。"},
				"project_id":      map[string]any{"type": "string", "description": "可选：项目 ID。"},
				"idempotency_key": map[string]any{"type": "string", "description": "可选：幂等键。"},
			},
			"required":             []string{"fact", "goal_detail"},
			"additionalProperties": false,
		},
	}
}

func handlerGoalCreate(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		fact := strArg(args, "fact", "")
		if strings.TrimSpace(fact) == "" {
			return "", fmt.Errorf("fact 不能为空")
		}
		gd, ok := args["goal_detail"]
		if !ok || gd == nil {
			return "", fmt.Errorf("goal_detail 必填（goal 类记忆的约束/KPI/状态由服务端校验）")
		}
		body := map[string]any{
			"fact":        fact,
			"goal_detail": gd,
			"visibility":  strArg(args, "visibility", "tenant"),
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if v := strArg(args, "project_id", ""); v != "" {
			body["project_id"] = v
		}
		if v := strArg(args, "idempotency_key", ""); v != "" {
			body["idempotency_key"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/goal", nil, body)
		if err != nil {
			return "", err
		}
		mem := mapOf(result["memory"])
		return fmt.Sprintf("计划已创建。memory_id=%s status=%s\n目标: %s",
			fieldOf(mem, "memory_id"),
			fieldOfNested(mem, "goal_detail", "status"),
			fact), nil
	}
}

func toolGoalProgressMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_goal_progress",
		Description: "推进度：更新计划进度（0~1），服务端按状态机自动迁移状态（progress>=1 置 completed）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_id": map[string]any{"type": "string", "description": "计划记忆 ID。"},
				"progress":  map[string]any{"type": "number", "description": "进度 0~1。"},
				"tenant_id": map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：用户 ID。"},
			},
			"required":             []string{"memory_id", "progress"},
			"additionalProperties": false,
		},
	}
}

func handlerGoalProgress(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		id := strArg(args, "memory_id", "")
		if id == "" {
			return "", fmt.Errorf("memory_id 不能为空")
		}
		progress := floatArg(args, "progress", -1)
		if progress < 0 || progress > 1 {
			return "", fmt.Errorf("progress 必须在 0~1 之间")
		}
		body := map[string]any{"memory_id": id, "progress": progress}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/goal/progress", nil, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("进度已更新。memory_id=%s progress=%s state=%s",
			fmt.Sprintf("%v", result["memory_id"]),
			fmt.Sprintf("%v", result["progress"]),
			fmt.Sprintf("%v", result["state"])), nil
	}
}

func toolGoalStateMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_goal_state",
		Description: "改状态：active / paused / completed / expired，服务端走状态机校验非法转移。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_id": map[string]any{"type": "string", "description": "计划记忆 ID。"},
				"state":     map[string]any{"type": "string", "description": "active | paused | completed | expired。"},
				"tenant_id": map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：用户 ID。"},
			},
			"required":             []string{"memory_id", "state"},
			"additionalProperties": false,
		},
	}
}

func handlerGoalState(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		id := strArg(args, "memory_id", "")
		state := strArg(args, "state", "")
		if id == "" || state == "" {
			return "", fmt.Errorf("memory_id 和 state 不能为空")
		}
		switch state {
		case "active", "paused", "completed", "expired":
		default:
			return "", fmt.Errorf("state 必须是 active|paused|completed|expired")
		}
		body := map[string]any{"memory_id": id, "state": state}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/goal/state", nil, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("状态已更新。memory_id=%s state=%s",
			fmt.Sprintf("%v", result["memory_id"]), fmt.Sprintf("%v", result["state"])), nil
	}
}

func toolPlanListMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_plan_list",
		Description: "列计划：列出某租户的计划（goal 类记忆），支持按 status 过滤与 top_k。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tenant_id": map[string]any{"type": "string", "description": "租户 ID（必填）。"},
				"status":    map[string]any{"type": "string", "description": "可选：active | paused | completed | expired。"},
				"top_k":     map[string]any{"type": "integer", "description": "可选：返回条数，默认 100。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：可见性过滤用 viewer。"},
			},
			"required":             []string{"tenant_id"},
			"additionalProperties": false,
		},
	}
}

func handlerPlanList(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		tenant := strArg(args, "tenant_id", "")
		if tenant == "" {
			return "", fmt.Errorf("tenant_id 不能为空")
		}
		q := urlValues()
		q.Set("tenant_id", tenant)
		if v := strArg(args, "status", ""); v != "" {
			q.Set("status", v)
		}
		if v := strArg(args, "user_id", ""); v != "" {
			q.Set("viewer", v)
		}
		q.Set("top_k", strconv.Itoa(intArg(args, "top_k", 100)))
		result, err := rc.do(ctx, "GET", "/tools/memory/plan", q, nil)
		if err != nil {
			return "", err
		}
		count := 0
		if v, ok := result["count"].(float64); ok {
			count = int(v)
		}
		plans := extractList(result["plans"])
		var b strings.Builder
		fmt.Fprintf(&b, "租户 %s 的计划共 %d 条%s：\n", tenant, count, statusSuffix(strArg(args, "status", "")))
		if len(plans) == 0 {
			b.WriteString("（无）\n")
		}
		for i, item := range plans {
			fmt.Fprintf(&b, "%d. [progress=%s status=%s] %s (memory_id=%s)\n",
				i+1,
				fieldOfNested(item, "goal_detail", "progress"),
				fieldOfNested(item, "goal_detail", "status"),
				fieldOf(item, "fact"),
				fieldOf(item, "memory_id"))
		}
		return b.String(), nil
	}
}

func toolStepAddMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_step_add",
		Description: "记步骤：把一条工作步骤沉淀为 procedure 类记忆，可关联 project_id 与 goal_id。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact":       map[string]any{"type": "string", "description": "步骤描述。"},
				"project_id": map[string]any{"type": "string", "description": "项目 ID（必填）。"},
				"goal_id":    map[string]any{"type": "string", "description": "可选：关联的计划 ID。"},
				"seq":        map[string]any{"type": "integer", "description": "可选：步骤序号。"},
				"tenant_id":  map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":    map[string]any{"type": "string", "description": "可选：用户 ID。"},
			},
			"required":             []string{"fact", "project_id"},
			"additionalProperties": false,
		},
	}
}

func handlerStepAdd(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		fact := strArg(args, "fact", "")
		project := strArg(args, "project_id", "")
		if fact == "" || project == "" {
			return "", fmt.Errorf("fact 和 project_id 不能为空")
		}
		body := map[string]any{"fact": fact, "project_id": project}
		if v := strArg(args, "goal_id", ""); v != "" {
			body["goal_id"] = v
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if v := intArg(args, "seq", 0); v > 0 {
			body["seq"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/step", nil, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("步骤已记录。memory_id=%s seq=%s goal_id=%s",
			fmt.Sprintf("%v", result["memory_id"]),
			fmt.Sprintf("%v", result["seq"]),
			fmt.Sprintf("%v", result["goal_id"])), nil
	}
}

func toolSummaryAddMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_summary_add",
		Description: "记总结：把一条工作总结沉淀为 event 类记忆，可关联 project_id 与 goal_id。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact":       map[string]any{"type": "string", "description": "总结内容。"},
				"project_id": map[string]any{"type": "string", "description": "项目 ID（必填）。"},
				"goal_id":    map[string]any{"type": "string", "description": "可选：关联的计划 ID。"},
				"tenant_id":  map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":    map[string]any{"type": "string", "description": "可选：用户 ID。"},
			},
			"required":             []string{"fact", "project_id"},
			"additionalProperties": false,
		},
	}
}

func handlerSummaryAdd(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		fact := strArg(args, "fact", "")
		project := strArg(args, "project_id", "")
		if fact == "" || project == "" {
			return "", fmt.Errorf("fact 和 project_id 不能为空")
		}
		body := map[string]any{"fact": fact, "project_id": project}
		if v := strArg(args, "goal_id", ""); v != "" {
			body["goal_id"] = v
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/summary", nil, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("总结已记录。memory_id=%s goal_id=%s",
			fmt.Sprintf("%v", result["memory_id"]), fmt.Sprintf("%v", result["goal_id"])), nil
	}
}

// ------------------------------------------------------------------
// 结果格式化与字段提取辅助
// ------------------------------------------------------------------

func formatRecallResult(result map[string]any, query string) string {
	fused := extractList(result["fused"])
	if len(fused) == 0 {
		// 回退到分层召回
		var lines []string
		for _, layer := range extractList(result["recalled"]) {
			for _, c := range extractList(layer["candidates"]) {
				mem := mapOf(c["memory"])
				if f := fieldOf(mem, "fact"); f != "" {
					lines = append(lines, "- ["+fieldOf(layer, "layer")+"] "+f)
				}
			}
		}
		if len(lines) > 0 {
			return fmt.Sprintf("查询 %q 命中（未融合，按层列出）：\n%s", query, strings.Join(lines, "\n"))
		}
		return fmt.Sprintf("查询 %q 没有命中任何记忆。", query)
	}
	mode := ""
	if v, ok := result["mode"]; ok {
		mode = fmt.Sprintf("%v", v)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "查询 %q 命中 %d 条（mode=%s）：\n", query, len(fused), mode)
	for i, item := range fused {
		mem := mapOf(item["memory"])
		fact := fieldOf(item, "fact")
		if fact == "" {
			fact = fieldOf(mem, "fact")
		}
		if fact == "" {
			fact = "(空)"
		}
		fmt.Fprintf(&b, "%d. [score=%s layer=%s] %s\n", i+1,
			fieldOf(item, "score"), fieldOf(item, "layer"), fact)
		if id := fieldOf(mem, "memory_id"); id != "" {
			fmt.Fprintf(&b, "   memory_id=%s\n", id)
		}
	}
	return b.String()
}

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func fieldOf(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func fieldOfNested(m map[string]any, sub, key string) string {
	inner := mapOf(m[sub])
	return fieldOf(inner, key)
}

func extractList(v any) []map[string]any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return t
	case map[string]any:
		if items, ok := t["items"].([]any); ok {
			out := make([]map[string]any, 0, len(items))
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		if items, ok := t["results"].([]any); ok {
			out := make([]map[string]any, 0, len(items))
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		return []map[string]any{t}
	}
	return nil
}

func strconvItoa(n int) string { return strconv.Itoa(n) }

func httpNewRequest(ctx context.Context, method, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, nil)
}

func urlValues() url.Values { return url.Values{} }

func statusSuffix(s string) string {
	if s == "" {
		return ""
	}
	return "（status=" + s + "）"
}
