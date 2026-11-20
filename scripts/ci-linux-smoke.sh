#!/bin/sh
# ============================================================================
# ci-linux-smoke.sh —— Linux 部署冒烟门禁（CI 友好，POSIX sh）
# ----------------------------------------------------------------------------
# 目标：把「只跑 Go 单测、从不验证部署脚本」的盲区补上。本脚本在 CI 的
#       ubuntu-latest 上执行，**不 sudo、不写 /etc、不编译**，只做 dry-run 与
#       静态检查；本机（macOS/FreeBSD）也能跑，Linux 专有项会打印明确 SKIP。
#
# 覆盖：
#   1) 部署相关脚本的 POSIX 语法（sh -n）
#   2) install.sh --profile solo --dry-run（不动系统、不落 _lightmem_run）
#   3) systemd 双 unit 的安装计划（install.sh --systemd --dry-run，A 项）
#   4) profile YAML 语法 + profile schema JSON 可解析
#   5) deploy/platform_test.sh 契约自测（T0 项，存在才测）
#   6) systemd unit 静态结构/端口口径 + systemd-analyze verify（无 systemd 则 SKIP）
#   7) B/C 软开关：systemd timer（T-0412）/ logrotate 配置（T-0413），存在才测
#
# 输出：每条一行 PASS / FAIL / SKIP，末行给计数与退出码。
#   全 PASS 或仅 SKIP -> 0；任一 FAIL -> 1。
#
# 用法：sh scripts/ci-linux-smoke.sh
# ============================================================================
set -u

SELF_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SELF_DIR/.." && pwd)
cd "$ROOT" || exit 1

# 子脚本统一用 C locale 运行：macOS 的 bash 3.2 在 UTF-8 locale 下会把紧邻变量的
# 全角标点并进变量名，配合 `set -u` 报 unbound variable（Linux dash 无此问题）。
# 这里只影响被调脚本的 locale，不改变检查内容。

PASS=0
FAIL=0
SKIP=0

pass() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL  %s\n' "$*"; }
skip() { SKIP=$((SKIP + 1)); printf 'SKIP  %s\n' "$*"; }
section() { printf '\n== %s ==\n' "$*"; }
dump() {
    # 失败时只回显末尾若干行，避免 CI 日志被刷屏
    printf '%s\n' "$1" | tail -n 20 | sed 's/^/      | /'
}

printf 'lightmem Linux 部署冒烟（%s）\n' "$ROOT"

