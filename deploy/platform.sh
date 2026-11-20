#!/bin/sh
# platform.sh —— lightmem 平台抽象层（POSIX sh，可被 `.` 引入）
#
# 定位：把「OS 判断 / 安装前缀 / 服务管理 / 端口探测 / 日志轮转」收敛到一处，
#       供 deploy/ 与 integrations/ 下的脚本复用，避免各线各写一套 uname/路径判断。
#
# 接口契约（T-0410 已冻结；接口名与语义不得随意改，后续线只复用）：
#   lm_detect_os                      -> 打印 freebsd | linux | darwin
#   lm_prefix                        -> 打印安装前缀（linux: /opt/lightmem；freebsd: /usr/local/lightmem）
#   lm_service_restart <name>        -> linux: systemctl restart <name>；freebsd: service <name> restart
#   lm_service_install <src> <name>  -> linux: 装 systemd unit；freebsd: 装 rc.d 脚本
#   lm_probe_port <port>             -> 端口是否在监听（ss -> sockstat -> lsof）
#   lm_logrotate_install [src]       -> linux: /etc/logrotate.d/lightmem；freebsd: newsyslog.conf.d
#
# 环境变量：
#   LM_DRY_RUN=1         只打印将要执行的命令，不改动系统（默认 0）
#   LM_FAKE_OS=<os>      测试用：强制走某个 OS 分支（freebsd/linux/darwin）
#   LM_DEPLOY_DIR=<dir>  deploy/ 目录（默认 deploy；newsyslog 源文件按它定位）
#
# 约束：
#   * 只使用 POSIX sh 语法（`sh -n deploy/platform.sh` 必须通过），不依赖 bash；
#   * 被 `.` 引入时不得有任何副作用（只定义函数与默认值）；
#   * 不改 rc.d 既有脚本行为，也不碰 kernel 代码。
#
# 用法：
#   . "$(dirname "$0")/platform.sh"
#   os=$(lm_detect_os) || exit 1
#   lm_service_restart lightmem

: "${LM_DRY_RUN:=0}"
: "${LM_FAKE_OS:=}"
: "${LM_DEPLOY_DIR:=deploy}"

lm__say() { printf '%s\n' "$*"; }
lm__err() { printf '%s\n' "$*" >&2; }
lm__dry() { printf '[dry-run] %s\n' "$*"; }

# 统一的 dry-run 判定：1/true/yes/on 视为「只打印」。
lm__is_dry() {
    case "${LM_DRY_RUN:-0}" in
        1|true|yes|on) return 0 ;;
        *) return 1 ;;
    esac
}

# 写系统路径的操作要求 root；dry-run 不需要。
lm__need_root() {
    if [ "$(id -u 2>/dev/null)" != "0" ]; then
        lm__err "$1: 需要 root（或设 LM_DRY_RUN=1 只打印计划）"
        return 1
    fi
    return 0
}

# lm_detect_os —— 打印 freebsd / linux / darwin。
# 未知系统返回 1；LM_FAKE_OS 非法值返回 2（测试钩子只认这三家）。
lm_detect_os() {
    if [ -n "${LM_FAKE_OS:-}" ]; then
        case "$LM_FAKE_OS" in
            freebsd|linux|darwin)
                lm__say "$LM_FAKE_OS"
                return 0
                ;;
            *)
                lm__err "lm_detect_os: LM_FAKE_OS=$LM_FAKE_OS 不支持（只认 freebsd/linux/darwin）"
                return 2
                ;;
        esac
    fi
    lm_dos_name=$(uname -s 2>/dev/null) || {
        lm__err "lm_detect_os: uname -s 失败"
        return 1
    }
    case "$lm_dos_name" in
        FreeBSD*) lm__say freebsd ;;
        Linux*)   lm__say linux ;;
        Darwin*)  lm__say darwin ;;
        *)
            lm__err "lm_detect_os: 未知系统 '$lm_dos_name'（只支持 FreeBSD/Linux/Darwin）"
            return 1
            ;;
    esac
}

