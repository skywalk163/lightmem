#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""R29-F：极简压测探针（stdlib，无第三方依赖；任务书 §7.3 / §9.1.2）。

用途：R13（批量扫描 P95 劣化 ≤20%）与 R25（探针风暴下的业务 P95 不劣化）的
固定条件压测器。**只发 HTTP，不碰进程、不改数据**。

用法：
  python3 scripts/perf_probe.py \
      --url http://127.0.0.1:18443 \
      --token "$TOK" \
      --seconds 60 --concurrency 8 \
      --path '/search?q=test&tenant_id=t1' \
      --p95-out /tmp/p95_off.txt

输出（stdout，一行 JSON + 人类可读摘要）：
  {"requests":N,"errors":E,"rps":R,"p50":..,"p95":..,"p99":..,"codes":{...}}

退出码：0 = 跑完（无论有无 5xx）；2 = 参数/连接错误（一个请求都没成功）。
"""

import argparse
import json
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request


def _one(url, token, timeout):
    req = urllib.request.Request(url, headers={"User-Agent": "r29-perf-probe"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            r.read()
            code = r.status
    except urllib.error.HTTPError as e:
        code = e.code
        try:
            e.read()
        except Exception:
            pass
    except Exception:
        code = -1
    return (time.perf_counter() - t0) * 1000.0, code


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True, help="base url, e.g. http://127.0.0.1:18443")
    ap.add_argument("--path", default="/search?q=test&tenant_id=t1")
    ap.add_argument("--token", default="")
    ap.add_argument("--seconds", type=float, default=60.0)
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--timeout", type=float, default=5.0)
    ap.add_argument("--p95-out", default="", help="把 P95(ms) 写成单行文本，便于脚本比对")
    ap.add_argument("--json-out", default="", help="完整结果落 JSON")
    ap.add_argument("--label", default="", help="标签（写进输出，便于多轮对比）")
    args = ap.parse_args()

    url = args.url.rstrip("/") + args.path
    stop_at = time.perf_counter() + args.seconds
    lat = []
    codes = {}
    lock = threading.Lock()

    def worker():
        local_lat, local_codes = [], {}
        while time.perf_counter() < stop_at:
            ms, code = _one(url, args.token, args.timeout)
            local_lat.append(ms)
            local_codes[code] = local_codes.get(code, 0) + 1
        with lock:
            lat.extend(local_lat)
            for k, v in local_codes.items():
                codes[k] = codes.get(k, 0) + v

    ths = [threading.Thread(target=worker) for _ in range(max(1, args.concurrency))]
    t0 = time.perf_counter()
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    elapsed = max(1e-6, time.perf_counter() - t0)

    if not lat:
        print(json.dumps({"label": args.label, "requests": 0, "error": "no request completed"}))
        return 2

    lat.sort()

    def pct(p):
        if not lat:
            return 0.0
        idx = min(len(lat) - 1, int(round((p / 100.0) * (len(lat) - 1))))
        return round(lat[idx], 2)

    errors = sum(v for k, v in codes.items() if k < 0 or k >= 500)
    out = {
        "label": args.label,
        "path": args.path,
        "concurrency": args.concurrency,
        "seconds": round(elapsed, 2),
        "requests": len(lat),
        "errors": errors,
        "rps": round(len(lat) / elapsed, 1),
        "p50": pct(50),
        "p95": pct(95),
        "p99": pct(99),
        "mean": round(statistics.fmean(lat), 2),
        "max": round(lat[-1], 2),
        "codes": {str(k): v for k, v in sorted(codes.items())},
    }
    print(json.dumps(out, ensure_ascii=False, sort_keys=True))
    print("[probe] label=%s n=%d rps=%.1f p50=%.2fms p95=%.2fms p99=%.2fms errors=%d codes=%s"
          % (args.label, out["requests"], out["rps"], out["p50"], out["p95"], out["p99"],
             out["errors"], out["codes"]), flush=True)

    if args.p95_out:
        with open(args.p95_out, "w", encoding="utf-8") as f:
            f.write("%.2f\n" % out["p95"])
    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as f:
            json.dump(out, f, ensure_ascii=False, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
