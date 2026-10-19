#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""R25-E · 检索延迟 P95 基线（只读 bench 脚本）。

任务书 §8.1 辅助判据 1：检索延迟 P95（0.88，/search，800 条语料），
建立基线并保证不劣化 >10%。本脚本**只读**：不改任何产品/评测代码，
仅驱动一个已在跑的 lightmem-kernel 实例做「灌语料 → 补向量 → 计时检索」。

设计口径（与 lightmem.md §9 对齐）：
  - P95 只计「内核记忆检索链路」端到端延迟（本脚本一次 HTTP 请求的 wall time，
    含 embedder 向量化 + 后端检索 + 序列化响应）。
  - 顺序单请求（无并发），排除客户端排队噪声；warmup 消除冷缓存/首次 JIT 类抖动。
  - 确定性：语料顺序固定、查询等距抽样固定、无随机数；同一语料 + 同一服务端
    可逐位复现。

用法（0.88，solo/SQLite 独立临时库，勿碰生产库 lims_dsh）：
    # 1) 起独立 solo 服务（临时 SQLite，hash embedder，B 线已让 /search/vector 可用）
    mkdir -p /tmp/r25_bench/data
    cd /home/workbuddy/lightmem/kernel
    go build -o /tmp/r25_bench/lightmem-kernel ./cmd/lightmem-kernel
    ( cd /tmp/r25_bench && nohup ./lightmem-kernel \
        -profile /home/workbuddy/lightmem/profiles/lims-solo.profile.yaml \
        -listen 127.0.0.1:18455 >server.log 2>&1 & )
    # 2) 跑 bench（/search 与 /search/vector 各一遍）
    python3 eval/scripts/bench_p95.py --base http://127.0.0.1:18455 \
        --corpus eval/holdout/holdout_v2.jsonl --load 800 --queries 300 \
        --endpoint search  --report eval/reports/r25_p95_baseline.md
    python3 eval/scripts/bench_p95.py --base http://127.0.0.1:18455 \
        --corpus eval/holdout/holdout_v2.jsonl --load 800 --queries 300 \
        --endpoint vector

