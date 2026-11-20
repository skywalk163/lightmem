#!/bin/sh
# platform_test.sh —— deploy/platform.sh 接口契约自测（POSIX sh，无需 root）
#
# 用法：sh deploy/platform_test.sh
#
# 说明：
#   * 安装/重启/轮转类接口全部以 LM_DRY_RUN=1 运行，只断言输出形状，不写系统；
#   * LM_FAKE_OS 用来在任意主机上覆盖 freebsd/linux 两个分支（Linux 上测 FreeBSD 语义，
#     FreeBSD 上测 Linux 语义），避免「只在本机 OS 上绿灯」的假覆盖；
#   * 唯一真实动作是 lm_probe_port 的只读探测：探一个必然没人监听的端口。
set -u

TEST_DIR=$(cd "$(dirname "$0")" && pwd)
. "$TEST_DIR/platform.sh"

PASS=0
FAIL=0

ok()    { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad()   { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
group() { printf '\n[%s]\n' "$*"; }

expect_rc() {  # <desc> <expected> <actual>
    if [ "$3" -eq "$2" ]; then
        ok "$1"
    else
        bad "$1（期望 rc=$2，实际 rc=$3）"
    fi
}

expect_nonzero() {  # <desc> <actual>
    if [ "$2" -ne 0 ]; then
        ok "$1"
    else
        bad "$1（期望非 0，实际 rc=0）"
    fi
}

expect_eq() {  # <desc> <expected> <actual>
    if [ "$3" = "$2" ]; then
        ok "$1"
    else
        bad "$1（期望 [$2]，实际 [$3]）"
    fi
}

expect_has() {  # <desc> <needle> <haystack>
    case "$3" in
        *"$2"*) ok "$1" ;;
        *) bad "$1（输出缺少 [$2]：[$3]）" ;;
    esac
}

expect_not_has() {  # <desc> <needle> <haystack>
    case "$3" in
        *"$2"*) bad "$1（输出不应包含 [$2]：[$3]）" ;;
        *) ok "$1" ;;
    esac
}

# 在子 shell 里设环境再调函数：互不污染，也保证 LM_FAKE_OS 只对本次调用生效。
out_fake() {  # <os> <fn> [args...]
    t_fake_os=$1
    shift
    ( LM_FAKE_OS=$t_fake_os; "$@" )
}

out_dry() {   # <os> <fn> [args...]
    t_dry_os=$1
    shift
    ( LM_FAKE_OS=$t_dry_os; LM_DRY_RUN=1; "$@" )
}

printf 'deploy/platform.sh 契约自测\n'

group "接口存在性（冻结名不得缺）"
for t_fn in lm_detect_os lm_prefix lm_service_restart lm_service_install lm_probe_port lm_logrotate_install; do
    if command -v "$t_fn" >/dev/null 2>&1; then
        ok "$t_fn"
    else
        bad "缺少接口 $t_fn"
    fi
done

group "lm_detect_os"
t_kernel=$(uname -s 2>/dev/null)
case "$t_kernel" in
    MINGW*|MSYS*|CYGWIN*)
        printf '  skip 真实系统断言：POSIX 垫片环境 %s（契约只认 FreeBSD/Linux/Darwin）\n' "$t_kernel"
        ;;
    *)
        t_out=$(lm_detect_os)
        t_rc=$?
        case "$t_out" in
            freebsd|linux|darwin) ok "真实系统识别为 $t_out（rc=$t_rc）" ;;
            *) bad "真实系统识别失败：rc=$t_rc out=[$t_out]" ;;
        esac
        ;;
esac

for t_os in freebsd linux darwin; do
    t_out=$(out_fake "$t_os" lm_detect_os)
    expect_eq "LM_FAKE_OS=$t_os -> $t_os" "$t_os" "$t_out"
done

t_out=$(out_fake plan9 lm_detect_os 2>/dev/null)
t_rc=$?
expect_rc "LM_FAKE_OS=plan9 拒绝（rc=2）" 2 "$t_rc"
expect_eq "非法 LM_FAKE_OS 不输出 OS 名" "" "$t_out"

