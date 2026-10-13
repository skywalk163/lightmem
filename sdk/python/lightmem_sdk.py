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
"""

import json
import urllib.parse
import urllib.request
import urllib.error


class LightMemClient:
    def __init__(self, base_url="http://localhost:8080", timeout=10):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    # ---- 底层 ----
    def _request(self, method, path, payload=None):
        url = self.base_url + path
        data = None
        headers = {"Content-Type": "application/json; charset=utf-8"}
        if payload is not None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
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
            return e.code, err
        except Exception as e:  # 连接失败等
            return 0, {"error": str(e)}

    # ---- CRUD ----
    def save(self, memory):
        """创建/幂等写入一条记忆。memory 至少含 tenant_id/fact/user_id。"""
        code, resp = self._request("POST", "/memories", memory)
        return code, resp

    def search(self, q, tenant_id, viewer=None, top_k=10, kind=None):
        """GET /search 文本检索。"""
        params = "q=%s&tenant_id=%s&top_k=%d" % (
            urllib.parse.quote(q), urllib.parse.quote(tenant_id), top_k)
        if viewer:
            params += "&viewer=%s" % urllib.parse.quote(viewer)
        if kind:
            params += "&kind=%s" % urllib.parse.quote(kind)
        code, resp = self._request("GET", "/search?" + params)
        return code, resp

    def recall(self, rc):
        """POST /tools/memory/recall 跨层召回 + 融合。rc 为 RouterContext 字典。"""
        code, resp = self._request("POST", "/tools/memory/recall", rc)
        return code, resp

    def update(self, memory_id, fact, tenant_id, viewer=None, user_id=None, confidence=0.0):
        """POST /tools/memory/update 编辑（追加新版本）。"""
        payload = {"memory_id": memory_id, "fact": fact, "tenant_id": tenant_id}
        if viewer:
            payload["viewer"] = viewer
        if user_id:
            payload["user_id"] = user_id
        if confidence > 0:
            payload["confidence"] = confidence
        code, resp = self._request("POST", "/tools/memory/update", payload)
        return code, resp

    # ---- GDPR 合规（P7-A）----
    def export_all(self, tenant_id, user_id):
        """§7.4 数据可携导出：返回该 (tenant,user) 作用域全部记忆。"""
        code, resp = self._request("POST", "/api/v1/gdpr/export",
                                  {"tenant_id": tenant_id, "user_id": user_id})
        return code, resp

    def cascade_delete(self, tenant_id, user_id, actor="sdk"):
        """§7.3 级联删除：删除该 (tenant,user) 作用域全部记忆。"""
        code, resp = self._request("POST", "/api/v1/gdpr/cascade-delete",
                                  {"tenant_id": tenant_id, "user_id": user_id, "actor": actor})
        return code, resp

    def forget(self, memory_id, actor="sdk"):
        """§7.5 被遗忘权：彻底移除单条记忆 + 匿名化（不保留 trace）。"""
        code, resp = self._request("POST", "/api/v1/gdpr/forget",
                                  {"memory_id": memory_id, "actor": actor})
        return code, resp


if __name__ == "__main__":
    print("LightMemClient ready. Usage: from lightmem_sdk import LightMemClient")
