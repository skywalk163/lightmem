// R21-A MCP 工具定义与实现（§3.2 冻结表 + §五 A 线）
//
// 工具命名沿用 mcp__lightmem__* 语义，13 个：
//
//	7 既有：lightmem_save / recall / search / update / feedback / forget / health
//	6 新增：lightmem_goal_create / goal_progress / goal_state / plan_list /
//	        step_add / summary_add
//
// 实现口径：一律通过 loopback HTTP 调 REST（§3.2 端点），不直接 import C 线新函数，
// 保证 A/C 编译期互不依赖，可真正并行。
package kernel

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ------------------------------------------------------------------
// 既有 7 工具
// ------------------------------------------------------------------

func toolSaveMeta() MCPTool {
	return MCPTool{
		Name: "lightmem_save",
		Description: "把一条长期记忆写入 lightmem 记忆系统。" +
			"当用户要求你\"记住\"某事，或对话中出现值得长期保留的事实、偏好、约定、结论时调用。" +
			"写入是不可变的（只增不改）：同一事实更新请用 lightmem_update。" +
			"默认 visibility=tenant（共享大脑语义）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact":             map[string]any{"type": "string", "description": "要记住的事实内容（一句话，自包含）。"},
				"memory_id":        map[string]any{"type": "string", "description": "可选：指定记忆 ID。"},
				"tenant_id":        map[string]any{"type": "string", "description": "可选：租户 ID，默认由 token 身份决定。"},
				"user_id":          map[string]any{"type": "string", "description": "可选：用户 ID，默认由 token 身份决定。"},
				"agent_id":         map[string]any{"type": "string", "description": "可选：Agent ID。"},
				"project_id":       map[string]any{"type": "string", "description": "可选：项目 ID。"},
				"memory_kind":      map[string]any{"type": "string", "description": "记忆类型，默认 fact。可选 fact / goal / preference / event / procedure。"},
				"visibility":       map[string]any{"type": "string", "description": "可见范围，默认 tenant。可选 private / shared_user / tenant / cross_tenant。"},
				"permission_level": map[string]any{"type": "string", "description": "密级，默认 internal。可选 public / internal / 机密。"},
				"confidence":       map[string]any{"type": "number", "description": "可选：置信度 0~1。"},
				"importance":       map[string]any{"type": "number", "description": "可选：重要性 0~1。"},
				"source":           map[string]any{"type": "string", "description": "可选：来源标识。"},
				"idempotency_key":  map[string]any{"type": "string", "description": "可选：幂等键，重复写入同一键会被忽略。"},
				"goal_detail":      map[string]any{"type": "object", "description": "goal 类记忆必填：{status,constraints[],kpi{metric,target,cadence},deadline,progress}。"},
			},
			"required":             []string{"fact"},
			"additionalProperties": false,
		},
	}
}

func handlerSave(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		fact := strArg(args, "fact", "")
		if strings.TrimSpace(fact) == "" {
			return "", fmt.Errorf("fact 不能为空")
		}
		body := map[string]any{
			"fact":             fact,
			"visibility":       strArg(args, "visibility", "tenant"),
			"permission_level": strArg(args, "permission_level", "internal"),
			"memory_kind":      strArg(args, "memory_kind", "fact"),
			"source":           strArg(args, "source", "lightmem-mcp"),
			"confidence":       floatArg(args, "confidence", 0.0),
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if v := strArg(args, "agent_id", ""); v != "" {
			body["agent_id"] = v
		}
		if v := strArg(args, "project_id", ""); v != "" {
			body["project_id"] = v
		}
		if v := strArg(args, "memory_id", ""); v != "" {
			body["memory_id"] = v
		}
		if v := strArg(args, "idempotency_key", ""); v != "" {
			body["idempotency_key"] = v
		}
		if v := strArg(args, "source", ""); v != "" {
			body["source"] = v
		}
		// permission.owner 必填：C 线 PermissionPolicy 会直接拒绝缺该字段的写入（对齐 Python 桥）
		owner := strArg(args, "owner", strArg(args, "user_id", strArg(args, "agent_id", "")))
		if owner != "" {
			body["permission"] = map[string]any{"owner": owner}
		}
		// 解 G3：goal_detail 透传
		if gd, ok := args["goal_detail"]; ok && gd != nil {
			body["goal_detail"] = gd
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/save", nil, body)
		if err != nil {
			return "", err
		}
		status := fmt.Sprintf("%v", result["status"])
		if status == "idempotent_noop" {
			return "幂等命中，未重复写入。", nil
		}
		mem := mapOf(result["memory"])
		return fmt.Sprintf("已保存记忆。memory_id=%s layer=%s\nfact: %s",
			fieldOf(mem, "memory_id"), fieldOf(mem, "layer"), fact), nil
	}
}

func toolRecallMeta() MCPTool {
	return MCPTool{
		Name: "lightmem_recall",
		Description: "从 lightmem 跨层召回并做多信号融合，是记忆检索的首选工具。" +
			"当用户的问题可能依赖此前保存的记忆、历史偏好或既有结论时调用。" +
			"返回按融合得分排序的记忆条目及其 memory_id。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":             map[string]any{"type": "string", "description": "检索查询文本（自然语言）。"},
				"tenant_id":        map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":          map[string]any{"type": "string", "description": "可选：用户 ID。"},
				"agent_id":         map[string]any{"type": "string", "description": "可选：Agent ID。"},
				"project_id":       map[string]any{"type": "string", "description": "可选：项目 ID。"},
				"top_k":            map[string]any{"type": "integer", "description": "可选：返回条数。"},
				"include_archived": map[string]any{"type": "boolean", "description": "可选：是否包含 L3 归档层。"},
				"as_of":            map[string]any{"type": "string", "description": "可选：时间回溯点（YYYY-MM-DD）。"},
			},
			"required":             []string{"text"},
			"additionalProperties": false,
		},
	}
}

