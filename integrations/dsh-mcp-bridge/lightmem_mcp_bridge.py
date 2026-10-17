#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""lightmem MCP bridge —— 把 lightmem 的 REST API 暴露为 MCP tools，供 DeepSeek Harness 调用。

背景
----
DSH 与外部记忆系统的唯一对接通道是 MCP（stdio 或 streamable-http），
而 lightmem 只提供 REST。本桥在两者之间做协议转换：

    DSH(dsh-mcp-client, stdio) ──JSON-RPC over stdio──> 本桥 ──HTTP──> lightmem REST

设计要点
--------
1. **零第三方依赖**：只用 Python 标准库（json / sys / urllib），
   因为 DSH 的 stdio transport 会清洗掉名字形如 KEY|PASSWORD|SECRET|TOKEN 的环境变量，
   且不建议在子进程里再装依赖。
2. **协议版本**：实测 DSH 内置的 @modelcontextprotocol/client@2.2.0 会先发
   `initialize` 并声明 `protocolVersion: "2025-11-25"`。本桥回显客户端请求的版本，
   因此同时兼容 2025-11-25 与 2026-07-28 两个 era。
3. **健壮性**：任何单条请求异常都不允许杀死主循环（否则 DSH 只会看到工具消失）。
   所有异常转成 JSON-RPC error 或 MCP isError 结果返回。
4. **日志**：写 stderr（DSH 会以 [server] 前缀转发）+ 可选文件，便于排障。

配置（环境变量，均可在 DSH 的 config.env 里覆盖）
--------------------------------------------------
    LIGHTMEM_BASE_URL   默认 http://192.168.0.88:18443
    LIGHTMEM_TENANT_ID  默认 dsh
    LIGHTMEM_USER_ID    默认 dsh-agent
    LIGHTMEM_AGENT_ID   默认 deepseek-harness
    LIGHTMEM_TIMEOUT    默认 15（秒）
    LIGHTMEM_LOG        可选，日志文件路径（追加写）

