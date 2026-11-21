#!/bin/sh
# envfile_test.sh —— 回归：install.sh 写 systemd EnvironmentFile 前必须把 agent-tokens.json 压成**单行**
#
# 背景（T-0424 / T-0417 TODO#1）：EnvironmentFile **按行解析**。旧写法
#     printf 'LIMS_AGENT_TOKENS=%s\n' "$(cat agent-tokens.json)" > lightmem.env
# 把多行美化 JSON 原样塞进去，systemd 只保留首行 `{` → kernel 启动即
#     parse agent tokens json: unexpected end of JSON input → status=1/FAILURE → Start request repeated too quickly
#
# 判据（真实产出 + 断言，不是「grep 到某个字符串就算过」）：
#   1) 多行美化 agent-tokens.json → lightmem.env **恰好 1 行**（wc -l = 1）
#   2) 首行以 LIMS_AGENT_TOKENS= 开头，去掉前缀后 json.loads 成功
#   3) 解析回来的 token 与源 JSON **等价**（字段/大小写/值一个不差）
#   4) 权限 600；且 agent-tokens.json 一个字节都没被改（sha256 前后一致）
#   5) 主路径走 scripts/lm-env-compact.sh；只拿到 integrations/ 子集时走内联 python3 退化路径
#   6) 反跑 A：造「旧写法」的多行 env，同一套判据必须**变红**（证明判据真的在测东西）
#   7) 反跑 B：python3 不可用时 install.sh 必须**明确失败**，且不落盘会崩的 env
#   8) RUN_DIR 属主归一（T-0425）：非 root 运行时**不**调用 chown（保持原语义）
#   9) root + --service-user 时必须调用 chown，目标与 --service-user 一致，且 data/logs=750、env/tokens=600
#  10) 反跑 C：把 chown 目标改到允许前缀之外（/tmp、/）必须被**拒绝**且一次 chown 都不发
#
# 用法（仓库根）：bash integrations/quickstart/envfile_test.sh
# 依赖：POSIX sh + python3（install.sh 压平 JSON 本就需要 python3，测试沿用同一依赖）
# 注意：Windows(MSYS) 上 mode 600 与 POSIX 语义不完全一致，验收请在 Linux/FreeBSD 上跑。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL="$HERE/install.sh"
REPO=$(cd "$HERE/../.." && pwd)

