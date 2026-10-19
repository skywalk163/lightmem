#!/bin/sh
# =============================================================================
# R26-C（缺陷 L-237）：三远端推送命令序列 + 前置自检 + 排错指引
# =============================================================================
# 定位：一键打印 origin / gitcode / github 三远端推送命令序列，
#       附带 gitcode token 前置连通性检查与失效排错指引、
#       github git 协议 502 时的 inline API 回退路径提示。
#
# 红线⑧：本脚本**不自动执行任何 git push**。
#         所有 push 命令仅打印，由业主确认后手动执行。
#
# 用法：
#   sh scripts/push_all_remotes.sh              # 默认：打印命令序列 + 只读前置检查
#   sh scripts/push_all_remotes.sh --dry-run    # 纯预览：只打印命令序列，不做任何检查
#   sh scripts/push_all_remotes.sh --help       # 帮助
#
# 文件域（任务书 §2.2）：
#   独占：scripts/push_all_remotes.sh（本文件）
#   只读引用：.env（GITCODE_TOKEN）、lightharness/scripts/push_github_inline.py
# =============================================================================

set -u

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        --dry-run|-n)
            DRY_RUN=1
            ;;
        --help|-h)
            sed -n '2,20p' "$0"
            exit 0
            ;;
        *)
            echo "未知参数: $arg" >&2
            echo "用法: sh scripts/push_all_remotes.sh [--dry-run]" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# 路径与变量
# ---------------------------------------------------------------------------
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ENV_FILE="$ROOT/.env"
INLINE_PY="../lightharness/scripts/push_github_inline.py"   # 相对 lightmem 根目录
HEAD_SHA="$(git rev-parse HEAD 2>/dev/null || echo '(无法读取 HEAD)')"
BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'main')"

# ---------------------------------------------------------------------------
# 从 .env 提取 GITCODE_TOKEN（只提取，不 source 整个文件，避免副作用）
# ---------------------------------------------------------------------------
GITCODE_TOKEN="${GITCODE_TOKEN:-}"   # 环境变量优先
if [ -z "$GITCODE_TOKEN" ] && [ -f "$ENV_FILE" ]; then
    GITCODE_TOKEN="$(grep -E '^GITCODE_TOKEN=' "$ENV_FILE" 2>/dev/null | head -1 | cut -d= -f2-)"
    # 去除首尾引号
    GITCODE_TOKEN="$(printf '%s' "$GITCODE_TOKEN" | sed 's/^["'\'']//; s/["'\'']$//')"
fi

# ---------------------------------------------------------------------------
# 头部
# ---------------------------------------------------------------------------
echo "================================================================"
echo " R26-C：三远端推送命令序列（L-237）"
echo "================================================================"
echo " 工作树： $ROOT"
echo " 分支：   $BRANCH"
echo " HEAD：   $HEAD_SHA"
echo " 日期：   $(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo unknown)"
echo " dry-run: $([ "$DRY_RUN" -eq 1 ] && echo '是（纯预览，不做检查）' || echo '否（含只读前置检查）')"
echo ""

# ---------------------------------------------------------------------------
# [1/3] origin（gitea 192.168.1.5:3000）
# ---------------------------------------------------------------------------
echo "── [1/3] origin（gitea http://192.168.1.5:3000/skywalk/lightmem.git）──────"
echo "  推送命令： git push origin $BRANCH"
echo "  复核命令： git ls-remote origin $BRANCH"
echo ""

# ---------------------------------------------------------------------------
# [2/3] gitcode
# ---------------------------------------------------------------------------
echo "── [2/3] gitcode（https://gitcode.com/skywalk163/lightmem.git）────────────"