依赖：Python 3 标准库（urllib/json/time/argparse/hashlib），零第三方依赖。
"""
import argparse
import hashlib
import json
import math
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HTTP_TIMEOUT = 30.0


def http_json(url, method="GET", body=None, timeout=HTTP_TIMEOUT):
    """发一次 HTTP 请求，返回 (status, parsed_json_or_text, elapsed_ms)。"""
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            code = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        code = e.code
    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    try:
        parsed = json.loads(raw.decode("utf-8"))
    except Exception:
        parsed = raw.decode("utf-8", "replace")
    return code, parsed, elapsed_ms


def load_corpus(path):
    """读 holdout jsonl，返回 [(memory_dict, query_str, tenant, user), ...]。"""
    rows = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            d = json.loads(line)
            mem = d.get("memory") or {}
            # 归一化 tenant/user：缺省用记录级 tenant/user，再缺省 t1/u1，
            # 保证 upsert 与检索用同一身份（viewer 能看见自己写的记忆）。
            tenant = mem.get("tenant_id") or d.get("tenant") or "t1"
            user = mem.get("user_id") or d.get("user") or "u1"
            mem.setdefault("tenant_id", tenant)
            mem.setdefault("user_id", user)
            mem.setdefault("visibility", "tenant")
            mem.setdefault("memory_kind", "fact")
            mem.setdefault("idempotency_key", mem.get("memory_id"))
            if not mem.get("permission"):
                mem["permission"] = {"owner": user}
            rows.append((mem, d.get("query", ""), tenant, user))
    return rows


def phase_load(base, rows, n):
    """灌前 n 条语料（POST /memories，幂等）。返回成功条数。"""
    ok = 0
    for i in range(min(n, len(rows))):
        mem, _, _, _ = rows[i]
        code, _, _ = http_json(base + "/memories", "POST", mem)
        if code in (200, 201):
            ok += 1
        else:
            print("  [load] %s -> HTTP %s" % (mem.get("memory_id"), code), file=sys.stderr)
    return ok


def phase_backfill(base, timeout=300.0, retries=8, pause=3.0):
    """触发向量补全（幂等，可重试收敛）。

    服务端 handleBackfill 自带 30s ctx 超时；大语料（800 条 BLOB 逐条落盘，
    每条一事务一 fsync）单次调用可能跑不满。好在 BackfillVectors 幂等
    （existingIDs 跳过已存在者），故按「多次调用逐次推进」收敛：
    每轮要么成功返回，要么服务端推进一部分（客户端超时也无所谓），
    循环直到 vectorized==0（无新补全 = 全部就位）或重试上限。
    """
    for attempt in range(1, retries + 1):
        try:
            code, body, ms = http_json(base + "/vector/backfill", "POST", {}, timeout=timeout)
        except Exception as e:
            print("  [backfill] attempt %d: client error (%s)，等服务端推进后重试" % (attempt, type(e).__name__))
            time.sleep(pause)
            continue
        print("  [backfill] attempt %d: HTTP %s in %.1fms -> %s" % (attempt, code, ms, body))
        if code == 200 and isinstance(body, dict):
            vec = body.get("vectorized", 0)
            if vec == 0:
                return code  # 已全部就位
            time.sleep(pause)  # 还有新增补，继续推进
        else:
            print("  [backfill] 非 200 响应：%s" % body, file=sys.stderr)
            return code
    print("  [backfill] 达到重试上限（%d 次），以当前向量状态继续" % retries, file=sys.stderr)
    return 0


def percentile(sorted_vals, p):
    """nearest-rank 百分位（与常见 SLO 口径一致）。"""
    if not sorted_vals:
        return float("nan")
    k = max(0, math.ceil(p / 100.0 * len(sorted_vals)) - 1)
    return sorted_vals[k]


def phase_bench(base, rows, endpoint, queries, warmup, topk):
    """等距抽样 queries 条查询，warmup 后计时，返回统计 dict + 原始 latencies。"""
    n = len(rows)
    # 等距抽样（确定性，覆盖全语料）
    idxs = [ (i * n) // queries for i in range(queries) ] if queries < n else list(range(n))
    seen = set()
    sample = []
    for i in idxs:
        if i in seen:
            continue
        seen.add(i)
        sample.append(rows[i])
    sample = sample[:queries]

    def one(row):
        mem, q, tenant, user = row
        if endpoint == "vector":
            url = base + "/search/vector"
            code, _, ms = http_json(url, "POST",
                                    {"query": q, "tenant_id": tenant, "viewer": user, "top_k": topk})
        else:
            qs = urllib.parse.urlencode({"tenant_id": tenant, "q": q, "viewer": user, "top_k": topk})
            code, _, ms = http_json(base + "/search?" + qs, "GET")
        return code, ms

    # warmup（不计入统计）
    for row in sample[:warmup]:
        one(row)

    lat = []
    codes = {}
    for row in sample:
        code, ms = one(row)
        codes[code] = codes.get(code, 0) + 1
        lat.append(ms)
    lat.sort()
    stats = {
        "endpoint": endpoint,
        "n": len(lat),
        "codes": codes,
        "mean_ms": sum(lat) / len(lat) if lat else float("nan"),
        "p50_ms": percentile(lat, 50),
        "p90_ms": percentile(lat, 90),
        "p95_ms": percentile(lat, 95),
        "p99_ms": percentile(lat, 99),
        "max_ms": lat[-1] if lat else float("nan"),
    }
    return stats, lat


def fmt(v):
    return "%.2f" % v if v == v else "n/a"  # nan -> n/a


def main():
    ap = argparse.ArgumentParser(description="R25-E 检索延迟 P95 基线 bench（只读）")
    ap.add_argument("--base", default="http://127.0.0.1:18455")
    ap.add_argument("--corpus", default="eval/holdout/holdout_v2.jsonl")
    ap.add_argument("--load", type=int, default=800, help="灌入语料条数（默认 800）")
    ap.add_argument("--queries", type=int, default=300, help="计时查询条数（默认 300）")
    ap.add_argument("--warmup", type=int, default=20)
    ap.add_argument("--topk", type=int, default=5)
    ap.add_argument("--endpoint", choices=["search", "vector"], default="search")
    ap.add_argument("--no-load", action="store_true", help="跳过灌库（语料已在库时）")
    ap.add_argument("--report", default="", help="可选：把基线写成 markdown 报告路径")
    args = ap.parse_args()

    # 健康检查
    try:
        code, health, _ = http_json(args.base + "/health", "GET")
    except Exception as e:
        print("FATAL: 连不上 %s/health：%s" % (args.base, e), file=sys.stderr)
        return 2
    print("== health == HTTP %s -> %s" % (code, json.dumps(health, ensure_ascii=False)))

    corpus_md5 = ""
    try:
        with open(args.corpus, "rb") as f:
            corpus_md5 = hashlib.md5(f.read()).hexdigest()
    except Exception as e:
        print("WARN: 读语料失败：%s" % e, file=sys.stderr)

    rows = load_corpus(args.corpus)
    tenants = sorted({r[2] for r in rows})
    print("== corpus == %s  md5=%s  rows=%d  tenants=%s" %
          (args.corpus, corpus_md5, len(rows), tenants[:5]))

    loaded = 0
    if not args.no_load:
        print("== load == POST /memories x%d ..." % min(args.load, len(rows)))
        loaded = phase_load(args.base, rows, args.load)
        print("  [load] ok=%d" % loaded)

    if args.endpoint == "vector":
        phase_backfill(args.base)

    print("== bench == endpoint=/%s queries=%d warmup=%d topk=%d" %
          (args.endpoint, args.queries, args.warmup, args.topk))
    stats, _ = phase_bench(args.base, rows, args.endpoint, args.queries, args.warmup, args.topk)

    print("\n---- P95 baseline (%s) ----" % args.endpoint)
    print("n=%s codes=%s" % (stats["n"], stats["codes"]))
    print("mean=%sms p50=%sms p90=%sms p95=%sms p99=%sms max=%sms" % (
        fmt(stats["mean_ms"]), fmt(stats["p50_ms"]), fmt(stats["p90_ms"]),
        fmt(stats["p95_ms"]), fmt(stats["p99_ms"]), fmt(stats["max_ms"])))

    if args.report:
        lines = []
        lines.append("# R25-E 检索延迟 P95 基线（%s）\n" % args.endpoint)
        lines.append("- 验证机：0.88（FreeBSD，go1.25.14）；独立 solo/SQLite 临时库（勿碰生产 lims_dsh）")
        lines.append("- 语料：`%s`（md5 `%s`，%d 条）" % (args.corpus, corpus_md5, len(rows)))
        lines.append("- 灌库：%d 条；embedder=hash（solo profile dimensions=512）" % loaded)
        lines.append("- 端点：`/%s`；queries=%d warmup=%d topk=%d" %
                     (args.endpoint, args.queries, args.warmup, args.topk))
        lines.append("- HTTP 状态分布：%s" % stats["codes"])
        lines.append("")
        lines.append("| 指标 | ms |")
        lines.append("|---|---|")
        lines.append("| mean | %s |" % fmt(stats["mean_ms"]))
        lines.append("| P50 | %s |" % fmt(stats["p50_ms"]))
        lines.append("| P90 | %s |" % fmt(stats["p90_ms"]))
        lines.append("| **P95** | **%s** |" % fmt(stats["p95_ms"]))
        lines.append("| P99 | %s |" % fmt(stats["p99_ms"]))
        lines.append("| max | %s |" % fmt(stats["max_ms"]))
        with open(args.report, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        print("  [report] -> %s" % args.report)

    return 0


if __name__ == "__main__":
    sys.exit(main())
