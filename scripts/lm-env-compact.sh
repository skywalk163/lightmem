#!/bin/sh
# lm-env-compact.sh —— 把 agent-tokens.json 压成**单行** JSON 写进 systemd EnvironmentFile
#
# 为什么需要（T-0417 真机验收实测）：
#   integrations/quickstart/install.sh 的 --systemd 分支用
#       printf 'LIMS_AGENT_TOKENS=%s\n' "$(cat agent-tokens.json)" > lightmem.env
#   写凭据；而 issue_token.py 产出的 agent-tokens.json 是**多行美化 JSON**（约 40 行）。
#   EnvironmentFile 是按行解析的，多行值只保留第一行 `{`，kernel 启动即报
#       [boot] auth config: LIMS_AGENT_TOKENS: parse agent tokens json: unexpected end of JSON input
#   然后 status=1/FAILURE 起不来（systemd 侧还会连着重启 5 次触发 StartLimitBurst）。
#
# 本脚本做**等价**操作：读同一份 agent-tokens.json，用 json.dumps(separators) 压成一行，
# 覆盖 lightmem.env（600，root/运行者均只读）。不改 token 内容、不改 unit。
#
# 用法（root）：
#   sudo sh scripts/lm-env-compact.sh /home/workbuddy/lightmem/_lightmem_run
#   sudo systemctl restart lightmem.service lightmem-mcp.service
#
# 幂等：可重复执行；输入缺失时返回 1 且不动现有 env。
set -eu

RUN_DIR=${1:-}
if [ -z "$RUN_DIR" ]; then
    echo "用法: sh scripts/lm-env-compact.sh <RUN_DIR>（如 <repo>/_lightmem_run）" >&2
    exit 2
fi
TOKENS="$RUN_DIR/agent-tokens.json"
ENVF="$RUN_DIR/lightmem.env"
if [ ! -f "$TOKENS" ]; then
    echo "缺少 token 文件：$TOKENS" >&2
    exit 1
fi

umask 077
python3 - "$TOKENS" "$ENVF" <<'PY'
import json, os, sys

tokens_path, env_path = sys.argv[1], sys.argv[2]
with open(tokens_path, encoding='utf-8') as f:
    data = json.load(f)
line = 'LIMS_AGENT_TOKENS=' + json.dumps(data, ensure_ascii=False, separators=(',', ':'))
with open(env_path, 'w', encoding='utf-8') as f:
    f.write(line + '\n')
os.chmod(env_path, 0o600)
print('wrote %s (%d bytes, %d line)' % (env_path, len(line) + 1, open(env_path, encoding='utf-8').read().count('\n')))
PY

# 自检：必须恰好一行、且不带换行残留
LINES=$(wc -l < "$ENVF")
if [ "$LINES" -ne 1 ]; then
    echo "自检失败：$ENVF 行数=$LINES（应为 1）" >&2
    exit 1
fi
echo "OK: $ENVF 单行、600"