# lm_prefix —— 打印安装前缀。
lm_prefix() {
    lm_pfx_os=$(lm_detect_os) || return $?
    case "$lm_pfx_os" in
        linux)   lm__say /opt/lightmem ;;
        freebsd) lm__say /usr/local/lightmem ;;
        darwin)  lm__say /usr/local/lightmem ;;
    esac
}

# lm_service_restart <name> —— 重启服务。
lm_service_restart() {
    if [ $# -lt 1 ] || [ -z "$1" ]; then
        lm__err "用法: lm_service_restart <service-name>"
        return 2
    fi
    lm_sr_name=$1
    lm_sr_os=$(lm_detect_os) || return $?
    case "$lm_sr_os" in
        linux)   lm_sr_cmd="systemctl restart $lm_sr_name" ;;
        freebsd) lm_sr_cmd="service $lm_sr_name restart" ;;
        darwin)
            lm__err "lm_service_restart: darwin 未冻结支持（当前只承诺 linux/freebsd）"
            return 3
            ;;
    esac
    if lm__is_dry; then
        lm__dry "$lm_sr_cmd"
        return 0
    fi
    if [ "$lm_sr_os" = "linux" ]; then
        systemctl restart "$lm_sr_name"
    else
        service "$lm_sr_name" restart
    fi
}

# lm_service_install <src> <name> —— 安装服务定义。
#   linux  : <src> -> /etc/systemd/system/<name>.service（644），随后 daemon-reload
#   freebsd: <src> -> /usr/local/etc/rc.d/<name>（555）
#
# <name> 兼容两种写法：带后缀（lightmem.service）与不带（lightmem）。
# 调用方 integrations/quickstart/install.sh 传的是带后缀的完整 unit 名，
# 早期实现无条件补 .service 会生成 lightmem.service.service（T-0417 真机验收实测），
# 故此处按后缀归一，不改变接口签名与其它分支行为。
lm_service_install() {
    if [ $# -lt 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
        lm__err "用法: lm_service_install <src-file> <service-name>"
        return 2
    fi
    lm_si_src=$1
    lm_si_name=$2
    lm_si_os=$(lm_detect_os) || return $?
    case "$lm_si_os" in
        linux)
            case "$lm_si_name" in
                *.service) lm_si_dest="/etc/systemd/system/${lm_si_name}" ;;
                *)         lm_si_dest="/etc/systemd/system/${lm_si_name}.service" ;;
            esac
            lm_si_mode=644
            ;;
        freebsd)
            lm_si_dest="/usr/local/etc/rc.d/${lm_si_name}"
            lm_si_mode=555
            ;;
        darwin)
            lm__err "lm_service_install: darwin 未冻结支持（当前只承诺 linux/freebsd）"
            return 3
            ;;
    esac
    if lm__is_dry; then
        lm__dry "install -m $lm_si_mode $lm_si_src $lm_si_dest"
        if [ "$lm_si_os" = "linux" ]; then
            lm__dry "systemctl daemon-reload"
        fi
        return 0
    fi
    if [ ! -f "$lm_si_src" ]; then
        lm__err "lm_service_install: 源文件不存在：$lm_si_src"
        return 1
    fi
    lm__need_root lm_service_install || return 1
    install -m "$lm_si_mode" "$lm_si_src" "$lm_si_dest" || return 1
    if [ "$lm_si_os" = "linux" ]; then
        systemctl daemon-reload || return 1
    fi
    return 0
}