FAILED=0
CHECKS=0
pass() { CHECKS=$((CHECKS + 1)); printf '  [v] %s\n' "$*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf '  [x] %s\n' "$*" >&2; }
note() { printf '  ... %s\n' "$*"; }
dump_log() { sed -n '1,25p' "$1" 2>/dev/null | sed 's/^/      | /' >&2; }

[ -f "$INSTALL" ] || { echo "FATAL: 找不到 $INSTALL" >&2; exit 1; }
if ! command -v python3 >/dev/null 2>&1; then
    echo "FATAL: 需要 python3（与 install.sh 同一依赖：压平 JSON 用它）" >&2
    exit 1
fi

TMP=${TMPDIR:-/tmp}/lm-envfile-test.$$
OWNROOT="$REPO/.hive/tmp-own.$$"   # 属主归一用例必须落在 $ROOT 前缀内（安全边界要求）
trap 'rm -rf "$TMP" "$OWNROOT"' EXIT HUP INT TERM
mkdir -p "$TMP"

# ---------------------------------------------------------------- 夹具：~35 行美化 JSON（贴近 issue_token.py 产物）
make_tokens() {   # $1 = 目标文件
    cat > "$1" <<'JSON'
{
  "version": 1,
  "tenant": "myteam",
  "user": "alice",
  "agents": [
    {
      "id": "agent-t0417",
      "name": "quickstart-agent",
      "token": "lm_sk_0123456789abcdef0123456789abcdef",
      "created_at": "2026-11-20T07:41:00+08:00",
      "scopes": [
        "memory:read",
        "memory:write"
      ],
      "meta": {
        "issued_by": "issue_token.py",
        "note": "记忆大脑·中文备注（ensure_ascii=False 口径）"
      }
    },
    {
      "id": "agent-mcp",
      "name": "mcp-client",
      "token": "lm_sk_fedcba9876543210fedcba9876543210",
      "created_at": "2026-11-20T07:41:00+08:00",
      "scopes": [
        "memory:read"
      ],
      "meta": {
        "issued_by": "issue_token.py",
        "note": "多行美化 JSON 是 systemd EnvironmentFile 的杀手"
      }
    }
  ]
}
JSON
}

# 判据本体：rc 0 = 合格的单行 EnvironmentFile；非 0 = 不合格（stderr 说明哪一条挂了）
check_env() {   # $1 = env 文件, $2 = 源 tokens 文件
    python3 - "$1" "$2" <<'PY'
import json, os, stat, sys

env_path, tokens_path = sys.argv[1], sys.argv[2]
if not os.path.isfile(env_path):
    sys.exit('env 文件不存在：%s' % env_path)
with open(tokens_path, encoding='utf-8') as f:
    want = json.load(f)
with open(env_path, encoding='utf-8') as f:
    raw = f.read()
prefix = 'LIMS_AGENT_TOKENS='
newlines = raw.count('\n')
if newlines != 1 or not raw.endswith('\n'):
    sys.exit('env 行数=%d（期望恰好 1 行 + 结尾换行）' % newlines)
line = raw[:-1]
if not line.startswith(prefix):
    sys.exit('首行不是 %s 开头：%.40s' % (prefix, line))
try:
    got = json.loads(line[len(prefix):])
except Exception as exc:
    sys.exit('单行值不是合法 JSON：%s' % exc)
if got != want:
    sys.exit('env 里的 token 与 %s 不等价（字段/大小写/值不一致）' % tokens_path)
mode = stat.S_IMODE(os.stat(env_path).st_mode)
if mode != 0o600:
    sys.exit('权限=%o（期望 600）' % mode)
PY
}

hash_file() {
    python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"
}

printf '== envfile_test: install.sh 写 EnvironmentFile 必须单行 ==\n'
printf '   仓库: %s\n' "$(cd "$HERE/../.." && pwd)"

# ---------------------------------------------------------------- 1) 主路径（仓库完整 → scripts/lm-env-compact.sh）
RUN1="$TMP/run1"
mkdir -p "$RUN1"
make_tokens "$RUN1/agent-tokens.json"
TOKENS_SHA_BEFORE=$(hash_file "$RUN1/agent-tokens.json")
note "输入 agent-tokens.json 行数 = $(wc -l < "$RUN1/agent-tokens.json" | tr -d ' ')（多行）"

if sh "$INSTALL" --write-token-env "$RUN1" >"$TMP/log1" 2>&1; then
    pass "install.sh --write-token-env（scripts/lm-env-compact.sh 路径）rc=0"
else
    fail "install.sh --write-token-env（compact 路径）非 0 退出"
    dump_log "$TMP/log1"
fi
if [ -f "$RUN1/lightmem.env" ]; then
    L1=$(wc -l < "$RUN1/lightmem.env" | tr -d ' ')
else
    L1=none
fi
if [ "$L1" = "1" ]; then
    pass "实际产出 lightmem.env 行数 wc -l = 1"
else
    fail "实际产出 lightmem.env 行数 wc -l = ${L1}（期望 1）"
fi
if check_env "$RUN1/lightmem.env" "$RUN1/agent-tokens.json"; then
    pass "单行前缀 + json 等价 + 600"
else
    fail "check_env 判定不合格（见上方原因）"
fi
if [ "$(hash_file "$RUN1/agent-tokens.json")" = "$TOKENS_SHA_BEFORE" ]; then
    pass "agent-tokens.json 未被改动（sha256 前后一致）"
else
    fail "agent-tokens.json 被改动了（红线：token 往返语义不得变）"
fi

# ---------------------------------------------------------------- 2) 退化路径（只拿到 integrations/ 子集，scripts/ 不可达）
MINI="$TMP/mini"
mkdir -p "$MINI/integrations/quickstart"
cp "$INSTALL" "$MINI/integrations/quickstart/install.sh"
RUN2="$TMP/run2"
mkdir -p "$RUN2"
make_tokens "$RUN2/agent-tokens.json"
if sh "$MINI/integrations/quickstart/install.sh" --write-token-env "$RUN2" >"$TMP/log2" 2>&1; then
    pass "退化路径（无 scripts/lm-env-compact.sh → 内联 python3）rc=0"
else
    fail "退化路径非 0 退出"
    dump_log "$TMP/log2"
fi
if [ -f "$RUN2/lightmem.env" ]; then
    L2=$(wc -l < "$RUN2/lightmem.env" | tr -d ' ')
else
    L2=none
fi
[ "$L2" = "1" ] && pass "退化路径产出也是 1 行（wc -l = 1）" || fail "退化路径产出 ${L2} 行（期望 1）"
if check_env "$RUN2/lightmem.env" "$RUN2/agent-tokens.json"; then
    pass "退化路径：前缀 + json 等价 + 600"
else
    fail "退化路径 check_env 判定不合格"
fi

# ---------------------------------------------------------------- 3) 反跑 A：旧写法（多行原样塞进去）必须变红
OLD="$TMP/old"
mkdir -p "$OLD"
make_tokens "$OLD/agent-tokens.json"
OLDLINES=$(wc -l < "$OLD/agent-tokens.json" | tr -d ' ')
( umask 077; printf 'LIMS_AGENT_TOKENS=%s\n' "$(cat "$OLD/agent-tokens.json")" > "$OLD/lightmem.env" )
chmod 600 "$OLD/lightmem.env" 2>/dev/null || true
OLD_ENV_LINES=$(wc -l < "$OLD/lightmem.env" | tr -d ' ')
if [ "$OLD_ENV_LINES" -gt 1 ]; then
    pass "反跑样本复现旧失败形态：env 实际 $OLD_ENV_LINES 行（源 JSON $OLDLINES 行）"
else
    fail "反跑样本没有复现多行（env 只有 $OLD_ENV_LINES 行，判据将失去意义）"
fi
if check_env "$OLD/lightmem.env" "$OLD/agent-tokens.json" >"$TMP/old_check.out" 2>"$TMP/old_check.err"; then
    fail "反跑没变红：多行 env 竟然被判合格（判据是空的）"
else
    pass "反跑 A 如期变红：同一套判据拒绝多行 env（$(sed -n '1p' "$TMP/old_check.err")）"
fi

# ---------------------------------------------------------------- 4) 反跑 B：python3 不可用 → 明确失败、不落盘
NBIN="$TMP/nobin"
mkdir -p "$NBIN"
for _c in dirname mkdir chmod; do
    _p=$(command -v "$_c" 2>/dev/null || true)
    [ -n "$_p" ] && ln -sf "$_p" "$NBIN/$_c"
done
SH_BIN=$(command -v sh)
RUN3="$TMP/run3"
mkdir -p "$RUN3"
make_tokens "$RUN3/agent-tokens.json"
if ( PATH="$NBIN"; "$SH_BIN" "$INSTALL" --write-token-env "$RUN3" ) >"$TMP/log3" 2>&1; then
    fail "python3 不可用时 install.sh 竟然 rc=0（应明确失败）"
else
    pass "python3 不可用 → install.sh 非 0 退出（没有静默写出会崩的 env）"
fi
if [ -f "$RUN3/lightmem.env" ]; then
    fail "python3 不可用时仍写出了 lightmem.env（半份会崩的 env 不得落盘）"
else
    pass "python3 不可用：未落盘 lightmem.env"
fi
if grep -q 'python3' "$TMP/log3" 2>/dev/null; then
    pass "失败提示里点了 python3（可操作）"
else
    fail "失败提示没说 python3（用户无从下手）"
fi

# ---------------------------------------------------------------- 5) RUN_DIR 属主归一（T-0425 / T-0417 TODO#2）
# 用 PATH 垫片模拟 root（假 id）并拦截 chown（只记账不真改），这样非 root 的验收机也能测两个分支。
OB="$OWNROOT/bin"
mkdir -p "$OB" "$OWNROOT"
cat > "$OB/id" <<'STUB'
#!/bin/sh
case "${1:-}" in
  -u)  if [ "$#" -ge 2 ]; then echo 1000; else echo "${LM_FAKE_UID:-0}"; fi ;;
  -un) if [ "$#" -ge 2 ]; then echo "$2"; else echo "${LM_FAKE_USER:-root}"; fi ;;
  *)   echo 0 ;;
