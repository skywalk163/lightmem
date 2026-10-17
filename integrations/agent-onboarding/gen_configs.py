#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""lightmem 客户端配置生成器（零依赖，Windows/macOS/Linux/FreeBSD 通用）。

把「服务端地址 + token 表」变成 **可直接粘贴** 的各家 AI Agent 配置片段。

用法：
    # 最常用：生成全部 8 家的配置片段
    python3 gen_configs.py --server http://192.168.0.88:18444 --tokens ./agent-tokens.json

    # 只生成某几家
    python3 gen_configs.py --server ... --tokens ... --agents dsh,trae

    # stdio 类（CodeArts/Roo）需要本机二进制路径
    python3 gen_configs.py --server ... --tokens ... \
        --mcp-binary /usr/local/lightmem/lightmem-mcp

产出（默认写到 ./out/）：
    out/
      MANIFEST.md              逐家「粘到哪、怎么粘」的操作清单
      dsh.cordis.patch.yml     DSH（DeepSeek Harness）
      trae.mcp.json            Trae CN
      trae-solo.mcp.json       TRAE SOLO CN
      trae-workspace.mcp.json  Trae 工作区级变体
      workbuddy.mcp.json       WorkBuddy
      raccoon.mcp.json         小浣熊 / Box-Agent
      opencode.json            OpenCode / DuMate
      codearts.mcp_settings.json  CodeArts（stdio）
      roo-cline.mcp_settings.json Roo Code / Cline 系（stdio）
      agent-prompt.md          **可直接粘给 AI 的安装 prompt**

设计要点：
  1. **各家字段名不同，且写错会静默不生效**（不报错、工具永远不出现）。
     本生成器把三种 HTTP 标记写法固化正确：
        DSH   : transport: streamable-http   （连字符）
        WorkBuddy: type: "streamableHttp"    （驼峰）
        小浣熊 : type: "streamable_http"     （下划线）
        Trae   : url + headers（无类型标记）
        OpenCode: type: "remote"，顶层键是 mcp
  2. **stdio 两家按「单条 token」生成**（不是整表）—— 见 issue_token.py 的说明。
  3. 产出**不覆盖任何真实配置**，只放到 out/ 供人工粘贴。