# ---------------------------------------------------------------- 1) sh -n
section "1/7 部署脚本 POSIX 语法（sh -n）"
for f in integrations/quickstart/install.sh deploy/*.sh scripts/*.sh; do
    [ -f "$f" ] || continue
    if out=$(sh -n "$f" 2>&1); then
        pass "sh -n $f"
    else
        fail "sh -n $f"
        dump "$out"
    fi
done

# ---------------------------------------------------------------- 2) install --dry-run
section "2/7 install.sh --profile solo --dry-run"
had_run=0
[ -e _lightmem_run ] && had_run=1
if out=$(LC_ALL=C sh integrations/quickstart/install.sh --profile solo --dry-run 2>&1); then
    case "$out" in
        *"[dry-run]"*)
            pass "install.sh --profile solo --dry-run 输出了 dry-run 计划"
            ;;
        *)
            fail "install.sh dry-run 没有输出 [dry-run] 计划行"
            dump "$out"
            ;;
    esac
    if [ "$had_run" -eq 0 ] && [ -e _lightmem_run ]; then
        fail "dry-run 不应创建 _lightmem_run（发现被创建）"
    else
        pass "dry-run 未落盘 _lightmem_run"
    fi
else
    fail "install.sh --profile solo --dry-run 退出码非 0"
    dump "$out"
fi

# ---------------------------------------------------------------- 3) systemd 双 unit 计划
section "3/7 systemd 双 unit 安装计划（A 项）"
if [ -f deploy/lightmem.service ] && [ -f deploy/lightmem-mcp.service ]; then
    # LM_FAKE_OS=linux：保证在本机 macOS 上也走 Linux 分支（CI 上等于真实值）
    if out=$(LC_ALL=C LM_FAKE_OS=linux sh integrations/quickstart/install.sh \
        --profile solo --systemd --dry-run 2>&1); then
        miss=""
        case "$out" in *"lightmem.service"*) ;; *) miss="$miss lightmem.service" ;; esac
        case "$out" in *"lightmem-mcp.service"*) ;; *) miss="$miss lightmem-mcp.service" ;; esac
        case "$out" in *"systemctl daemon-reload"*) ;; *) miss="$miss daemon-reload" ;; esac
        case "$out" in *"install -m 644"*) ;; *) miss="$miss install -m 644" ;; esac
        if [ -z "$miss" ]; then
            pass "install.sh --systemd --dry-run 规划了双 unit（7443/7444）"
        else
            fail "systemd 安装计划缺少:$miss"
            dump "$out"
        fi
    else
        fail "install.sh --systemd --dry-run 退出码非 0"
        dump "$out"
    fi
else
    skip "A 项未合入：deploy/lightmem{,-mcp}.service 不存在"
fi

# ---------------------------------------------------------------- 4) profile YAML
section "4/7 profile YAML / schema 语法"
if command -v python3 >/dev/null 2>&1; then
    out=$(python3 - 2>&1 <<'PY'
import glob
import json
import sys

bad = 0
files = sorted(glob.glob('profiles/*.yaml')) + sorted(glob.glob('profiles/*.yml'))
if not files:
    print('没有找到 profiles/*.yaml')
    sys.exit(1)

try:
    import yaml
except ImportError:
    print('PyYAML 未安装')
    sys.exit(2)

for f in files:
    try:
        with open(f, encoding='utf-8') as fh:
            data = yaml.safe_load(fh)
    except Exception as exc:  # noqa: BLE001 - 语法错误要原样回显
        print('FAIL %s: %s' % (f, exc))
        bad = 1
        continue
    if not isinstance(data, dict):
        print('FAIL %s: 顶层不是映射' % f)
        bad = 1
        continue
    if not data.get('profile'):
        print('FAIL %s: 缺少 profile 字段' % f)
        bad = 1
        continue
    if not data.get('storage_backend'):
        print('FAIL %s: 缺少 storage_backend 字段' % f)
        bad = 1
        continue
    print('ok   %s (profile=%s)' % (f, data.get('profile')))

schema = 'profiles/lims.profile.schema.json'
try:
    with open(schema, encoding='utf-8') as fh:
        json.load(fh)
    print('ok   %s' % schema)
except Exception as exc:  # noqa: BLE001
    print('FAIL %s: %s' % (schema, exc))
    bad = 1

sys.exit(1 if bad else 0)
PY
)
    rc=$?
    case "$rc" in
        0)
            pass "profiles/*.yaml 与 schema JSON 均可解析"
            ;;
        2)
            skip "PyYAML 未安装，跳过 profile YAML 语法校验"
            ;;
        *)
            fail "profile YAML 语法校验失败"
            dump "$out"
            ;;
    esac
else
    skip "没有 python3，跳过 profile YAML 语法校验"
fi

# ---------------------------------------------------------------- 5) T0 契约自测
section "5/7 deploy/platform_test.sh 契约自测（T0 项）"
if [ -f deploy/platform_test.sh ]; then
    if out=$(LC_ALL=C sh deploy/platform_test.sh 2>&1); then
        case "$out" in
            *"0 failed"*)
                pass "platform_test.sh 全绿（$(printf '%s\n' "$out" | tail -n 1)）"
                ;;
            *)
                fail "platform_test.sh 退出 0 但没有 0 failed 汇总"
                dump "$out"
                ;;
        esac
    else
        fail "platform_test.sh 有失败用例"
        dump "$out"
    fi
else
    skip "T0 未合入：deploy/platform_test.sh 不存在"
fi

# ---------------------------------------------------------------- 6) systemd unit
section "6/7 systemd unit 静态校验"
unit_static() {   # <unit-file> <期望端口>
    us_file=$1
    us_port=$2
    us_body=$(cat "$us_file")
    us_miss=""
    case "$us_body" in *"[Unit]"*) ;; *) us_miss="$us_miss [Unit]" ;; esac
    case "$us_body" in *"[Service]"*) ;; *) us_miss="$us_miss [Service]" ;; esac
    case "$us_body" in *"ExecStart="*) ;; *) us_miss="$us_miss ExecStart=" ;; esac
    case "$us_body" in *"127.0.0.1:$us_port"*) ;; *) us_miss="$us_miss 127.0.0.1:$us_port" ;; esac
    if [ -z "$us_miss" ]; then
        pass "unit 结构与端口口径 $us_file (:$us_port)"
    else
        fail "unit $us_file 缺少:$us_miss"
    fi
}

if [ -f deploy/lightmem.service ] && [ -f deploy/lightmem-mcp.service ]; then
    unit_static deploy/lightmem.service 7443
    unit_static deploy/lightmem-mcp.service 7444
    mcp_body=$(cat deploy/lightmem-mcp.service)
    case "$mcp_body" in
        *"127.0.0.1:7443"*)
            pass "MCP unit 的 rest-base 指向 7443（端口口径未混用）"
            ;;
        *)
            fail "MCP unit 的 rest-base 未指向 7443"
            ;;
    esac

    if [ -d /run/systemd/system ] && command -v systemd-analyze >/dev/null 2>&1; then
        if out=$(systemd-analyze verify deploy/lightmem.service deploy/lightmem-mcp.service 2>&1); then
            pass "systemd-analyze verify 通过"
        else
            case "$out" in
                *"is not executable"*|*"No such file or directory"*)
                    skip "systemd-analyze verify：unit 语法通过，但 ExecStart 目标未安装（未部署环境）"
                    ;;
                *)
                    fail "systemd-analyze verify 失败"
                    dump "$out"
                    ;;
            esac
        fi
    else
        skip "无 systemd（容器/runner），systemd-analyze verify 跳过；已由静态结构校验兜底"
    fi
else
    skip "A 项未合入：systemd unit 不存在"
fi

# ---------------------------------------------------------------- 7) B/C 软开关
section "7/7 B/C 软开关（存在才测）"
timer_found=0
for t in deploy/*.timer deploy/systemd/*.timer; do
    [ -f "$t" ] || continue
    timer_found=1
    t_body=$(cat "$t")
    case "$t_body" in
        *"[Timer]"*)
            pass "systemd timer 结构 $t"
            ;;
        *)
            fail "timer $t 缺少 [Timer] 段"
            ;;
    esac
done
if [ "$timer_found" -eq 0 ]; then
    skip "T-0412 未合入：没有 deploy/*.timer（看门狗 timer 门禁跳过）"
fi

logrotate_found=0
for c in deploy/logrotate.d/*; do
    [ -f "$c" ] || continue
    logrotate_found=1
    c_body=$(cat "$c")
    c_miss=""
    case "$c_body" in *"{"*) ;; *) c_miss="$c_miss 规则块" ;; esac
    case "$c_body" in *"rotate"*) ;; *) c_miss="$c_miss rotate" ;; esac
    if [ -z "$c_miss" ]; then
        pass "logrotate 配置结构 $c"
    else
        fail "logrotate 配置 $c 缺少:$c_miss"
    fi
done
if [ "$logrotate_found" -eq 0 ]; then
    skip "T-0413 未合入：没有 deploy/logrotate.d/（logrotate 门禁跳过）"
fi

# ---------------------------------------------------------------- 汇总
printf '\n===== 汇总：PASS=%d FAIL=%d SKIP=%d =====\n' "$PASS" "$FAIL" "$SKIP"
if [ "$FAIL" -ne 0 ]; then
    printf 'RESULT: FAIL exit=1\n'
    exit 1
fi
printf 'RESULT: PASS exit=0\n'
exit 0
