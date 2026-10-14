#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""LightMem REST SDK（Python，stdlib-only，无第三方依赖）。

对应 kernel 的 HTTP API 骨架（§4.4）。覆盖 CRUD + 检索 + GDPR 合规端点。
仅用 urllib，便于在无网络/无 pip 的受限环境直接跑通。

端点清单（见 sdk/README.md）：
  POST   /memories                 创建/幂等写入一条记忆
  GET    /search?q=&tenant_id=...  文本检索
  POST   /tools/memory/recall      跨层召回 + 多信号融合
  POST   /tools/memory/update      编辑（追加新版本，不覆盖）
  POST   /api/v1/gdpr/export       数据可携导出（§7.4）
  POST   /api/v1/gdpr/cascade-delete 级联删除（§7.3）
  POST   /api/v1/gdpr/forget       被遗忘权（§7.5）

版本历史：
  0.1.0 — 基础版（R5-C 交付，stdlib-only，无重试/类型）
  0.2.0 — 生产级增强（type hints / 异常分层 / 重试 / 上下文管理器）
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional, Tuple, Union

__version__ = "0.2.0"


# ---------------------------------------------------------------------------
# 异常分层
# ---------------------------------------------------------------------------

class LightMemError(Exception):
    """LightMem SDK 基类异常。"""


class LightMemConnectionError(LightMemError):
    """网络连接 / 超时 / 不可达异常。"""


class LightMemAPIError(LightMemError):
    """API 服务端错误（4xx / 5xx）。"""

    def __init__(self, message: str, status_code: int, body: Any = None):
        super().__init__(message)
        self.status_code = status_code
        self.body = body


class LightMemNotFoundError(LightMemAPIError):
    """404 资源未找到。"""

    def __init__(self, message: str = "resource not found", body: Any = None):
        super().__init__(message, 404, body)


# ---------------------------------------------------------------------------
# 类型别名
# ---------------------------------------------------------------------------

MemoryDict = Dict[str, Any]
SearchParams = Dict[str, Any]
RecallContext = Dict[str, Any]
ResponseTuple = Tuple[int, Any]


# ---------------------------------------------------------------------------
# 客户端
# ---------------------------------------------------------------------------