"""

from __future__ import annotations

import argparse
import json
import os
import sys

# agent 键 -> agent_id（与 issue_token.py 保持一致）
AGENT_ID_MAP = {"dsh": "deepseek-harness"}

ALL_AGENTS = ["dsh", "trae", "trae-solo", "trae-workspace", "workbuddy",
              "raccoon", "opencode", "codearts", "roo-cline"]

# 走 HTTP 传输的（能吃 url+headers）
HTTP_AGENTS = {"dsh", "trae", "trae-solo", "trae-workspace", "workbuddy", "raccoon", "opencode"}
# 只能吃 stdio 的（CodeArts 实测仅 local；Roo/Cline 系传统 stdio）
STDIO_AGENTS = {"codearts", "roo-cline"}


def load_tokens(path: str) -> dict:
    with open(path, encoding="utf-8") as f:
        t = json.load(f)
    if not isinstance(t, dict) or not t:
        raise SystemExit("token 表必须是非空 JSON 对象：%s" % path)
    return t


def want_agent_id(key: str) -> str:
    """配置键 -> 期望的 agent_id。

    trae-workspace 是 Trae 的**工作区级变体**，与用户级 trae 是同一个 Agent，
    因此复用 trae 的 token（不额外签发）。
    """
    if key == "trae-workspace":
        return "trae"
    return AGENT_ID_MAP.get(key, key)


def token_by_agent(table: dict, key: str) -> str:
    """从整表里挑出某个 agent 的 token（整表可能只含一家）。"""
    want = want_agent_id(key)
    for tok, v in table.items():
        if v.get("agent_id") == want:
            return tok
    # 兜底：整表只有一条时就直接用它（用户可能只签了一家）
    if len(table) == 1:
        return next(iter(table))
    raise SystemExit("token 表里找不到 agent=%s（现有：%s）"
                     % (key, sorted(v.get("agent_id", "?") for v in table.values())))


def one_entry(table: dict, key: str) -> dict:
    tok = token_by_agent(table, key)
    return {tok: table[tok]}


def write(path: str, content: str, outdir: str):
    full = os.path.join(outdir, path)
    os.makedirs(os.path.dirname(os.path.abspath(full)), exist_ok=True)
    with open(full, "w", encoding="utf-8", newline="\n") as f:
        f.write(content)
    return full


def dump(obj) -> str:
    return json.dumps(obj, ensure_ascii=False, indent=2) + "\n"


def main() -> int:
    ap = argparse.ArgumentParser(description="生成各家 AI Agent 的 lightmem MCP 配置片段")
    ap.add_argument("--server", required=True,
                    help="lightmem MCP 服务地址，如 http://192.168.0.88:18444（会自动补 /mcp）")
    ap.add_argument("--tokens", required=True, help="agent-tokens.json 路径（issue_token.py 产出）")
    ap.add_argument("--agents", default=",".join(ALL_AGENTS), help="逗号分隔，默认全部")
    ap.add_argument("--mcp-binary", default="/usr/local/lightmem/lightmem-mcp",
                    help="stdio 类用的 lightmem-mcp 二进制路径（默认 /usr/local/lightmem/lightmem-mcp）")
    ap.add_argument("--rest-base", default="http://127.0.0.1:18443",
                    help="stdio 桥转发的 REST 地址（默认 http://127.0.0.1:18443）")
    ap.add_argument("--out", default="out", help="输出目录（默认 ./out）")
    args = ap.parse_args()

    url = args.server.rstrip("/")
    if not url.endswith("/mcp"):
        url += "/mcp"

    keys = [a.strip() for a in args.agents.split(",") if a.strip()]
    bad = [k for k in keys if k not in ALL_AGENTS]
    if bad:
        raise SystemExit("未知 agent：%s\n可选：%s" % (bad, ALL_AGENTS))

    table = load_tokens(args.tokens)
    # 先**全部校验**再落盘：否则会先写一半、再因缺 token 报错，留下残缺产物。
    for k in keys:
        try:
            token_by_agent(table, k)
        except SystemExit as exc:
            raise SystemExit(
                "%s\n\n提示：token 表必须覆盖你要生成的每一家。\n"
                "  要么 --agents 只填已签发的家，要么重跑 issue_token.py 补签。\n"
                "  当前 token 表覆盖：%s" % (exc, sorted(v.get("agent_id", "?") for v in table.values())))
    outdir = os.path.abspath(args.out)
    written = []

    def bearer(key):
        return "Bearer " + token_by_agent(table, key)

    # ---------------- DSH ----------------
    if "dsh" in keys:
        written.append(write("dsh.cordis.patch.yml", """# lightmem 记忆系统 —— DeepSeek Harness（DSH）
# 目标文件（桌面版）：%%USERPROFILE%%\\.dsh\\profiles\\desktop\\cordis.patch.yml
#   macOS/Linux 对应：~/.dsh/profiles/desktop/cordis.patch.yml
# 粘贴方式：**追加到该文件末尾**（该文件同时承载模型/UI 等配置，整文件覆盖会冲掉它们）
# 生效：保存即被 dsh-hmr 热重载，**无需重启**
# 命名陷阱：HTTP 传输标记是 `transport: streamable-http`（全小写 + 连字符）
- insert:
    - id: mcp-lightmem
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: lightmem
        transport: streamable-http
        url: '%s'
        headers:
          Authorization: '%s'
        toolCallTimeoutMs: 30000
        failOnStartupError: false
        reconnect:
          enabled: true
          initialDelayMs: 500
          maxDelayMs: 30000
          maxAttempts: 10