if [ -z "$GITCODE_TOKEN" ]; then
    echo "  ⚠️  GITCODE_TOKEN 未加载"
    echo "     .env 路径：$ENV_FILE（$([ -f "$ENV_FILE" ] && echo '文件存在' || echo '文件不存在')）"
    echo ""
    echo "  排错指引："
    echo "    1. 在 $ENV_FILE 中新增一行：GITCODE_TOKEN=<personal_access_token>"
    echo "    2. 或在 shell 中执行：export GITCODE_TOKEN=<token>"
    echo "    3. 获取 token：https://gitcode.com/profile/personal-access-tokens"
    echo "       （需要 repo 读写权限）"
    echo "    4. 验证 token 有效：git ls-remote gitcode"
    echo "       （应返回 main SHA，不出现 401/Unauthorized）"
    echo ""
    echo "  推送命令（token 修复后执行）： git push gitcode $BRANCH"
else
    echo "  GITCODE_TOKEN：已加载（长度 ${#GITCODE_TOKEN}，不显示明文）"

    if [ "$DRY_RUN" -eq 0 ]; then
        echo "  前置连通性检查：git ls-remote gitcode $BRANCH"
        if git ls-remote gitcode "$BRANCH" >/dev/null 2>&1; then
            echo "    → 连通 OK（ls-remote 成功，凭据有效）"
        else
            echo "    → ⚠️  连通失败！可能原因："
            echo "      - token 失效/过期（HTTP 401 Unauthorized）"
            echo "      - 网络不通 / DNS 解析失败"
            echo "      - 仓库不存在或无权限"
            echo ""
            echo "  排错指引："
            echo "    1. 重新生成 token：https://gitcode.com/profile/personal-access-tokens"
            echo "    2. 更新 $ENV_FILE 中的 GITCODE_TOKEN 值"
            echo "    3. 验证：git ls-remote gitcode $BRANCH（应返回 SHA）"
            echo "    4. 若 GCM 已缓存旧凭据，可尝试："
            echo "       git credential-manager erase https://gitcode.com"
            echo "       然后重试 git ls-remote"
        fi
    fi

    echo ""
    echo "  推送命令： git push gitcode $BRANCH"
fi
echo ""

# ---------------------------------------------------------------------------
# [3/3] github
# ---------------------------------------------------------------------------
echo "── [3/3] github（https://github.com/skywalk163/lightmem.git）─────────────"
echo "  主路径： git push github $BRANCH"
echo ""
echo "  回退路径（git 协议被代理 502 / TLS 握手失败时）："
echo "    python3 $INLINE_PY"
echo ""
echo "  ⚠️  inline API 通道为内容等价同步："
echo "    - commit SHA 可能与主远端不同（树内容相同）"
echo "    - 推送后须核对 tree 与 origin/$BRANCH 逐字节一致："
echo "      git rev-parse 'HEAD^{tree}'"
echo "      git rev-parse 'origin/$BRANCH^{tree}'"
echo ""
echo "  复核命令： git ls-remote github $BRANCH"
echo ""

# ---------------------------------------------------------------------------
# 推送后复核清单
# ---------------------------------------------------------------------------
echo "================================================================"
echo " 推送后复核清单（业主肉眼核对 SHA 一致）"
echo "================================================================"
echo "  1. origin:   git ls-remote origin $BRANCH"
echo "                → 应为 $HEAD_SHA"
echo ""
echo "  2. gitcode:  git ls-remote gitcode $BRANCH"
echo "                → 应与 origin 逐位相同"
echo ""
echo "  3. github:   git ls-remote github $BRANCH"
echo "                → 若走 inline 通道，SHA 可能不同；"
echo "                  须额外核对 tree 与 origin 逐字节一致"
echo "                → 核对命令："
echo "                  git rev-parse 'origin/$BRANCH^{tree}'"
echo "                  git rev-parse 'github/$BRANCH^{tree}'"
echo ""
echo "================================================================"
echo " ⚠️  红线⑧：本脚本不自动执行任何 git push。"
echo "    请业主确认后手动执行上述 push 命令序列。"
echo "================================================================"