class LightMemClient:
    """LightMem REST API 客户端。

    参数：
        base_url: 服务地址，默认 http://localhost:8080。
        timeout: 单次请求超时（秒），默认 10。
        retries: 重试次数（仅对连接错误 / 5xx），默认 1。
        backoff_factor: 指数退避基数（秒），默认 0.5。

    示例：
        >>> with LightMemClient() as c:
        ...     code, resp = c.save({"tenant_id": "t1", "user_id": "u1", "fact": "hello"})
    """

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        timeout: int = 10,
        retries: int = 1,
        backoff_factor: float = 0.5,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.retries = retries
        self.backoff_factor = backoff_factor
        self._closed = False

    # ---- 上下文管理器 ----

    def __enter__(self) -> "LightMemClient":
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.close()

    def close(self) -> None:
        """关闭客户端（当前无持久连接，仅标记状态）。"""
        self._closed = True

    # ---- 底层 ----

    def _request(
        self,
        method: str,
        path: str,
        payload: Optional[Dict[str, Any]] = None,
    ) -> ResponseTuple:
        """发送 HTTP 请求，含重试逻辑。

        仅对连接错误（urllib.error.URLError）和 5xx 响应重试；
        4xx 不重试（属于客户端错误，重试无意义）。
        """
        url = self.base_url + path
        data: Optional[bytes] = None
        headers = {"Content-Type": "application/json; charset=utf-8"}
        if payload is not None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")

        last_exc: Optional[Exception] = None
        for attempt in range(self.retries + 1):
            if self._closed:
                raise LightMemConnectionError("client closed")
            req = urllib.request.Request(url, data=data, headers=headers, method=method)
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                    body = resp.read().decode("utf-8")
                    return resp.status, (json.loads(body) if body else {})
            except urllib.error.HTTPError as e:
                body = e.read().decode("utf-8", "replace")
                try:
                    err = json.loads(body)
                except Exception:
                    err = {"error": body}
                # 5xx 重试，4xx 直接返回
                if 500 <= e.code < 600 and attempt < self.retries:
                    time.sleep(self.backoff_factor * (2 ** attempt))
                    last_exc = e
                    continue
                if e.code == 404:
                    raise LightMemNotFoundError(body, err) from e
                if 400 <= e.code < 500:
                    raise LightMemAPIError(f"API error {e.code}", e.code, err) from e
                raise LightMemAPIError(f"server error {e.code}", e.code, err) from e
            except urllib.error.URLError as e:
                # 连接错误，重试
                if attempt < self.retries:
                    time.sleep(self.backoff_factor * (2 ** attempt))
                    last_exc = e
                    continue
                raise LightMemConnectionError(f"connection failed: {e}") from e
            except Exception as e:
                raise LightMemConnectionError(f"unexpected error: {e}") from e

        # 不应到达此处，但为安全保留
        if last_exc is not None:
            raise LightMemConnectionError(f"all retries exhausted: {last_exc}")
        raise LightMemConnectionError("unknown request error")

    # ---- CRUD ----

    def save(self, memory: MemoryDict) -> ResponseTuple:
        """创建/幂等写入一条记忆。

        参数：
            memory: 记忆字典，至少含 tenant_id / fact / user_id。

        返回：
            (status_code, response_body)
        """
        return self._request("POST", "/memories", memory)

    def search(
        self,
        q: str,
        tenant_id: str,
        viewer: Optional[str] = None,
        top_k: int = 10,
        kind: Optional[str] = None,
    ) -> ResponseTuple:
        """GET /search 文本检索。

        参数：
            q: 查询文本。
            tenant_id: 租户 ID。
            viewer: 可见性过滤的 viewer（可选）。
            top_k: 返回条数，默认 10。
            kind: 记忆类型过滤（可选）。

        返回：
            (status_code, response_body)
        """
        params = "q=%s&tenant_id=%s&top_k=%d" % (
            urllib.parse.quote(q), urllib.parse.quote(tenant_id), top_k
        )
        if viewer:
            params += "&viewer=%s" % urllib.parse.quote(viewer)
        if kind:
            params += "&kind=%s" % urllib.parse.quote(kind)
        return self._request("GET", "/search?" + params)

    def recall(self, rc: RecallContext) -> ResponseTuple:
        """POST /tools/memory/recall 跨层召回 + 融合。

        参数：
            rc: RouterContext 字典，至少含 text / tenant_id。

        返回：
            (status_code, response_body)
        """
        return self._request("POST", "/tools/memory/recall", rc)

    def update(
        self,
        memory_id: str,
        fact: str,
        tenant_id: str,
        viewer: Optional[str] = None,
        user_id: Optional[str] = None,
        confidence: float = 0.0,
    ) -> ResponseTuple:
        """POST /tools/memory/update 编辑（追加新版本）。

        参数：
            memory_id: 要更新的记忆 ID。
            fact: 新事实文本。
            tenant_id: 租户 ID。
            viewer: 可见性过滤（可选）。
            user_id: 用户 ID（可选）。
            confidence: 置信度（可选，>0 时发送）。

        返回：
            (status_code, response_body)
        """
        payload: Dict[str, Any] = {
            "memory_id": memory_id,
            "fact": fact,
            "tenant_id": tenant_id,
        }
        if viewer:
            payload["viewer"] = viewer
        if user_id:
            payload["user_id"] = user_id
        if confidence > 0:
            payload["confidence"] = confidence
        return self._request("POST", "/tools/memory/update", payload)

    # ---- GDPR 合规（P7-A）----

    def export_all(self, tenant_id: str, user_id: str) -> ResponseTuple:
        """§7.4 数据可携导出：返回该 (tenant,user) 作用域全部记忆。

        参数：
            tenant_id: 租户 ID。
            user_id: 用户 ID。

        返回：
            (status_code, response_body)
        """
        return self._request(
            "POST",
            "/api/v1/gdpr/export",
            {"tenant_id": tenant_id, "user_id": user_id},
        )

    def cascade_delete(
        self, tenant_id: str, user_id: str, actor: str = "sdk"
    ) -> ResponseTuple:
        """§7.3 级联删除：删除该 (tenant,user) 作用域全部记忆。

        参数：
            tenant_id: 租户 ID。
            user_id: 用户 ID。
            actor: 操作者标识，默认 "sdk"。

        返回：
            (status_code, response_body)
        """
        return self._request(
            "POST",
            "/api/v1/gdpr/cascade-delete",
            {"tenant_id": tenant_id, "user_id": user_id, "actor": actor},
        )

    def forget(self, memory_id: str, actor: str = "sdk") -> ResponseTuple:
        """§7.5 被遗忘权：彻底移除单条记忆 + 匿名化（不保留 trace）。

        参数：
            memory_id: 要删除的记忆 ID。
            actor: 操作者标识，默认 "sdk"。

        返回：
            (status_code, response_body)
        """
        return self._request(
            "POST",
            "/api/v1/gdpr/forget",
            {"memory_id": memory_id, "actor": actor},
        )


if __name__ == "__main__":
    print(f"LightMemClient v{__version__} ready. Usage: from lightmem_sdk import LightMemClient")