esac
STUB
cat > "$OB/chown" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "${LM_CHOWN_LOG:?LM_CHOWN_LOG 未设置}"
STUB
chmod 755 "$OB/id" "$OB/chown"
perm_of() { python3 -c 'import os,stat,sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode))[2:])' "$1"; }

# 5a) root + --service-user tester → 必须 chown 到 tester:tester 且权限归一
RA="$OWNROOT/run-root"
mkdir -p "$RA/data"
: > "$RA/lightmem.env"; : > "$RA/agent-tokens.json"
CLOG_A="$OWNROOT/chown-a.log"; : > "$CLOG_A"
if PATH="$OB:$PATH" LM_FAKE_UID=0 LM_FAKE_USER=root LM_CHOWN_LOG="$CLOG_A" \
        "$SH_BIN" "$INSTALL" --own-run-dir "$RA" --service-user tester >"$OWNROOT/log-a" 2>&1; then
    pass "root 模拟：--own-run-dir rc=0"
else
    fail "root 模拟：--own-run-dir 非 0 退出"
    dump_log "$OWNROOT/log-a"
fi
if grep -qF -- "-R tester:tester $RA" "$CLOG_A"; then
    pass "root 模拟：确实调用了 chown -R tester:tester <RUN_DIR>（目标与 --service-user 一致）"