func handlerRecall(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		text := strArg(args, "text", "")
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("text 不能为空")
		}
		body := map[string]any{"text": text}
		for _, k := range []string{"tenant_id", "user_id", "agent_id", "project_id", "as_of"} {
			if v := strArg(args, k, ""); v != "" {
				body[k] = v
			}
		}
		if v := intArg(args, "top_k", 0); v > 0 {
			body["top_k"] = v
		}
		if boolArg(args, "include_archived", false) {
			body["include_archived"] = true
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/recall", nil, body)
		if err != nil {
			return "", err
		}
		return formatRecallResult(result, text), nil
	}
}

func toolSearchMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_search",
		Description: "lightmem 的轻量文本检索（关键词/相似度直查，不做多信号融合）。需要简单命中时使用。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":      map[string]any{"type": "string", "description": "查询文本。"},
				"tenant_id": map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：可见性过滤用的 viewer。"},
				"top_k":     map[string]any{"type": "integer", "description": "可选：返回条数，默认 10。"},
				"kind":      map[string]any{"type": "string", "description": "可选：按记忆类型过滤（fact/goal/event/procedure/preference）。"},
			},
			"required":             []string{"text"},
			"additionalProperties": false,
		},
	}
}

func handlerSearch(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		text := strArg(args, "text", "")
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("text 不能为空")
		}
		q := url.Values{}
		q.Set("q", text)
		q.Set("text", text)
		if v := strArg(args, "tenant_id", ""); v != "" {
			q.Set("tenant_id", v)
		}
		if v := strArg(args, "user_id", ""); v != "" {
			q.Set("viewer", v)
		}
		if v := strArg(args, "kind", ""); v != "" {
			q.Set("kind", v)
		}
		q.Set("top_k", strconvItoa(intArg(args, "top_k", 10)))
		result, err := rc.do(ctx, "GET", "/search", q, nil)
		if err != nil {
			return "", err
		}
		// REST 返回可能为对象 {items:[...]} 或裸数组
		items := extractList(result)
		if len(items) == 0 {
			return "查询 '" + text + "' 没有命中。", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "查询 %q 命中 %d 条：\n", text, len(items))
		for i, item := range items {
			fmt.Fprintf(&b, "%d. [score=%s] %s (memory_id=%s)\n",
				i+1, fieldOf(item, "score"), fieldOf(item, "fact"), fieldOf(item, "memory_id"))
		}
		return b.String(), nil
	}
}

func toolUpdateMeta() MCPTool {
	return MCPTool{
		Name: "lightmem_update",
		Description: "更正一条已有记忆。lightmem 只增不改：本操作会写入新版本并把旧版本标记为被取代，" +
			"不会物理覆盖。需要知道目标记忆的 memory_id（可由 lightmem_recall 获得）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_id":  map[string]any{"type": "string", "description": "要更正的记忆 ID。"},
				"fact":       map[string]any{"type": "string", "description": "新的正确内容。"},
				"tenant_id":  map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":    map[string]any{"type": "string", "description": "可选：用户 ID。"},
				"confidence": map[string]any{"type": "number", "description": "可选：新置信度。"},
			},
			"required":             []string{"memory_id", "fact"},
			"additionalProperties": false,
		},
	}
}

