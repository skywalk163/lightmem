#!/bin/sh
# R16-S7：ZFS 快照 + zfs send 容灾链路脚本。
# 真源：manuals/p1-freebsd-zfs-setup.md §10「ZFS 快照与容灾实操」。
#
# 用途：一条命令完成「打快照 → 按保留策略清理 → zfs send 增量复制到目标池」，
#       交付 P1 存储底座的可恢复性（快照可回滚、可异地复制）。
#       纯 POSIX sh + zfs/ssh，无第三方依赖，可由 /etc/crontab 或 CI 调用。
#
# 用法：
#   sh scripts/zfs_backup.sh snapshot [-p POOL/DS] [-r] [-t TYPE]
#   sh scripts/zfs_backup.sh prune    [-p POOL/DS] [-r] [-t TYPE] [-k N]
#   sh scripts/zfs_backup.sh send     [-p POOL/DS] [-r] [-t TYPE] -d DEST
#   sh scripts/zfs_backup.sh all      [-p POOL/DS] [-r] [-t TYPE] [-k N] -d DEST
#   sh scripts/zfs_backup.sh list     [-p POOL/DS] [-t TYPE]
#   sh scripts/zfs_backup.sh rollback -s DS@SNAP
#
# 选项：
#   -p POOL/DS  源数据集根（默认 tank/lims）
#   -r          递归作用于根下所有子数据集
#   -t TYPE     快照类型标签：hourly（默认）/ daily / weekly
#   -k N        保留最近 N 个同类型快照（默认 hourly=48 / daily=7 / weekly=4）
#   -d DEST     zfs send 目标：host:pool/ds（远程 ssh）或 pool/ds（本地第二池）
#   -s SNAP     回滚目标快照全名（ds@snap，仅 rollback 用）
#   -n          dry-run：只打印将执行的命令，不落盘（评审/演练用）
#
# 退出码：0 成功；1 运行期失败；2 参数错误；3 环境缺失（无 zfs 命令）。
#
# 设计要点：
#   1. 只增不改：snapshot/send 只创建快照、只在目标池 receive；prune 只删
#      **本脚本命名规范内**的快照（@<TYPE>-*），人工/其他工具的快照一律不碰；
#      rollback 是显式的人工恢复动作，不进入 all 自动链路。
#   2. 幂等：同一时间戳快照已存在则跳过，重复执行不报错。
#   3. 增量发送：源与目标存在共同快照时用 `zfs send -i <common>` 发增量，
#      否则退回全量；目标已是最新则跳过。
#   4. 确定性：快照名 = <ds>@<TYPE>-<YYYYMMDDHHMMSS>，可 grep、可按名排序。
#   5. 递归：send 时按数据集层级一一映射到目标池（tank/lims/pg -> <DEST>/pg）。

set -e

usage() {
	cat <<'EOF'
zfs_backup.sh — ZFS 快照 + zfs send 容灾链路

用法：
  sh scripts/zfs_backup.sh snapshot [-p POOL/DS] [-r] [-t TYPE]
  sh scripts/zfs_backup.sh prune    [-p POOL/DS] [-r] [-t TYPE] [-k N]
  sh scripts/zfs_backup.sh send     [-p POOL/DS] [-r] [-t TYPE] -d DEST
  sh scripts/zfs_backup.sh all      [-p POOL/DS] [-r] [-t TYPE] [-k N] -d DEST
  sh scripts/zfs_backup.sh list     [-p POOL/DS] [-t TYPE]
  sh scripts/zfs_backup.sh rollback -s DS@SNAP

选项：
  -p POOL/DS  源数据集根（默认 tank/lims）
  -r          递归作用于根下所有子数据集
  -t TYPE     快照类型：hourly（默认）| daily | weekly
  -k N        保留最近 N 个同类型快照（默认 hourly=48 / daily=7 / weekly=4）
  -d DEST     send 目标：host:pool/ds（远程 ssh）或 pool/ds（本地）
  -s SNAP     回滚目标快照全名（ds@snap）
  -n          dry-run：只打印将执行的命令，不落盘
EOF
}