# lm_probe_port <port> —— 端口是否在监听。
#   返回 0 = 在监听（并打印命中的那一行）；1 = 未监听；2 = 参数/环境错误。
lm_probe_port() {
    if [ $# -lt 1 ] || [ -z "$1" ]; then
        lm__err "用法: lm_probe_port <port>"
        return 2
    fi
    lm_pp_port=$1
    case "$lm_pp_port" in
        *[!0-9]*)
            lm__err "lm_probe_port: 端口必须是纯数字：$lm_pp_port"
            return 2
            ;;
    esac

    # dry-run 必须在选工具之前：CI 容器里可能三样都没有，
    # 但「只打印计划」不应该因此变成错误。
    if lm__is_dry; then
        lm__dry "探测端口 $lm_pp_port"
        return 0
    fi

    if command -v ss >/dev/null 2>&1; then
        lm_pp_tool=ss
    elif command -v sockstat >/dev/null 2>&1; then
        lm_pp_tool=sockstat
    elif command -v lsof >/dev/null 2>&1; then
        lm_pp_tool=lsof
    else
        lm__err "lm_probe_port: 没有 ss / sockstat / lsof 可用"
        return 2
    fi

    case "$lm_pp_tool" in
        ss)       lm_pp_out=$(ss -ltn 2>/dev/null) ;;
        sockstat) lm_pp_out=$(sockstat -4l 2>/dev/null) ;;
        lsof)     lm_pp_out=$(lsof -nP -iTCP:"$lm_pp_port" -sTCP:LISTEN 2>/dev/null) ;;
    esac

    if [ "$lm_pp_tool" = "lsof" ]; then
        if [ -n "$lm_pp_out" ]; then
            lm__say "$lm_pp_out"
            return 0
        fi
        return 1
    fi

    lm_pp_hit=0
    while IFS= read -r lm_pp_line; do
        case "$lm_pp_line" in
            *":$lm_pp_port "*|*":$lm_pp_port")
                lm__say "$lm_pp_line"
                lm_pp_hit=1
                ;;
        esac
    done <<LM_PROBE_EOF
$lm_pp_out
LM_PROBE_EOF
    if [ "$lm_pp_hit" = "1" ]; then
        return 0
    fi
    return 1
}

# lm_logrotate_install [src] —— 安装日志轮转配置。
#   linux  : /etc/logrotate.d/lightmem（无 src 时写入内置 daily/rotate 7/compress 规则）
#   freebsd: /usr/local/etc/newsyslog.conf.d/lightmem.conf
#            （无 src 时取 ${LM_DEPLOY_DIR}/newsyslog.d/lightmem.conf）
lm_logrotate_install() {
    lm_lr_src=${1:-}
    lm_lr_os=$(lm_detect_os) || return $?
    case "$lm_lr_os" in
        linux)
            lm_lr_dest=/etc/logrotate.d/lightmem
            ;;
        freebsd)
            lm_lr_dest=/usr/local/etc/newsyslog.conf.d/lightmem.conf
            if [ -z "$lm_lr_src" ]; then
                lm_lr_src="${LM_DEPLOY_DIR}/newsyslog.d/lightmem.conf"
            fi
            ;;
        darwin)
            lm__err "lm_logrotate_install: darwin 未冻结支持（当前只承诺 linux/freebsd）"
            return 3
            ;;
    esac

    if lm__is_dry; then
        if [ -n "$lm_lr_src" ]; then
            lm__dry "install -m 644 $lm_lr_src $lm_lr_dest"
        else
            lm__dry "生成 $lm_lr_dest（内置 logrotate：daily / rotate 7 / compress）"
        fi
        return 0
    fi

    lm__need_root lm_logrotate_install || return 1
    if [ -n "$lm_lr_src" ]; then
        if [ ! -f "$lm_lr_src" ]; then
            lm__err "lm_logrotate_install: 源文件不存在：$lm_lr_src"
            return 1
        fi
        install -m 644 "$lm_lr_src" "$lm_lr_dest" || return 1
        return 0
    fi
    cat > "$lm_lr_dest" <<'LM_LOGROTATE_EOF'
# lightmem 日志轮转（deploy/platform.sh 生成；与 FreeBSD newsyslog 规则等价）
/var/log/lightmem.log /var/log/lightmem-mcp.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
LM_LOGROTATE_EOF
    chmod 644 "$lm_lr_dest" || return 1
    return 0
}
