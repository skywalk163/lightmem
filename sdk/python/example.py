#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""LightMem Python SDK CRUD 示例。

演示：创建 → 检索 → 更新 → 导出 → 被遗忘（删除）完整闭环。
需先启动 kernel 服务（sqlite 骨架）：
  lightmem-kernel -backend-override sqlite -listen :8080

运行：
  python3 example.py [base_url]
"""

import sys

from lightmem_sdk import LightMemClient


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080"
    c = LightMemClient(base)

    tenant, user = "demo-tenant", "demo-user"
    print("== LightMem SDK CRUD 示例 (base=%s) ==" % base)

    # 1) Create
    mem = {
        "tenant_id": tenant,
        "user_id": user,
        "memory_kind": "fact",
        "fact": "用户 skywalk 偏好用中文交流",
        "visibility": "private",
        "confidence": 0.9,
        "permission": {"owner": user},
    }
    code, resp = c.save(mem)
    assert code in (200, 201), "save failed: %s %s" % (code, resp)
    mid = resp.get("memory_id")
    print("[C] create -> code=%s memory_id=%s" % (code, mid))
    assert mid, "memory_id missing"

    # 2) Read (search)
    code, resp = c.search("中文交流", tenant_id=tenant, viewer=user)
    assert code == 200, "search failed: %s" % resp
    hits = resp if isinstance(resp, list) else resp.get("memories", [])
    print("[R] search -> code=%s hits=%d" % (code, len(hits)))
    assert len(hits) >= 1, "search returned no hits"

    # 2b) Read (recall + fuse)
    code, resp = c.recall({"text": "中文交流", "tenant_id": tenant, "user_id": user})
    assert code == 200, "recall failed: %s" % resp
    fused = resp.get("fused", [])
    print("[R] recall+fuse -> code=%s fused=%d" % (code, len(fused)))

    # 3) Update
    code, resp = c.update(mid, "用户 skywalk 偏好用中文与英文混合交流",
                          tenant_id=tenant, viewer=user, user_id=user)
    assert code == 200, "update failed: %s" % resp
    print("[U] update -> code=%s new_id=%s" % (code, resp.get("memory_id")))

    # 再检索确认更新生效
    code, resp = c.search("英文混合", tenant_id=tenant, viewer=user)
    hits2 = resp if isinstance(resp, list) else resp.get("memories", [])
    print("[R] search-after-update -> code=%s hits=%d" % (code, len(hits2)))
    assert len(hits2) >= 1, "updated fact not searchable"

    # 4) Export (GDPR §7.4)
    code, resp = c.export_all(tenant, user)
    assert code == 200, "export failed: %s" % resp
    print("[GDPR] export -> code=%s exported=%s" % (code, resp.get("exported")))

    # 5) Delete (GDPR §7.5 被遗忘权)
    code, resp = c.forget(mid, actor=user)
    assert code == 200, "forget failed: %s" % resp
    print("[D] forget -> code=%s result=%s" % (code, resp))

    # 删除后检索应不再命中该条
    code, resp = c.search("中文交流", tenant_id=tenant, viewer=user)
    hits3 = resp if isinstance(resp, list) else resp.get("memories", [])
    print("[R] search-after-forget -> code=%s hits=%d" % (code, len(hits3)))

    print("\n== CRUD + GDPR 闭环全部跑通 ==")


if __name__ == "__main__":
    main()