else
    fail "root 模拟：没有按 service-user 调用 chown（实际：$(cat "$CLOG_A" 2>/dev/null | tr '\n' '|')）"
fi
[ "$(perm_of "$RA/data")" = 750 ] && pass "root 模拟：data/ = 750" || fail "data/ 权限 = $(perm_of "$RA/data")（期望 750）"
[ "$(perm_of "$RA/logs")" = 750 ] && pass "root 模拟：logs/ = 750" || fail "logs/ 权限 = $(perm_of "$RA/logs")（期望 750）"
[ "$(perm_of "$RA/lightmem.env")" = 600 ] && pass "root 模拟：lightmem.env = 600" || fail "env 权限 = $(perm_of "$RA/lightmem.env")（期望 600）"
[ "$(perm_of "$RA/agent-tokens.json")" = 600 ] && pass "root 模拟：agent-tokens.json = 600" || fail "tokens 权限 = $(perm_of "$RA/agent-tokens.json")（期望 600）"

# 5b) 非 root → 一次 chown 都不许发（纯 quickstart 语义不变）
RB="$OWNROOT/run-nonroot"
mkdir -p "$RB"
CLOG_B="$OWNROOT/chown-b.log"; : > "$CLOG_B"
if PATH="$OB:$PATH" LM_FAKE_UID=1000 LM_FAKE_USER=workbuddy LM_CHOWN_LOG="$CLOG_B" \
        "$SH_BIN" "$INSTALL" --own-run-dir "$RB" --service-user tester >"$OWNROOT/log-b" 2>&1; then
    pass "非 root：--own-run-dir rc=0（跳过而非报错）"
else
    fail "非 root：--own-run-dir 不该非 0 退出"
    dump_log "$OWNROOT/log-b"
fi
if [ -s "$CLOG_B" ]; then
    fail "非 root 竟然调用了 chown（纯 quickstart 语义被改：$(cat "$CLOG_B" | tr '\n' '|')）"
else
    pass "非 root：未调用 chown（保持原语义）"
fi
if grep -q '跳过 chown' "$OWNROOT/log-b" 2>/dev/null; then
    pass "非 root：输出明确写了「跳过 chown」"
else
    fail "非 root：没有明确打印跳过原因"
fi

# 5c) 反跑 C：目标改到允许前缀之外（/tmp、/）必须被拒绝，且一次 chown 都不发
for _bad in /tmp /; do
    CLOG_C="$OWNROOT/chown-c.log"; : > "$CLOG_C"
    if PATH="$OB:$PATH" LM_FAKE_UID=0 LM_FAKE_USER=root LM_CHOWN_LOG="$CLOG_C" \
            "$SH_BIN" "$INSTALL" --own-run-dir "$_bad" --service-user tester >"$OWNROOT/log-c" 2>&1; then
        fail "反跑 C：目标 $_bad 竟然被接受（红线：允许前缀校验失效）"
    else
        pass "反跑 C：目标 $_bad 被拒绝（$(sed -n '1p' "$OWNROOT/log-c" | cut -c1-70)）"
    fi
    if [ -s "$CLOG_C" ]; then
        fail "反跑 C：目标 $_bad 被拒绝前仍发了 chown（$(cat "$CLOG_C" | tr '\n' '|')）"
    else
        pass "反跑 C：目标 $_bad 一次 chown 都没发"
    fi
done
rm -rf "$OWNROOT"

# ---------------------------------------------------------------- 汇总
if [ "$FAILED" -eq 0 ]; then
    printf '\nPASS: envfile_test 全绿（%d 项判据，含 3 组反跑）\n' "$CHECKS"
    exit 0
fi
printf '\nFAIL: %d/%d 项判据未通过\n' "$FAILED" "$CHECKS" >&2
exit 1