注意：环境变量名刻意不含 KEY/PASSWORD/SECRET/TOKEN 字样，
避免被 DSH 的 stdio 环境清洗规则（scrubbedParentEnv）丢弃。
"""

from __future__ import annotations

import datetime
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

# ---------------------------------------------------------------------------
# 关键：强制 stdio 走 UTF-8
# ---------------------------------------------------------------------------
# MCP 的 stdio 传输规定 JSON-RPC 用 UTF-8 编码。但在中文 Windows 上，
# Python 的 sys.stdin / sys.stdout 默认取系统 ANSI 代码页（cp936/GBK），
# 且错误处理器为 surrogateescape。这会造成三步损坏：
#   1) DSH 发来的 UTF-8 中文被按 GBK 解码 → 乱码；
#   2) 解不出的字节变成**孤立代理项**（lone surrogate，如 \udc8f）；
#   3) 随后 json.dumps(...).encode("utf-8") 抛
#      UnicodeEncodeError: ... surrogates not allowed
# 现象：写入含中文的记忆必定失败，纯英文正常。
# 因此必须在任何 I/O 之前重设三个流的编码。
for _stream in (sys.stdin, sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

PROTOCOL_FALLBACK = "2025-11-25"
SERVER_NAME = "lightmem"
SERVER_VERSION = "0.1.0"

DEFAULT_BASE_URL = "http://192.168.0.88:18443"
DEFAULT_TENANT = "dsh"
DEFAULT_USER = "dsh-agent"
DEFAULT_AGENT = "deepseek-harness"


def env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value or default


BASE_URL = env("LIGHTMEM_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
TENANT_ID = env("LIGHTMEM_TENANT_ID", DEFAULT_TENANT)
USER_ID = env("LIGHTMEM_USER_ID", DEFAULT_USER)
AGENT_ID = env("LIGHTMEM_AGENT_ID", DEFAULT_AGENT)
TIMEOUT = float(env("LIGHTMEM_TIMEOUT", "15"))
LOG_PATH = os.environ.get("LIGHTMEM_LOG", "").strip()


def log(message: str) -> None:
    """记录一行日志：到 stderr（DSH 会转发）与可选日志文件。"""
    line = "[%s] %s" % (datetime.datetime.now().isoformat(timespec="seconds"), message)
    sys.stderr.write(line + "\n")
    sys.stderr.flush()
    if LOG_PATH:
        try:
            with open(LOG_PATH, "a", encoding="utf-8") as handle:
                handle.write(line + "\n")
        except OSError:
            pass


# ---------------------------------------------------------------------------
# HTTP 层：调用 lightmem REST
# ---------------------------------------------------------------------------

class LightmemError(Exception):
    """lightmem REST 调用失败（网络或 4xx/5xx）。"""

    def __init__(self, message: str, status: int | None = None, body=None) -> None:
        super().__init__(message)
        self.status = status
        self.body = body


def http_json(method: str, path: str, payload=None, query=None):
    """发一次 HTTP 请求并解析 JSON 响应，失败抛 LightmemError。"""
    url = BASE_URL + path
    if query:
        pairs = [(k, str(v)) for k, v in query.items() if v is not None and v != ""]
        if pairs:
            url += "?" + urllib.parse.urlencode(pairs)

    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        # errors="replace" 是第二道防线：即使上游仍递来孤立代理项，
        # 也只降级为 "?"，绝不让一次工具调用因编码而整体失败。
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8", "replace")
        headers["Content-Type"] = "application/json; charset=utf-8"

    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT) as response:
            raw = response.read().decode("utf-8", "replace")
            return json.loads(raw) if raw.strip() else {}
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"error": raw}
        detail = body.get("error") if isinstance(body, dict) else raw
        raise LightmemError(f"{method} {path} -> HTTP {exc.code}: {detail}", exc.code, body) from exc
    except urllib.error.URLError as exc:
        raise LightmemError(f"{method} {path} -> 无法连接 {BASE_URL}: {exc.reason}") from exc
    except TimeoutError as exc:
        raise LightmemError(f"{method} {path} -> 超时（{TIMEOUT}s）") from exc
    except ValueError as exc:
        raise LightmemError(f"{method} {path} -> 响应不是合法 JSON: {exc}") from exc


def scope(args: dict, *, require_fact: bool = False, require_text: bool = False,
          require_memory_id: bool = False) -> dict:
    """从工具参数里取作用域与校验必填项。"""
    fact = (args.get("fact") or "").strip()
    text = (args.get("text") or "").strip()
    memory_id = (args.get("memory_id") or "").strip()

    if require_fact and not fact:
        raise LightmemError("fact 不能为空")
    if require_text and not text:
        raise LightmemError("text 不能为空")
    if require_memory_id and not memory_id:
        raise LightmemError("memory_id 不能为空")

    tenant = (args.get("tenant_id") or "").strip() or TENANT_ID
    user = (args.get("user_id") or "").strip() or USER_ID
    return {"tenant_id": tenant, "user_id": user, "fact": fact, "text": text, "memory_id": memory_id}


# ---------------------------------------------------------------------------
# 工具实现
# ---------------------------------------------------------------------------

def tool_save(args: dict) -> str:
    s = scope(args, require_fact=True)
    # permission.owner 必填：lightmem 的 PermissionPolicy 会直接拒绝缺该字段的写入。
    body = {
        "tenant_id": s["tenant_id"],
        "user_id": s["user_id"],
        "fact": s["fact"],
        "visibility": (args.get("visibility") or "private").strip(),
        "permission": {"owner": (args.get("owner") or s["user_id"]).strip()},
        "permission_level": (args.get("permission_level") or "internal").strip(),
        "memory_kind": (args.get("memory_kind") or "fact").strip(),
        "source": (args.get("source") or "deepseek-harness").strip(),
        "confidence": float(args.get("confidence") or 0.0),
    }
    for key, target in (
        ("memory_id", "memory_id"),
        ("idempotency_key", "idempotency_key"),
        ("agent_id", "agent_id"),
        ("project_id", "project_id"),
    ):
        value = (args.get(key) or "").strip() or (AGENT_ID if key == "agent_id" else "")
        if value:
            body[target] = value
    for key in ("importance",):
        if args.get(key) is not None:
            body[key] = float(args[key])
    if args.get("valid_from"):
        body["valid_from"] = str(args["valid_from"])
    if args.get("expire_at"):
        body["expire_at"] = str(args["expire_at"])

    result = http_json("POST", "/tools/memory/save", body)
    memory = result.get("memory") or {}
    status = result.get("status", "unknown")
    if status == "idempotent_noop":
        return "幂等命中，未重复写入。memory_id=%s" % memory.get("memory_id", "?")
    return "已保存记忆。memory_id=%s layer=%s\nfact: %s" % (
        memory.get("memory_id", "?"), memory.get("layer", "?"), memory.get("fact", s["fact"]))


def tool_recall(args: dict) -> str:
    s = scope(args, require_text=True)
    body = {
        "text": s["text"],
        "tenant_id": s["tenant_id"],
        "user_id": s["user_id"],
        "agent_id": (args.get("agent_id") or AGENT_ID).strip(),
    }
    for key, value in (("project_id", args.get("project_id")), ("top_k", args.get("top_k")),
                       ("include_archived", args.get("include_archived")), ("as_of", args.get("as_of"))):
        if value not in (None, "", False):
            body[key] = value

    result = http_json("POST", "/tools/memory/recall", body)
    return format_recall(result, s["text"])


def format_recall(result: dict, query: str) -> str:
    """把 recall 的融合结果整理成对模型友好的文本。"""
    fused = result.get("fused") or []
    if not fused:
        # 回退到分层召回结果
        lines = []
        for layer in result.get("recalled") or []:
            for candidate in layer.get("candidates") or []:
                memory = candidate.get("memory") or {}
                if memory.get("fact"):
                    lines.append("- [%s] %s" % (layer.get("layer", "?"), memory["fact"]))
        if lines:
            return "查询 %r 命中（未融合，按层列出）：\n%s" % (query, "\n".join(lines))
        return "查询 %r 没有命中任何记忆。" % query

    lines = ["查询 %r 命中 %d 条（mode=%s）：" % (query, len(fused), result.get("mode", "?"))]
    for index, item in enumerate(fused, 1):
        memory = item.get("memory") or {}
        fact = item.get("fact") or memory.get("fact") or "(空)"
        lines.append("%d. [score=%.4f layer=%s] %s" % (
            index, float(item.get("score") or 0.0), item.get("layer") or memory.get("layer") or "?", fact))
        if memory.get("memory_id"):
            lines.append("   memory_id=%s" % memory["memory_id"])
    return "\n".join(lines)


def tool_search(args: dict) -> str:
    s = scope(args, require_text=True)
    # 参数名坑（R160 I-12）：lightmem 的 handleSearch 只读 `q`：
    #     kernel/router.go:154   Text: q.Get("q")
    # README 却写 `/search?text=`，照抄 README 会让 Text 为空 →
    # 后端跳过文本过滤 → 返回作用域内**全部**（分数恒定），看起来像"过滤失效"。
    # 因此这里同时发 q 与 text：q 保证当前服务端真正过滤，text 兼容未来若加别名。
    query = {
        "q": s["text"],
        "text": s["text"],
        "tenant_id": s["tenant_id"],
        "viewer": s["user_id"],
        "top_k": args.get("top_k") or 10,
    }
    if args.get("kind"):
        query["kind"] = args["kind"]
    result = http_json("GET", "/search", query=query)
    # 注意：lightmem 的 /search 在无命中时返回 JSON null。
    if not result:
        return "查询 %r 没有命中。" % s["text"]
    if isinstance(result, dict) and result.get("error"):
        raise LightmemError(str(result["error"]))
    lines = ["查询 %r 命中 %d 条：" % (s["text"], len(result))]
    for index, item in enumerate(result, 1):
        lines.append("%d. [score=%.4f] %s (memory_id=%s)" % (
            index, float(item.get("score") or 0.0), item.get("fact", ""), item.get("memory_id", "?")))
    return "\n".join(lines)


def tool_update(args: dict) -> str:
    s = scope(args, require_fact=True, require_memory_id=True)
    body = {
        "memory_id": s["memory_id"],
        "fact": s["fact"],
        "tenant_id": s["tenant_id"],
        "user_id": s["user_id"],
        "viewer": s["user_id"],
    }
    if args.get("confidence") is not None:
        body["confidence"] = float(args["confidence"])
    result = http_json("POST", "/tools/memory/update", body)
    memory = result.get("memory") or {}
    return "已更新记忆（追加版本，不覆盖原值）。新 memory_id=%s 被取代=%s" % (
        memory.get("memory_id", "?"), result.get("superseded", "?"))


def tool_feedback(args: dict) -> str:
    memory_id = (args.get("memory_id") or "").strip()
    if not memory_id:
        raise LightmemError("memory_id 不能为空")
    tenant = (args.get("tenant_id") or "").strip() or TENANT_ID
    user = (args.get("user_id") or "").strip() or USER_ID
    body = {
        "memory_id": memory_id,
        "status": (args.get("status") or "adopted").strip(),
        "operator": (args.get("operator") or user).strip(),
        "tenant_id": tenant,
        # viewer / user_id 必填：lightmem 的 feedback 走 backend.Get(memory_id, viewer, tenant)
        # 做可见性硬过滤。缺 viewer 会得到 404
        # 「memory <id> not visible to /<tenant>」（viewer 为空）。
        "user_id": user,
        "viewer": user,
    }
    result = http_json("POST", "/tools/memory/feedback", body)
    return "已记录反馈：status=%s memory_id=%s" % (body["status"], body["memory_id"])


def tool_forget(args: dict) -> str:
    memory_id = (args.get("memory_id") or "").strip()
    if not memory_id:
        raise LightmemError("memory_id 不能为空")
    tenant = (args.get("tenant_id") or "").strip() or TENANT_ID
    user = (args.get("user_id") or "").strip() or USER_ID
    body = {
        "memory_id": memory_id,
        "actor": (args.get("actor") or user).strip(),
        "tenant_id": tenant,
        # 同 feedback：forget 也需要 viewer 才能通过可见性硬过滤。
        "user_id": user,
        "viewer": user,
    }
    result = http_json("POST", "/tools/memory/forget", body)
    return "已软删除（置 is_historical + superseded_by）：memory_id=%s 结果=%s" % (
        memory_id, json.dumps(result, ensure_ascii=False))


def tool_health(_args: dict) -> str:
    result = http_json("GET", "/healthz")
    return "lightmem 状态：%s" % json.dumps(result, ensure_ascii=False)


TOOLS = [
    {
        "name": "lightmem_save",
        "description": (
            "把一条长期记忆写入 lightmem 记忆系统。"
            "当用户要求你『记住』某事，或对话中出现值得长期保留的事实、偏好、约定、结论时调用。"
            "写入是不可变的（只增不改）：同一事实更新请用 lightmem_update。"
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "fact": {"type": "string", "description": "要记住的事实内容（一句话，自包含，避免依赖上下文代词）。"},
                "memory_id": {"type": "string", "description": "可选：指定记忆 ID；不填由服务端生成。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：用户 ID，默认 dsh-agent。"},
                "agent_id": {"type": "string", "description": "可选：Agent ID，默认 deepseek-harness。"},
                "project_id": {"type": "string", "description": "可选：项目 ID。"},
                "memory_kind": {
                    "type": "string",
                    "description": "记忆类型，默认 fact。可选 fact / goal / preference / event 等。",
                },
                "visibility": {
                    "type": "string",
                    "description": "可见范围，默认 private。可选 private / shared_user / tenant / cross_tenant。",
                },
                "permission_level": {
                    "type": "string",
                    "description": "密级，默认 internal。可选 public / internal / 机密。",
                },
                "confidence": {"type": "number", "description": "可选：置信度 0~1。"},
                "importance": {"type": "number", "description": "可选：重要性 0~1。"},
                "source": {"type": "string", "description": "可选：来源标识，默认 deepseek-harness。"},
                "idempotency_key": {"type": "string", "description": "可选：幂等键，重复写入同一键会被忽略。"},
            },
            "required": ["fact"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_recall",
        "description": (
            "从 lightmem 跨层召回并做多信号融合，是记忆检索的首选工具。"
            "当用户的问题可能依赖此前保存的记忆、历史偏好或既有结论时调用。"
            "返回按融合得分排序的记忆条目及其 memory_id。"
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {"type": "string", "description": "检索查询文本（自然语言）。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：用户 ID，默认 dsh-agent。"},
                "agent_id": {"type": "string", "description": "可选：Agent ID。"},
                "project_id": {"type": "string", "description": "可选：项目 ID。"},
                "top_k": {"type": "integer", "description": "可选：返回条数。"},
                "include_archived": {"type": "boolean", "description": "可选：是否包含 L3 归档层。"},
                "as_of": {"type": "string", "description": "可选：时间回溯点（YYYY-MM-DD），用于双时态查询。"},
            },
            "required": ["text"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_search",
        "description": "lightmem 的轻量文本检索（关键词/相似度直查，不做多信号融合）。需要简单命中时使用。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {"type": "string", "description": "查询文本。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：可见性过滤用的 viewer。"},
                "top_k": {"type": "integer", "description": "可选：返回条数，默认 10。"},
                "kind": {"type": "string", "description": "可选：按记忆类型过滤。"},
            },
            "required": ["text"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_update",
        "description": (
            "更正一条已有记忆。lightmem 只增不改：本操作会写入新版本并把旧版本标记为被取代，"
            "不会物理覆盖。需要知道目标记忆的 memory_id（可由 lightmem_recall 获得）。"
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "memory_id": {"type": "string", "description": "要更正的记忆 ID。"},
                "fact": {"type": "string", "description": "新的正确内容。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：用户 ID。"},
                "confidence": {"type": "number", "description": "可选：新置信度。"},
            },
            "required": ["memory_id", "fact"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_feedback",
        "description": (
            "对一条召回结果给出反馈（adopted 采用 / rejected 拒绝 / edited 纠正），"
            "用于 lightmem 的反馈闭环与排序学习。"
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "memory_id": {"type": "string", "description": "被反馈的记忆 ID。"},
                "status": {"type": "string", "description": "adopted | rejected | edited，默认 adopted。"},
                "operator": {"type": "string", "description": "可选：反馈者标识。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：用户 ID（决定可见性 viewer），默认 dsh-agent。"},
            },
            "required": ["memory_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_forget",
        "description": "软删除一条记忆（置为历史版本并标记取代者），保留审计痕迹，可被合规流程追溯。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "memory_id": {"type": "string", "description": "要删除的记忆 ID。"},
                "actor": {"type": "string", "description": "可选：操作者标识。"},
                "tenant_id": {"type": "string", "description": "可选：租户 ID，默认 dsh。"},
                "user_id": {"type": "string", "description": "可选：用户 ID（决定可见性 viewer），默认 dsh-agent。"},
            },
            "required": ["memory_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "lightmem_health",
        "description": "检查 lightmem 服务健康状态与所用后端，排障时使用。",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
    },
]

HANDLERS = {
    "lightmem_save": tool_save,
    "lightmem_recall": tool_recall,
    "lightmem_search": tool_search,
    "lightmem_update": tool_update,
    "lightmem_feedback": tool_feedback,
    "lightmem_forget": tool_forget,
    "lightmem_health": tool_health,
}

INSTRUCTIONS = (
    "lightmem 是面向 AI Agent 与人的长期记忆系统（四层温度分层 + 8 路多信号融合）。\n"
    "当用户要求你记住某事时调用 lightmem_save；"
    "当问题可能依赖历史信息时先调用 lightmem_recall 检索，并基于返回结果作答。"
)


# ---------------------------------------------------------------------------
# MCP / JSON-RPC 层
# ---------------------------------------------------------------------------

def send(message: dict) -> None:
    sys.stdout.write(json.dumps(message, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def send_result(request_id, result: dict) -> None:
    send({"jsonrpc": "2.0", "id": request_id, "result": result})


def send_error(request_id, code: int, message: str) -> None:
    send({"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}})


def tool_text(text: str, is_error: bool = False) -> dict:
    """MCP tools/call 的返回体。"""
    result = {"content": [{"type": "text", "text": text}]}
    if is_error:
        result["isError"] = True
    return result


def handle(message: dict) -> None:
    method = message.get("method")
    request_id = message.get("id")

    if method == "initialize":
        params = message.get("params") or {}
        requested = params.get("protocolVersion") or PROTOCOL_FALLBACK
        send_result(request_id, {
            "protocolVersion": requested,
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
            "instructions": INSTRUCTIONS,
        })
        log("initialize 完成，协商协议版本=%s（客户端请求=%s）" % (requested, params.get("protocolVersion")))
        return

    if method in ("notifications/initialized", "initialized"):
        return

    if method == "ping":
        send_result(request_id, {})
        return

    if method == "tools/list":
        send_result(request_id, {"tools": TOOLS})
        return

    if method == "tools/call":
        params = message.get("params") or {}
        name = params.get("name") or ""
        args = params.get("arguments") or {}
        if not isinstance(args, dict):
            send_result(request_id, tool_text("参数必须是 JSON 对象。", is_error=True))
            return
        handler = HANDLERS.get(name)
        if handler is None:
            send_result(request_id, tool_text("未知工具：%s" % name, is_error=True))
            return
        try:
            output = handler(args)
            log("tools/call %s 成功" % name)
            send_result(request_id, tool_text(output))
        except LightmemError as exc:
            log("tools/call %s 失败：%s" % (name, exc))
            send_result(request_id, tool_text("lightmem 调用失败：%s" % exc, is_error=True))
        except Exception as exc:  # noqa: BLE001 —— 任何异常都不能杀死主循环
            log("tools/call %s 异常：%r" % (name, exc))
            send_result(request_id, tool_text("桥内部异常：%r" % (exc,), is_error=True))
        return

    # 未实现的 method：有 id 才回错误，通知类消息静默忽略。
    if request_id is not None:
        send_error(request_id, -32601, "method not found: %s" % method)
        log("未实现的 method：%s" % method)


def main() -> None:
    log("lightmem MCP 桥启动：base_url=%s tenant=%s user=%s" % (BASE_URL, TENANT_ID, USER_ID))
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except ValueError as exc:
            log("收到非法 JSON：%s" % exc)
            continue
        if isinstance(message, list):
            # JSON-RPC 批量：逐个处理（MCP 已弃用批量，但不应因此崩溃）
            for item in message:
                if isinstance(item, dict):
                    handle(item)
            continue
        try:
            handle(message)
        except Exception as exc:  # noqa: BLE001
            log("处理消息异常：%r" % (exc,))
            if message.get("id") is not None:
                send_error(message["id"], -32603, "internal error: %r" % (exc,))
    log("stdin 关闭，桥退出。")


if __name__ == "__main__":
    main()