SRC="tank/lims"
RECURSE=0
TYPE="hourly"
KEEP=""
DEST=""
SNAP=""
DRY=0

default_keep() {
	case "$1" in
	hourly) echo 48 ;;
	daily) echo 7 ;;
	weekly) echo 4 ;;
	*) echo 7 ;;
	esac
}

# datasets：列出源数据集（-r 时含全部子数据集）。
# 数据集名不含空白，故按空白分词遍历是安全的（POSIX sh 无数组）。
datasets() {
	if [ "$RECURSE" -eq 1 ]; then
		zfs list -H -o name -r "$SRC"
	else
		zfs list -H -o name "$SRC"
	fi
}

# latest_snap：取某数据集下本类型最新快照全名（无则空）。
latest_snap() {
	zfs list -t snapshot -H -o name -s creation "$1" 2>/dev/null \
		| grep -F "@${TYPE}-" | tail -n 1 || true
}

# run_dry：dry-run 时只打印；否则执行。
run_dry() {
	if [ "$DRY" -eq 1 ]; then
		printf 'DRY: %s\n' "$*"
	else
		"$@"
	fi
}

do_snapshot() {
	STAMP="$(date -u +%Y%m%d%H%M%S)"
	for ds in $(datasets); do
		snap="${ds}@${TYPE}-${STAMP}"
		if zfs list -t snapshot -H -o name "$snap" >/dev/null 2>&1; then
			echo "SKIP   exists  $snap"
			continue
		fi
		run_dry zfs snapshot "$snap"
		echo "SNAP   $snap"
	done
}

do_prune() {
	if [ -z "$KEEP" ]; then
		KEEP="$(default_keep "$TYPE")"
	fi
	for ds in $(datasets); do
		snaps="$(zfs list -t snapshot -H -o name -s creation "$ds" 2>/dev/null \
			| grep -F "@${TYPE}-" || true)"
		if [ -z "$snaps" ]; then
			continue
		fi
		total="$(printf '%s\n' "$snaps" | wc -l | tr -d ' ')"
		if [ "$total" -le "$KEEP" ]; then
			echo "KEEP   $ds  $total/$KEEP"
			continue
		fi
		drop=$((total - KEEP))
		i=0
		for s in $snaps; do
			i=$((i + 1))
			if [ "$i" -gt "$drop" ]; then
				break
			fi
			run_dry zfs destroy "$s"
			echo "DESTROY $s"
		done
	done
}

# run_send_stream：执行（或 dry-run 打印）一次 zfs send | [ssh] zfs receive。
run_send_stream() {
	snap_new="$1"
	inc_from="$2"
	host="$3"
	dds="$4"
	if [ -n "$inc_from" ]; then
		send_expr="zfs send -i $inc_from $snap_new"
	else
		send_expr="zfs send $snap_new"
	fi
	if [ "$DRY" -eq 1 ]; then
		if [ -n "$host" ]; then
			printf 'DRY: %s | ssh %s zfs receive -F -u %s\n' "$send_expr" "$host" "$dds"
		else
			printf 'DRY: %s | zfs receive -F -u %s\n' "$send_expr" "$dds"
		fi
		return 0
	fi
	if [ -n "$host" ]; then
		# shellcheck disable=SC2029
		$send_expr | ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "zfs receive -F -u '$dds'"
	else
		$send_expr | zfs receive -F -u "$dds"
	fi
}