group "lm_prefix"
expect_eq "linux -> /opt/lightmem" "/opt/lightmem" "$(out_fake linux lm_prefix)"
expect_eq "freebsd -> /usr/local/lightmem" "/usr/local/lightmem" "$(out_fake freebsd lm_prefix)"
expect_eq "dry-run 不影响 lm_prefix" "/opt/lightmem" "$(out_dry linux lm_prefix)"

group "lm_service_restart"
t_out=$(out_dry linux lm_service_restart lightmem)
expect_has "linux dry-run 走 systemctl" "systemctl restart lightmem" "$t_out"
t_out=$(out_dry freebsd lm_service_restart lightmem)
expect_has "freebsd dry-run 走 service" "service lightmem restart" "$t_out"
t_out=$(out_dry linux lm_service_restart 2>/dev/null)
t_rc=$?
expect_rc "缺参数拒绝（rc=2）" 2 "$t_rc"
t_out=$(out_dry darwin lm_service_restart lightmem 2>/dev/null)
t_rc=$?
expect_rc "darwin 明确拒绝（rc=3）" 3 "$t_rc"

group "lm_service_install"
t_out=$(out_dry linux lm_service_install /tmp/lm-unit lightmem)
expect_has "linux unit 目标 /etc/systemd/system/lightmem.service" \
    "/etc/systemd/system/lightmem.service" "$t_out"
expect_has "linux unit 权限 644" "install -m 644" "$t_out"
expect_has "linux 装完 daemon-reload" "systemctl daemon-reload" "$t_out"
t_out=$(out_dry freebsd lm_service_install /tmp/lm-rc lightmem)
expect_has "freebsd rc.d 目标 /usr/local/etc/rc.d/lightmem" \
    "/usr/local/etc/rc.d/lightmem" "$t_out"
expect_has "freebsd rc.d 权限 555" "install -m 555" "$t_out"
# T-0417：install.sh 传的是带后缀的完整 unit 名，不得再补一次 .service
t_out=$(out_dry linux lm_service_install /tmp/lm-unit lightmem.service)
expect_has "带 .service 后缀不再重复拼接" \
    "/etc/systemd/system/lightmem.service" "$t_out"
expect_not_has "带 .service 后缀不产生 .service.service" \
    ".service.service" "$t_out"
t_out=$(out_dry linux lm_service_install 2>/dev/null)
t_rc=$?
expect_rc "缺参数拒绝（rc=2）" 2 "$t_rc"

group "lm_probe_port"
t_out=$(out_dry linux lm_probe_port 7443)
expect_has "linux dry-run 打印端口 7443" "7443" "$t_out"
t_out=$(out_dry freebsd lm_probe_port 18443)
expect_has "freebsd dry-run 打印端口 18443" "18443" "$t_out"
t_out=$(out_dry linux lm_probe_port 4x43 2>/dev/null)
t_rc=$?
expect_rc "非数字端口拒绝（rc=2）" 2 "$t_rc"
lm_probe_port 1 >/dev/null 2>&1
t_rc=$?
expect_nonzero "真实探测关闭端口 1 返回非 0" "$t_rc"

group "lm_logrotate_install"
t_out=$(out_dry linux lm_logrotate_install)
expect_has "linux 目标 /etc/logrotate.d/lightmem" "/etc/logrotate.d/lightmem" "$t_out"
t_out=$(out_dry freebsd lm_logrotate_install)
expect_has "freebsd 目标 /usr/local/etc/newsyslog.conf.d/lightmem.conf" \
    "/usr/local/etc/newsyslog.conf.d/lightmem.conf" "$t_out"
expect_has "freebsd 默认源 deploy/newsyslog.d/lightmem.conf" \
    "newsyslog.d/lightmem.conf" "$t_out"
t_out=$(
    LM_FAKE_OS=freebsd
    LM_DRY_RUN=1
    LM_DEPLOY_DIR=/srv/lmdeploy
    lm_logrotate_install
)
expect_has "LM_DEPLOY_DIR 可覆盖源目录" "/srv/lmdeploy/newsyslog.d/lightmem.conf" "$t_out"
t_out=$(out_dry linux lm_logrotate_install /tmp/custom-logrotate)
expect_has "linux 可指定源文件" "/tmp/custom-logrotate" "$t_out"
t_out=$(out_dry darwin lm_logrotate_install 2>/dev/null)
t_rc=$?
expect_rc "darwin 明确拒绝（rc=3）" 3 "$t_rc"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
exit 0