func handlerUpdate(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		id := strArg(args, "memory_id", "")
		fact := strArg(args, "fact", "")
		if id == "" || fact == "" {
			return "", fmt.Errorf("memory_id 和 fact 不能为空")
		}
		body := map[string]any{"memory_id": id, "fact": fact, "viewer": strArg(args, "user_id", "")}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if v := floatArg(args, "confidence", -1); v >= 0 {
			body["confidence"] = v
		}
		result, err := rc.do(ctx, "POST", "/tools/memory/update", nil, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已更新记忆（追加版本，不覆盖原值）。新 memory_id=%s 被取代=%s",
			fieldOf(mapOf(result["memory"]), "memory_id"), fmt.Sprintf("%v", result["superseded"])), nil
	}
}

func toolFeedbackMeta() MCPTool {
	return MCPTool{
		Name: "lightmem_feedback",
		Description: "对一条召回结果给出反馈（adopted 采用 / rejected 拒绝 / edited 纠正），" +
			"用于 lightmem 的反馈闭环与排序学习。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_id": map[string]any{"type": "string", "description": "被反馈的记忆 ID。"},
				"status":    map[string]any{"type": "string", "description": "adopted | rejected | edited，默认 adopted。"},
				"operator":  map[string]any{"type": "string", "description": "可选：反馈者标识。"},
				"tenant_id": map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：用户 ID（决定可见性 viewer）。"},
			},
			"required":             []string{"memory_id"},
			"additionalProperties": false,
		},
	}
}

func handlerFeedback(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		id := strArg(args, "memory_id", "")
		if id == "" {
			return "", fmt.Errorf("memory_id 不能为空")
		}
		body := map[string]any{
			"memory_id": id,
			"status":    strArg(args, "status", "adopted"),
			"viewer":    strArg(args, "user_id", ""),
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if v := strArg(args, "operator", ""); v != "" {
			body["operator"] = v
		}
		if _, err := rc.do(ctx, "POST", "/tools/memory/feedback", nil, body); err != nil {
			return "", err
		}
		return "已记录反馈：status=" + fmt.Sprintf("%v", body["status"]) + " memory_id=" + id, nil
	}
}

func toolForgetMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_forget",
		Description: "软删除一条记忆（置为历史版本并标记取代者），保留审计痕迹，可被合规流程追溯。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_id": map[string]any{"type": "string", "description": "要删除的记忆 ID。"},
				"actor":     map[string]any{"type": "string", "description": "可选：操作者标识。"},
				"tenant_id": map[string]any{"type": "string", "description": "可选：租户 ID。"},
				"user_id":   map[string]any{"type": "string", "description": "可选：用户 ID（决定可见性 viewer）。"},
			},
			"required":             []string{"memory_id"},
			"additionalProperties": false,
		},
	}
}

func handlerForget(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, args map[string]any) (string, error) {
		id := strArg(args, "memory_id", "")
		if id == "" {
			return "", fmt.Errorf("memory_id 不能为空")
		}
		body := map[string]any{
			"memory_id": id,
			"viewer":    strArg(args, "user_id", ""),
			"actor":     strArg(args, "actor", strArg(args, "user_id", "")),
		}
		if v := strArg(args, "tenant_id", ""); v != "" {
			body["tenant_id"] = v
		}
		if v := strArg(args, "user_id", ""); v != "" {
			body["user_id"] = v
		}
		if _, err := rc.do(ctx, "POST", "/tools/memory/forget", nil, body); err != nil {
			return "", err
		}
		return "已软删除（置 is_historical + superseded_by）：memory_id=" + id, nil
	}
}

func toolHealthMeta() MCPTool {
	return MCPTool{
		Name:        "lightmem_health",
		Description: "检查 lightmem 服务健康状态与所用后端，排障时使用。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	}
}

func handlerHealth(rc *mcpRESTClient) mcpToolHandler {
	return func(ctx context.Context, _ map[string]any) (string, error) {
		req, err := httpNewRequest(ctx, "GET", rc.baseURL+"/healthz")
		if err != nil {
			return "", err
		}
		resp, err := rc.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("healthz 请求失败: %w", err)
		}
		defer resp.Body.Close()
		raw := make([]byte, 0, 512)
		buf := make([]byte, 256)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				raw = append(raw, buf[:n]...)
			}
			if rerr != nil {
				break
			}
		}
		return "lightmem 状态：" + strings.TrimSpace(string(raw)), nil
	}
}