do_send() {
	if [ -z "$DEST" ]; then
		echo "ERR: send 需要 -d DEST（host:pool/ds 或 pool/ds）" >&2
		return 2
	fi
	case "$DEST" in
	*:*) DEST_HOST="${DEST%%:*}"
		DEST_ROOT="${DEST#*:}"
		ZFS_REMOTE="ssh -o BatchMode=yes -o ConnectTimeout=10 $DEST_HOST zfs"
		;;
	*) DEST_HOST=""
		DEST_ROOT="$DEST"
		ZFS_REMOTE="zfs"
		;;
	esac
	for ds in $(datasets); do
		suffix="${ds#"$SRC"}"
		dds="${DEST_ROOT}${suffix}"
		new_snap="$(latest_snap "$ds")"
		if [ -z "$new_snap" ]; then
			echo "WARN   $ds 无 @${TYPE}-* 快照，请先执行 snapshot" >&2
			continue
		fi
		new_tag="${new_snap##*@}"
		if ! $ZFS_REMOTE list -H -o name "$dds" >/dev/null 2>&1; then
			echo "FULL   new-target  $new_snap -> $dds"
			run_send_stream "$new_snap" "" "$DEST_HOST" "$dds"
			continue
		fi
		old_snap="$($ZFS_REMOTE list -t snapshot -H -o name "$dds" 2>/dev/null \
			| grep -F "@${TYPE}-" | sort | tail -n 1 || true)"
		old_tag="${old_snap##*@}"
		if [ -n "$old_snap" ] && [ "$old_tag" = "$new_tag" ]; then
			echo "SKIP   up-to-date $dds @ $new_tag"
			continue
		fi
		src_common="${ds}@${old_tag}"
		if [ -n "$old_snap" ] && zfs list -t snapshot -H -o name "$src_common" >/dev/null 2>&1; then
			echo "INC    $old_tag -> $new_tag  $dds"
			run_send_stream "$new_snap" "$src_common" "$DEST_HOST" "$dds"
		else
			echo "FULL   no-common  $new_snap -> $dds"
			run_send_stream "$new_snap" "" "$DEST_HOST" "$dds"
		fi
	done
}

do_list() {
	for ds in $(datasets); do
		echo "== $ds =="
		zfs list -t snapshot -H -o name,creation,used -s creation "$ds" 2>/dev/null \
			| grep -F "@${TYPE}-" || true
	done
}

do_rollback() {
	if [ -z "$SNAP" ]; then
		echo "ERR: rollback 需要 -s DS@SNAP" >&2
		return 2
	fi
	echo "WARN: rollback 会丢弃该快照之后的源数据，属人工恢复动作" >&2
	run_dry zfs rollback -r "$SNAP"
	echo "ROLLBACK $SNAP"
}

main() {
	ACTION="${1:-}"
	if [ -z "$ACTION" ]; then
		usage
		exit 2
	fi
	if [ $# -gt 0 ]; then
		shift
	fi

	while [ $# -gt 0 ]; do
		case "$1" in
		-p) SRC="$2"; shift 2 ;;
		-r) RECURSE=1; shift ;;
		-t) TYPE="$2"; shift 2 ;;
		-k) KEEP="$2"; shift 2 ;;
		-d) DEST="$2"; shift 2 ;;
		-s) SNAP="$2"; shift 2 ;;
		-n) DRY=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*)
			echo "未知参数：$1" >&2
			usage
			exit 2
			;;
		esac
	done

	case "$TYPE" in
	hourly | daily | weekly) ;;
	*)
		echo "ERR: -t 必须是 hourly|daily|weekly，实际为 '$TYPE'" >&2
		exit 2
		;;
	esac

	command -v zfs >/dev/null 2>&1 || {
		echo "ERR: 未找到 zfs 命令（本脚本须在 FreeBSD+ZFS 主机运行）" >&2
		exit 3
	}

	case "$ACTION" in
	snapshot) do_snapshot ;;
	prune) do_prune ;;
	send) do_send ;;
	all)
		do_snapshot
		do_prune
		do_send
		;;
	list) do_list ;;
	rollback) do_rollback ;;
	-h | --help) usage ;;
	*)
		echo "未知子命令：$ACTION" >&2
		usage
		exit 2
		;;
	esac
}

main "$@"