""" % (url, bearer("dsh")), outdir))

    # ---------------- Trae（三个变体） ----------------
    def trae_body(key):
        return dump({"mcpServers": {"lightmem": {
            "url": url, "headers": {"Authorization": bearer(key)}}}})

    if "trae" in keys:
        written.append(write("trae.mcp.json", trae_body("trae"), outdir))
    if "trae-solo" in keys:
        written.append(write("trae-solo.mcp.json", trae_body("trae-solo"), outdir))
    if "trae-workspace" in keys:
        written.append(write("trae-workspace.mcp.json", trae_body("trae"), outdir))

    # ---------------- WorkBuddy（驼峰 streamableHttp） ----------------
    if "workbuddy" in keys:
        written.append(write("workbuddy.mcp.json", dump({"mcpServers": {"lightmem": {
            "type": "streamableHttp",       # ← 驼峰！写成下划线/连字符会静默不生效
            "url": url,
            "headers": {"Authorization": bearer("workbuddy")},
            "timeout": 30000, "disabled": False}}}), outdir))

    # ---------------- 小浣熊 / Box-Agent（下划线 streamable_http） ----------------
    if "raccoon" in keys:
        written.append(write("raccoon.mcp.json", dump({"mcpServers": {"lightmem": {
            "type": "streamable_http",      # ← 下划线！写成驼峰会静默不生效
            "url": url,
            "headers": {"Authorization": bearer("raccoon")},
            "alwaysLoad": True, "disabled": False,
            "connect_timeout": 15, "execute_timeout": 60}}}), outdir))

    # ---------------- OpenCode / DuMate（顶层 mcp + type remote） ----------------
    if "opencode" in keys:
        written.append(write("opencode.json", dump({
            "$schema": "https://opencode.ai/config.json",
            "mcp": {"lightmem": {           # ← 顶层键是 mcp（不是 mcpServers）
                "type": "remote",
                "url": url,
                "enabled": True,
                "headers": {"Authorization": bearer("opencode")}}}}), outdir))

    # ---------------- CodeArts（stdio，command 为数组） ----------------
    if "codearts" in keys:
        written.append(write("codearts.mcp_settings.json", dump({
            "_comment": "CodeArts：type=local，command 必须是**数组**；stdio 用「单条」token 表",
            "mcp": {"lightmem": {
                "enabled": True, "timeout": 60000, "type": "local",
                "command": [args.mcp_binary, "-transport", "stdio",
                            "-rest-base", args.rest_base],
                "environment": {"LIMS_AGENT_TOKENS": "<本机 clients/codearts.json 的绝对路径>"}}}}), outdir))

    # ---------------- Roo Code（stdio，command 为字符串 + args） ----------------
    if "roo-cline" in keys:
        written.append(write("roo-cline.mcp_settings.json", dump({
            "_comment": "Roo/Cline：command 是**字符串** + args 数组；stdio 用「单条」token 表",
            "mcpServers": {"lightmem": {
                "type": "stdio",
                "command": args.mcp_binary,
                "args": ["-transport", "stdio", "-rest-base", args.rest_base],
                "env": {"LIMS_AGENT_TOKENS": "<本机 clients/roo-cline.json 的绝对路径>"},
                "alwaysAllow": [], "disabled": False}}}), outdir))

    # ---------------- MANIFEST ----------------
    manifest = build_manifest(url, keys, args, table)
    written.append(write("MANIFEST.md", manifest, outdir))

    # ---------------- 给 AI 的安装 prompt ----------------
    written.append(write("agent-prompt.md", build_prompt(url, keys, args, table), outdir))

    print("=" * 72)
    print("lightmem 配置片段已生成")
    print("=" * 72)
    print("  服务地址 : %s" % url)
    print("  agent 数 : %d" % len(keys))
    print("  输出目录 : %s" % outdir)
    print()
    for p in written:
        print("  %s" % os.path.relpath(p, outdir))
    print()
    print("下一步：打开 out/MANIFEST.md 逐家粘贴；或把 out/agent-prompt.md 粘给 AI 让它自动装。")
    return 0


# ---------------------------------------------------------------------------
# MANIFEST：逐家「粘到哪、怎么粘」
# ---------------------------------------------------------------------------
TARGETS = {
    "dsh":
        ("追加", "DSH(DeepSeek Harness)",
         "%USERPROFILE%\\.dsh\\profiles\\desktop\\cordis.patch.yml",
         "~/.dsh/profiles/desktop/cordis.patch.yml"),
    "trae":
        ("新建", "Trae CN",
         "%APPDATA%\\Trae CN\\User\\mcp.json",
         "~/.config/Trae CN/User/mcp.json"),
    "trae-solo":
        ("新建", "TRAE SOLO CN",
         "%APPDATA%\\TRAE SOLO CN\\User\\mcp.json",
         "~/.config/TRAE SOLO CN/User/mcp.json"),
    "trae-workspace":
        ("新建", "Trae（工作区级，优先于用户级）",
         "<工作区根>\\.trae\\mcp.json",
         "<repo>/.trae/mcp.json"),
    "workbuddy":
        ("合并", "WorkBuddy",
         "%USERPROFILE%\\.workbuddy\\mcp.json",
         "~/.workbuddy/mcp.json"),
    "raccoon":
        ("合并", "小浣熊 / Box-Agent（用户层）",
         "%USERPROFILE%\\.box-agent\\config\\mcp.json",
         "~/.box-agent/config/mcp.json"),
    "opencode":
        ("合并", "OpenCode / DuMate（工作区级）",
         "<工作区根>\\opencode.json",
         "<repo>/opencode.json"),
    "codearts":
        ("合并", "CodeArts（含镜像目录）",
         "%USERPROFILE%\\.codearts\\mcp\\mcp_settings.json",
         "~/.codearts/mcp/mcp_settings.json"),
    "roo-cline":
        ("新建", "Roo Code / Cline 系",
         "%APPDATA%\\Code\\User\\globalStorage\\rooveterinaryinc.roo-cline\\settings\\mcp_settings.json",
         "~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/settings/mcp_settings.json"),
}

FILE_OF = {
    "dsh": "dsh.cordis.patch.yml",
    "trae": "trae.mcp.json",
    "trae-solo": "trae-solo.mcp.json",
    "trae-workspace": "trae-workspace.mcp.json",
    "workbuddy": "workbuddy.mcp.json",
    "raccoon": "raccoon.mcp.json",
    "opencode": "opencode.json",
    "codearts": "codearts.mcp_settings.json",
    "roo-cline": "roo-cline.mcp_settings.json",
}

# 传输 / 类型标记 / 顶层键 —— 三大陷阱一览
CONTRACT = {
    "dsh": ("HTTP", "transport: streamable-http", "- insert: 补丁条目"),
    "trae": ("HTTP", "url + headers（无类型标记）", "mcpServers"),
    "trae-solo": ("HTTP", "url + headers（无类型标记）", "mcpServers"),
    "trae-workspace": ("HTTP", "url + headers（无类型标记）", "mcpServers"),
    "workbuddy": ("HTTP", "type: \"streamableHttp\"（驼峰）", "mcpServers"),
    "raccoon": ("HTTP", "type: \"streamable_http\"（下划线）", "mcpServers"),
    "opencode": ("HTTP", "type: \"remote\"", "mcp"),
    "codearts": ("stdio", "type: \"local\"；command 为数组", "mcp"),
    "roo-cline": ("stdio", "type: \"stdio\"；command 为字符串 + args", "mcpServers"),
}


def build_manifest(url, keys, args, table) -> str:
    L = []
    L.append("# lightmem 接入清单（MANIFEST）\n")
    L.append("> 本文件由 `gen_configs.py` 生成。逐行照做即可。\n")
    L.append("> 服务地址：`%s`\n" % url)
    L.append("\n## 一、总表：粘到哪、怎么粘\n")
    L.append("| # | Agent | 传输 | 类型标记（**写错会静默失效**） | 顶层键 | 目标文件（Windows） | 方式 |")
    L.append("|---|---|---|---|---|---|---|")
    for i, k in enumerate(keys, 1):
        tr, mark, top = CONTRACT[k]
        _, name, win, _ = TARGETS[k]
        how = TARGETS[k][0]
        L.append("| %d | %s | %s | `%s` | `%s` | `%s` | %s |" % (i, name, tr, mark, top, win, how))
    L.append("\n> **`合并` = 该文件已有其它条目，只加 lightmem 这一块，不要整体覆盖。**")
    L.append("> **`新建` = 该文件可能不存在，直接创建即可。**\n")

    L.append("\n## 二、三大命名陷阱（同一语义，三种写法）\n")
    L.append("| Agent | HTTP 类型标记 | 顶层键 |")
    L.append("|---|---|---|")
    L.append("| DSH | `streamable-http`（连字符） | `- insert:` 补丁条目 |")
    L.append("| WorkBuddy | `streamableHttp`（**驼峰**） | `mcpServers` |")
    L.append("| 小浣熊 / Box-Agent | `streamable_http`（**下划线**） | `mcpServers` |")
    L.append("| Trae | 无类型标记，用 `url` | `mcpServers` |")
    L.append("| OpenCode / DuMate | `remote` | `mcp` |")
    L.append("| CodeArts | `local`（仅 stdio） | `mcp`，`command` 为**数组** |")
    L.append("| Roo Code | `stdio` | `mcpServers`，`command` 为**字符串**+`args` |")
    L.append("\n> **写错不会报错**，只会「工具永远不出现」。出现这种情况先查这一栏。\n")

    L.append("\n## 三、逐家操作\n")
    for k in keys:
        _, name, win, nix = TARGETS[k]
        L.append("### %s\n" % name)
        L.append("- 用文件：`%s`" % FILE_OF[k])
        L.append("- Windows 目标：`%s`" % win)
        L.append("- macOS/Linux 目标：`%s`" % nix)
        L.append("- 粘贴方式：**%s**" % TARGETS[k][0])
        if k == "dsh":
            L.append("- 生效：保存即热重载，**无需重启**")
        elif k in ("trae", "trae-solo", "trae-workspace"):
            L.append("- 生效：**完全退出 Trae 再启动**（MCP 不热重载）")
        elif k == "workbuddy":
            L.append("- 生效：重启 WorkBuddy；**首次调用需在界面点一次「允许」**（它有逐服务器审批门）")
        elif k == "raccoon":
            L.append("- 生效：重启小浣熊 / Box-Agent")
        elif k == "opencode":
            L.append("- 生效：重启 opencode 进程")
        elif k == "codearts":
            L.append("- 生效：重启 IDE；**另有镜像目录** `~/.codeartswork/mcp/` 需同步一份")
            L.append("- 前置：`%s` 需存在于**该客户端所在机器**（stdio 是本机子进程）" % args.mcp_binary)
            L.append("- 前置：把 `clients/codearts.json` 的**绝对路径**填进 `environment.LIMS_AGENT_TOKENS`")
        elif k == "roo-cline":
            L.append("- 生效：重载 VS Code 窗口")
            L.append("- 前置：`%s` 需存在于本机；把 `clients/roo-cline.json` 绝对路径填进 `env.LIMS_AGENT_TOKENS`" % args.mcp_binary)
        L.append("")

    L.append("\n## 四、验证（每粘完一家都做一遍）\n")
    L.append("在该 Agent 的对话里说：\n")
    L.append("```\n调用 lightmem_health，把返回原样贴给我。\n```")
    L.append("\n**期望**：返回 JSON 里含 `\"status\":\"ok\"`。")
    L.append("若工具没出现 → 先查 §二 的命名陷阱；若返回 401 → token 不对。\n")

    L.append("\n## 五、装完立刻能做的事\n")
    L.append("```")
    L.append("用 lightmem_save 记住这件事：<你要它记的内容>")
    L.append("先用 lightmem_recall 查一下我们之前关于 <主题> 的结论，再动手。")
    L.append("用 lightmem_goal_create 建一个计划：<目标>，deadline <日期>")
    L.append("```")
    L.append("\n> 第二条是「共享记忆大脑」的日常用法：**动手前先回忆**。\n")
    return "\n".join(L) + "\n"


def build_prompt(url, keys, args, table) -> str:
    """生成一段可直接粘给「有文件读写权限的 AI」的自我安装 prompt。"""
    win_dir = "%USERPROFILE%\\.dsh\\profiles\\desktop\\cordis.patch.yml"
    lines = []
    lines.append("# 给 AI Agent 的安装指令（可直接粘贴）\n")
    lines.append("> 把下面 `---` 之间的**全部内容**复制给你本机的 AI（需要有文件读写权限的那种），")
    lines.append("> 它就会自动把 lightmem 的 MCP 配置装好。\n")
    lines.append("---\n")
    lines.append("请帮我把 lightmem「共享记忆大脑」接入本机的 AI 工具。按以下步骤做，每步做完向我确认：\n")
    lines.append("")
    lines.append("**服务地址**：`" + url + "`")
    lines.append("")
    lines.append("**第 1 步 — 备份**：")
    lines.append("对每个即将修改的配置文件，先复制一份带 `.lightmem-bak` 后缀的备份。")
    lines.append("")
    lines.append("**第 2 步 — 写入配置**（只改 MCP 相关字段，**不要动文件里其它已有条目**）：")
    lines.append("")
    # 注意：目标路径含字面 `%USERPROFILE%`，故**不能用 % 格式化**，改用拼接。
    lines.append("- **DSH / DeepSeek Harness** → 追加到 `" + win_dir + "` 末尾：")
    lines.append("")
    lines.append("```yaml")
    lines.append(read_generated("dsh.cordis.patch.yml", outdir=args.out) or "# (未生成)")
    lines.append("```")
    lines.append("")
    for k in keys:
        if k == "dsh":
            continue
        _, name, win, _ = TARGETS[k]
        body = read_generated(FILE_OF[k], outdir=args.out)
        if not body:
            continue
        lines.append("- **" + name + "**（" + TARGETS[k][0] + " `" + FILE_OF[k] + "`）→ 目标 `" + win + "`：")
        lines.append("")
        lines.append("```json" if FILE_OF[k].endswith(".json") else "```")
        lines.append(body)
        lines.append("```")
        lines.append("")
    lines.append("**第 3 步 — 提醒我重启**：")
    lines.append("Trae / SOLO / WorkBuddy / 小浣熊 / Roo / OpenCode 需要**完全退出再启动**才生效；")
    lines.append("DSH 保存即热重载。请列出哪些需要我手动重启。")
    lines.append("")
    lines.append("**第 4 步 — 验证**：")
    lines.append("重启后，让对应 AI 调用 `lightmem_health`，确认返回里有 `\"status\":\"ok\"`。")
    lines.append("如果工具不出现，检查**类型标记写法**：")
    lines.append("DSH 用 `streamable-http`（连字符）、WorkBuddy 用 `streamableHttp`（驼峰）、")
    lines.append("小浣熊用 `streamable_http`（下划线）—— 写错不会报错，只会不生效。")
    lines.append("")
    lines.append("**注意**：")
    lines.append("- 这些配置里含明文 token，写入后请确认文件权限（Linux/macOS 用 `chmod 600`）。")
    lines.append("- 不要把这些 token 提交进任何 git 仓库。")
    lines.append("- 若服务端是**后补签/换发** token：必须**重启 lightmem-mcp 服务**新 token 才生效")
    lines.append("  （服务端只在启动时装配 token 表；换表后旧 token 会被拒为新 token 之外的 401）。")
    lines.append("---\n")
    return "\n".join(lines) + "\n"


def read_generated(name: str, outdir: str) -> str:
    p = os.path.join(outdir, name)
    if not os.path.exists(p):
        return ""
    with open(p, encoding="utf-8") as f:
        return f.read().rstrip("\n")


if __name__ == "__main__":
    sys.exit(main())
