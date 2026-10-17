#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""lightmem token 签发工具（零依赖，Linux/macOS/FreeBSD/Windows 通用）。

用途：为每个 AI Agent 签发一份独立 token。服务端据此确定身份
（agent_id / user_id / tenant_id），从而 **杜绝冒充**：Agent 只能以自己
的身份写记忆，请求体里谎报 user_id 也没用。

用法：
    # 交互式签发（最常用）
    python3 issue_token.py --tenant myteam --user alice --agents dsh,trae,workbuddy

    # 指定输出目录与文件名
    python3 issue_token.py --out /etc/lightmem --agents dsh,trae

    # 签发单个并只打印 token（脚本里用）
    python3 issue_token.py --agent trae --quiet

产出：
    agent-tokens.json          服务端用（整表；喂给 LIMS_AGENT_TOKENS）
    clients/<agent>.json       客户端用（**只含该 agent 那一条**）
    tokens.csv                 人看的清单（含明文 token，注意保管！）

为什么客户端只要"一条"：
    stdio 通道（CodeArts / Roo / Cline）没有 HTTP 头，无法区分身份。
    lightmem 的约定是「每个 stdio Agent 各起一个进程，只带自己那一条 token」，
    于是"进程身份"唯一确定。带整表会被服务端拒绝（多身份无法区分，宁可无身份
    也不猜测，避免把 A 的身份安到 B 头上）。

安全提醒：
    - 产出文件含**明文凭据**，务必 chmod 600 并纳入密钥管理；
    - 不要提交进 git（本工具的产出目录应加进 .gitignore）；
    - 生产环境请换发正式 token，不要用示例/测试值。
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import secrets
import sys
import stat

# ---------------------------------------------------------------------------
# 默认：一份覆盖主流 AI coding agent 的清单
# ---------------------------------------------------------------------------
DEFAULT_AGENTS = [
    "dsh",        # DeepSeek Harness
    "trae",       # Trae CN
    "trae-solo",  # TRAE SOLO CN
    "workbuddy",  # WorkBuddy
    "raccoon",    # 小浣熊 / Box-Agent
    "opencode",   # OpenCode / DuMate
    "codearts",   # 华为 CodeArts（stdio）
    "roo-cline",  # Roo Code / Cline 系（stdio）
]

# agent 键 -> agent_id（多数同名；DSH 用可读名）
AGENT_ID_MAP = {
    "dsh": "deepseek-harness",
}


def token_for(key: str) -> str:
    """生成不可猜测的 token（占位前缀便于识别，随机体 32 hex = 128 bit）。"""
    return "t-%s-%s" % (key, secrets.token_hex(16))


def write_json(path: str, obj, secret: bool = False):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(obj, f, ensure_ascii=False, indent=2)
        f.write("\n")
    if secret and os.name != "nt":          # Windows 下 chmod 语义有限，跳过
        try:
            os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)   # 0600
        except OSError:
            pass


def main() -> int:
    ap = argparse.ArgumentParser(
        description="为 AI Agent 签发 lightmem token（零依赖）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__)
    ap.add_argument("--agents", default=",".join(DEFAULT_AGENTS),
                    help="逗号分隔的 agent 键（默认：8 家主流 agent）")
    ap.add_argument("--agent", help="只签发这一个 agent（与 --agents 二选一）")
    ap.add_argument("--tenant", default="myteam", help="租户 ID（默认 myteam）")
    ap.add_argument("--user", default="alice", help="用户名（默认 alice）")
    ap.add_argument("--out", default=".", help="输出目录（默认当前目录）")
    ap.add_argument("--quiet", action="store_true",
                    help="只把 token 打到 stdout（配合 --agent 用）")
    ap.add_argument("--dry-run", action="store_true", help="只显示计划，不写文件")
    args = ap.parse_args()

    keys = [args.agent] if args.agent else [a.strip() for a in args.agents.split(",") if a.strip()]
    if not keys:
        print("FATAL: 没有指定任何 agent", file=sys.stderr)
        return 2

    table = {}
    mapping = {}
    for k in keys:
        tok = token_for(k)
        table[tok] = {
            "agent_id": AGENT_ID_MAP.get(k, k),
            "user_id": args.user,
            "tenant_id": args.tenant,
        }
        mapping[k] = tok

    if args.agent and args.quiet:
        # 只打印 token，方便 shell 取值
        sys.stdout.write(mapping[args.agent] + "\n")
        if not args.dry_run:
            write_json(os.path.join(args.out, "agent-tokens.json"), table, secret=True)
            write_json(os.path.join(args.out, "clients", args.agent + ".json"), table, secret=True)
        return 0

    print("=" * 72)
    print("lightmem token 签发")
    print("=" * 72)
    print("  租户 tenant_id : %s" % args.tenant)
    print("  用户 user_id   : %s" % args.user)
    print("  agent 数       : %d" % len(keys))
    print("  输出目录       : %s" % os.path.abspath(args.out))
    print()

    print("--- 各 Agent 的 token（**明文，注意保管**）---")
    for k in keys:
        print("  %-12s agent_id=%-18s %s" % (k, AGENT_ID_MAP.get(k, k), mapping[k]))
    print()

    if args.dry_run:
        print("[dry-run] 未写任何文件。")
        return 0

    # 1) 服务端整表
    srv = os.path.join(args.out, "agent-tokens.json")
    write_json(srv, table, secret=True)
    print("[写入] 服务端整表      : %s  (0600)" % srv)

    # 2) 客户端单条（stdio 场景的关键）
    cli_dir = os.path.join(args.out, "clients")
    os.makedirs(cli_dir, exist_ok=True)
    for k in keys:
        write_json(os.path.join(cli_dir, k + ".json"), {mapping[k]: table[mapping[k]]}, secret=True)
    print("[写入] 客户端单条      : %s/  (%d 个文件，0600)" % (cli_dir, len(keys)))

    # 3) 人看的清单
    csv_path = os.path.join(args.out, "tokens.csv")
    with open(csv_path, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f)
        w.writerow(["agent", "agent_id", "user_id", "tenant_id", "token"])
        for k in keys:
            w.writerow([k, AGENT_ID_MAP.get(k, k), args.user, args.tenant, mapping[k]])
    if os.name != "nt":
        try:
            os.chmod(csv_path, stat.S_IRUSR | stat.S_IWUSR)
        except OSError:
            pass
    print("[写入] 人工清单        : %s  (0600)" % csv_path)

    print()
    print("=" * 72)
    print("下一步")
    print("=" * 72)
    print("  1) 让服务端加载整表（二选一）：")
    print("     a. 启动时注入： LIMS_AGENT_TOKENS=$(cat %s)" % srv)
    print("     b. 或写进 profile 的 auth.agent_tokens")
    print("  2) 给每个客户端配 MCP 时：**stdio 用 clients/<agent>.json**（单条），")
    print("     HTTP 用该 agent 的 token 填 headers.Authorization: Bearer <token>")
    print("  3) 生成配置片段： python3 gen_configs.py --server <你的地址> --tokens %s" % srv)
    print()
    print("  ⚠️ 上述文件含明文凭据：请 chmod 600、勿入 git、生产前换发。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
