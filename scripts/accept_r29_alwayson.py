#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""R29 永续运行（Always-On）独立验收 —— F 线把 R1–R25 逐条脚本化（任务书 §9）。

设计纪律（任务书 §9.1）：
  * **只读产品代码**：本脚本只启停「自己拉起的」进程与临时 PG 实例；
    不修改仓库任何文件，不改系统服务（`--mode service` 例外，且仅测试机，
    并自带「改前备份 + 收尾还原」）。
  * **不谎报**：每条判据要么 PASS（附原始证据），要么 FAIL（附失败现场），
    要么 SKIP（附原因 + 责任人）。缺前置一律 SKIP，绝不当通过。
  * **可复现**：固定端口、固定语料、固定时长；输出落 `_r29_accept.log`。

模式（一台机器跑一部分，最后合并进 `_R29_F线验收报告.md`）：
  --mode static   文档/红线（R21 + 红线复核），任何有 .git 的 checkout
  --mode solo     SQLite/Solo 档服务级判据（R8/R9/R10/R11/R12/R13/R14/R15/R16/R22/R25 + 单测）
  --mode pg       PG 真机判据（R6/R7/R8/R18/R19/R24），需 --pg-* 与 --pg-ctl
  --mode service  rc.d 守护自愈（R1/R2/R3/R23），需 sudo + 已安装 service
  --mode all      以上全部（缺前置者自动 SKIP）

典型调用（0.82，solo）：
  sudo -n true && python3 scripts/accept_r29_alwayson.py --mode solo \
      --repo /home/ai/r29f/lightmem --bin /tmp/r29f/bin/lightmem-kernel \
      --mcp-bin /tmp/r29f/bin/lightmem-mcp --baseline-bin /tmp/r29f/bin/lightmem-kernel-c203db8 \
      --log /home/ai/r29f/_r29_accept_solo.log

典型调用（0.88，pg）：
  python3 scripts/accept_r29_alwayson.py --mode pg \
      --pg-host 127.0.0.1 --pg-port 15432 --pg-db r29f --pg-user lightmem_app \
      --pg-password ... --pg-super-user workbuddy --pg-super-db postgres \
      --pg-ctl "/usr/local/bin/pg_ctl -D /tmp/r29f/pgdata" ...
"""

import argparse
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

RESULTS = []          # [{id, name, status, evidence}]
LOG_FH = None


# ----------------------------------------------------------------------------
# 基础设施
# ----------------------------------------------------------------------------

def log(msg):
    line = str(msg)
    print(line, flush=True)
    if LOG_FH:
        LOG_FH.write(line + "\n")
        LOG_FH.flush()


def sh(cmd, timeout=60, env=None, cwd=None, check=False):
    """执行 shell 命令，返回 (rc, combined_output)。"""
    full_env = dict(os.environ)
    if env:
        full_env.update({k: str(v) for k, v in env.items()})
    try:
        p = subprocess.run(cmd, shell=True, capture_output=True, text=True,
                           timeout=timeout, env=full_env, cwd=cwd)
        out = (p.stdout or "") + (p.stderr or "")
        rc = p.returncode
    except subprocess.TimeoutExpired as e:
        out = "TIMEOUT after %ss: %s" % (timeout, (e.stdout or "") + (e.stderr or ""))
        rc = 124
    out = out.strip()
    if check and rc != 0:
        raise RuntimeError("command failed rc=%d: %s\n%s" % (rc, cmd, out))
    return rc, out


def http(method, url, body=None, token=None, timeout=6):
    """极简 HTTP；返回 (status_code, text)。连接失败返回 (-1, err)。"""
    data = None
    headers = {"User-Agent": "r29-accept"}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        try:
            txt = e.read().decode("utf-8", "replace")
        except Exception:
            txt = ""
        return e.code, txt
    except Exception as e:  # 连接拒绝/超时
        return -1, str(e)


def jget(url, token=None, timeout=6):
    code, txt = http("GET", url, token=token, timeout=timeout)
    try:
        return code, json.loads(txt)
    except Exception:
        return code, {"_raw": txt[:400]}


def port_free(port, host="127.0.0.1"):
    s = socket.socket()
    try:
        s.bind((host, port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def wait_port(port, timeout=20.0, host="127.0.0.1"):
    end = time.time() + timeout
    while time.time() < end:
        s = socket.socket()
        s.settimeout(0.5)
        try:
            s.connect((host, port))
            return True
        except OSError:
            time.sleep(0.2)
        finally:
            s.close()
    return False


def wait_gone(port, timeout=20.0, host="127.0.0.1"):
    end = time.time() + timeout
    while time.time() < end:
        s = socket.socket()
        s.settimeout(0.3)
        try:
            s.connect((host, port))
            time.sleep(0.2)
        except OSError:
            return True
        finally:
            s.close()
    return False


class Proc(object):
    """自己拉起的子进程（记录日志路径，收尾可杀）。"""

    def __init__(self, popen, port, logpath, argv):
        self.p = popen
        self.port = port
        self.logpath = logpath
        self.argv = argv

    def alive(self):
        return self.p.poll() is None

    def logtext(self):
        try:
            with open(self.logpath, "r", encoding="utf-8", errors="replace") as f:
                return f.read()
        except OSError:
            return ""

    def stop(self, sig=signal.SIGTERM, timeout=25):
        if self.p.poll() is not None:
            return self.p.returncode
        try:
            os.kill(self.p.pid, sig)
        except OSError:
            pass
        end = time.time() + timeout
        while time.time() < end:
            if self.p.poll() is not None:
                return self.p.returncode
            time.sleep(0.2)
        try:
            os.kill(self.p.pid, signal.SIGKILL)
        except OSError:
            pass
        try:
            self.p.wait(timeout=5)
        except Exception:
            pass
        return -9

    def kill9(self):
        try:
            os.kill(self.p.pid, signal.SIGKILL)
        except OSError:
            pass
        try:
            self.p.wait(timeout=10)
        except Exception:
            pass


# ----------------------------------------------------------------------------
# 结果记录
# ----------------------------------------------------------------------------

def record(cid, name, status, evidence=""):
    RESULTS.append({"id": cid, "name": name, "status": status, "evidence": evidence})
    log("[%s] %-6s %s :: %s" % (cid, status, name, evidence.replace("\n", " | ")[:600]))


def guard(cid, name, fn, ctx):
    """跑一条判据；异常 → FAIL（不中断整轮）。"""
    log("")
    log("=" * 78)
    log("== %s %s" % (cid, name))
    log("=" * 78)
    try:
        fn(ctx, cid, name)
    except Exception as e:
        import traceback
        tb = traceback.format_exc()
        log(tb)
        record(cid, name, "FAIL", "脚本异常：%s" % e)


# ----------------------------------------------------------------------------
# profile / 进程装配
# ----------------------------------------------------------------------------

class Ctx(object):
    pass


def solo_profile(ctx, name, sqlite_path, extra_lines=""):
    """基于仓库 profiles/lims-solo.profile.yaml 造一个隔离副本（绝对路径）。"""
    src = os.path.join(ctx.repo, "profiles", "lims-solo.profile.yaml")
    with open(src, "r", encoding="utf-8") as f:
        txt = f.read()
    txt = txt.replace("path: ./data/lims.sqlite", "path: " + sqlite_path)
    if extra_lines:
        txt = txt + "\n" + extra_lines + "\n"
    dst = os.path.join(ctx.workdir, name + ".yaml")
    with open(dst, "w", encoding="utf-8") as f:
        f.write(txt)
    return dst


def smb_profile(ctx, name, drop_host=False):
    src = os.path.join(ctx.repo, "profiles", "lims-smb.profile.yaml")
    with open(src, "r", encoding="utf-8") as f:
        lines = f.readlines()
    out = []
    for ln in lines:
        m = re.match(r"^(\s+)host:\s*10\.0\.20\.10\s*$", ln)
        if drop_host and m:
            # B1 反证：pg.host 置空（保留 key、保留缩进，保证 YAML 仍合法）
            out.append('%shost: ""\n' % m.group(1))
            continue
        out.append(ln)
    dst = os.path.join(ctx.workdir, name + ".yaml")
    with open(dst, "w", encoding="utf-8") as f:
        f.writelines(out)
    return dst


def start_kernel(ctx, profile, port, logname, env_extra=None, data_dir=None,
                 extra_args=None, wait=25):
    env = {}
    if env_extra:
        env.update(env_extra)
    args = [ctx.bin, "-profile", profile, "-listen", "127.0.0.1:%d" % port]
    if data_dir:
        args += ["-data-dir", data_dir]
    if extra_args:
        args += list(extra_args)
    logpath = os.path.join(ctx.workdir, logname + ".log")
    logf = open(logpath, "w", encoding="utf-8")
    logf.write("# " + " ".join(args) + "\n")
    logf.flush()
    full_env = dict(os.environ)
    full_env.update({k: str(v) for k, v in env.items()})
    popen = subprocess.Popen(args, stdout=logf, stderr=subprocess.STDOUT,
                             env=full_env, cwd=ctx.workdir)
    pr = Proc(popen, port, logpath, args)
    end = time.time() + wait
    while time.time() < end:
        if popen.poll() is not None:
            return pr
        code, _ = http("GET", "http://127.0.0.1:%d/livez" % port, timeout=1)
        if code == 200:
            return pr
        time.sleep(0.2)
    return pr


def start_mcp(ctx, port, kernel_port, logname, env_extra=None):
    env = dict(env_extra or {})
    args = [ctx.mcp_bin, "-transport", "http", "-listen", "127.0.0.1:%d" % port,
            "-rest-base", "http://127.0.0.1:%d" % kernel_port]
    logpath = os.path.join(ctx.workdir, logname + ".log")
    logf = open(logpath, "w", encoding="utf-8")
    logf.write("# " + " ".join(args) + "\n")
    logf.flush()
    full_env = dict(os.environ)
    full_env.update({k: str(v) for k, v in env.items()})
    popen = subprocess.Popen(args, stdout=logf, stderr=subprocess.STDOUT,
                             env=full_env, cwd=ctx.workdir)
    pr = Proc(popen, port, logpath, args)
    wait_port(port, timeout=15)
    return pr


def memory_body(i, tenant="t1", user="u1", fact=None):
    mid = "r29f-%s-%03d" % (tenant, i)
    return {
        "memory_id": mid,
        "idempotency_key": mid,
        "tenant_id": tenant,
        "user_id": user,
        "visibility": "tenant",
        "memory_kind": "fact",
        "fact": fact or ("R29 验收语料 %03d：永续运行需要守护自愈与耐久自检。" % i),
        "confidence": 0.9,
        "source": "r29-f-accept",
        "importance": 0.5,
        "permission": {"owner": user},
    }


def write_memories(base, n, token=None, tenant="t1", user="u1"):
    ok = 0
    codes = {}
    for i in range(1, n + 1):
        code, _ = http("POST", base + "/memories", memory_body(i, tenant, user), token=token)
        codes[code] = codes.get(code, 0) + 1
        if code in (200, 201):
            ok += 1
    return ok, codes


# ----------------------------------------------------------------------------
# R1–R5 / R23：rc.d 守护自愈（0.82 真机，需 sudo）
# ----------------------------------------------------------------------------

def _svc(ctx, verb):
    # 注意（F-1 缺陷规避）：rc.d 的 lightmem_start 不 chdir 到 ${lightmem_chdir}，
    # 而 solo profile 的 sqlite 路径是相对路径（./data/lims.sqlite）。
    # 因此**必须在 chdir 目录下调用 service**，否则内核会把库解析到 /data
    # 并以 skywalk 身份打开失败（详见 _R29_F线验收报告.md 缺陷 F-1）。
    # 这是验收侧的规避，不是修复；修复责任在 A 线（rc.d 增加 cd）。
    rc, out = sh("sudo -n sh -c 'cd %s && service %s %s'"
                 % (ctx.svc_chdir, ctx.service, verb), timeout=120)
    log("   [svc] %s rc=%d :: %s" % (verb, rc, out.replace("\n", " / ")[:300]))
    return rc, out


def _svc_status(ctx):
    return sh("sudo -n sh -c 'cd %s && service %s status'" % (ctx.svc_chdir, ctx.service),
              timeout=40)


def _kernel_pids(ctx):
    # 用 -x 精确匹配进程名：`pgrep -f` 会把调用它的 shell 自身也算进来（假阳性）。
    rc, out = sh("pgrep -x %s" % ctx.service_bin_pat, timeout=10)
    pids = [x.strip() for x in out.split() if x.strip().isdigit()]
    return pids


def _svc_reset(ctx):
    """清理上一轮验收遗留的 daemon 监督者 / kernel 进程（否则 R1「不复活」会被
    历史遗留的第二个 supervisor 污染）。只清本服务二进制，不动同机其他实例。"""
    if not ctx.sudo_ok:
        return
    sh("sudo -n pkill -9 -f 'daemon: env' ; sudo -n pkill -9 -x %s ; "
       "sudo -n rm -f /var/run/lightmem/lightmem.pid" % ctx.service_bin_pat, timeout=30)
    time.sleep(2)
    log("   [reset] 已清理遗留 daemon/kernel 与 pidfile")


def r1_stop_no_resurrect(ctx, cid, name):
    if not ctx.sudo_ok:
        record(cid, name, "SKIP", "无 sudo -n 权限，无法操作 service（责任人：运维窗口）")
        return
    _svc_reset(ctx)
    _svc(ctx, "start")
    time.sleep(4)
    before = set(_kernel_pids(ctx))
    rc, out = _svc(ctx, "stop")
    time.sleep(20)
    rc2, st = _svc_status(ctx)
    after = set(_kernel_pids(ctx))
    new_pids = sorted(after - before)
    listening = sh("sockstat -4l | grep ':%d '" % ctx.svc_port, timeout=10)[1].strip()
    ok = ("NOT RUNNING" in st.upper()) and not new_pids and not listening
    record(cid, name, "PASS" if ok else "FAIL",
           "stop 前 pid=%s；stop 后 20s：status=`%s`、新增 pid=%s、端口监听=`%s`"
           "（判据：不复活 = 无新 pid 且端口无监听）"
           % (sorted(before), st.replace("\n", " / "), new_pids, listening))
    _svc(ctx, "start")
    time.sleep(3)


def r2_kill9_autorecover(ctx, cid, name):
    if not ctx.sudo_ok:
        record(cid, name, "SKIP", "无 sudo -n 权限（责任人：运维窗口）")
        return
    _svc(ctx, "start")
    time.sleep(4)
    before = _kernel_pids(ctx)
    if not before:
        record(cid, name, "FAIL", "服务未运行，无法 kill -9（先看 R1 的 start 是否成功）")
        return
    pid1 = before[0]
    sh("sudo -n kill -9 %s" % pid1, timeout=10)
    time.sleep(15)
    rc, st = _svc_status(ctx)
    after = _kernel_pids(ctx)
    pid2 = after[0] if after else ""
    code, body = jget("http://127.0.0.1:%d/readyz" % ctx.svc_port)
    body = json.dumps(body)
    ok = (pid2 and pid2 != pid1 and code == 200 and "RUNNING" in st.upper())
    record(cid, name, "PASS" if ok else "FAIL",
           "kill -9 pid=%s → 15s 后 pid=%s（新 pid=%s）status=`%s` /readyz=%d %s"
           % (pid1, pid2, bool(pid2 and pid2 != pid1), st.replace("\n", " / "), code, body[:120]))


def r3_single_listener(ctx, cid, name):
    rc, out = sh("sockstat -4l | grep ':%d '" % ctx.svc_port, timeout=10)
    lines = [l for l in out.splitlines() if l.strip()]
    ok = len(lines) == 1
    record(cid, name, "PASS" if ok else "FAIL",
           "sockstat -4l | grep :%d → %d 行：%s" % (ctx.svc_port, len(lines), " / ".join(lines)[:300]))


def r23_restart_bounded(ctx, cid, name):
    """R23：制造**不可自愈**故障，观察重启有界、间隔 ≥5s、不刷屏。

    故障注入：把 rc.conf 的 `lightmem_profile` 指向**语法非法**的 profile
    （rc.subr 会先检查文件可读性，所以必须「存在但解析失败」，不能指向不存在文件），
    内核每次启动即 `log.Fatalf("parse profile ...")` 退出 → daemon(8) 按 `-R 5`
    每 5s 拉起一次、`-C 24` 用尽后自行退出。

    **不用「占端口」注入**：本服务监听 tcp46 `*:18443`，与 `127.0.0.1:18443` 的
    占位进程在 FreeBSD 上可共存，注入无效（F 线实测，见报告 §反证实验）。

    观测口径：内核每次启动毫秒级退出，进程表采样会漏；以 **daemon `-o` 日志的
    时间戳**为主证据（每次拉起必写一行 parse profile 失败）。
    收尾：还原 rc.conf 并把服务拉回 RUNNING。
    """
    if not ctx.sudo_ok:
        record(cid, name, "SKIP", "无 sudo -n 权限（责任人：运维窗口）")
        return
    rcconf = "/etc/rc.conf"
    bak = "/tmp/r29f/rc.conf.r23.bak"
    badprof = "/tmp/r29f/bad.profile.yaml"
    sh("sudo -n cp -p %s %s" % (rcconf, bak), timeout=20)
    with open(badprof, "w", encoding="utf-8") as f:
        f.write("storage_backend: pgvector\nstorage://n  pg://n    host: [unclosed\n")
    os.chmod(badprof, 0o644)

    _svc_reset(ctx)
    _svc(ctx, "stop")
    time.sleep(2)
    sh("sudo -n sed -i '' 's#^lightmem_profile=.*#lightmem_profile=\"%s\"#' %s"
       % (badprof, rcconf), timeout=20)
    log("   [r23] rc.conf: %s" % sh("grep '^lightmem_profile' %s" % rcconf)[1].strip())

    start_off = os.path.getsize(ctx.svc_log) if os.path.exists(ctx.svc_log) else 0
    _svc(ctx, "start")           # 健康等待 20s 后返回非 0；daemon 已在后台按 -R 重试
    time.sleep(75)               # 观察窗：20s 启动等待 + 75s ≈ 19 次拉起机会

    # 日志是 root:wheel 0600（服务用户写入），必须经 sudo 读；按字节偏移取新增段。
    _rc, txt = sh("sudo -n tail -c +%d %s" % (start_off + 1, ctx.svc_log), timeout=30)
    if txt.count("parse profile") < 2:      # 日志若被 daemon 重开截断，回退读全文
        _rc, txt = sh("sudo -n cat %s" % ctx.svc_log, timeout=30)
    log("   [r23] 读日志 rc=%d len=%d hits=%d 首行=%s"
        % (_rc, len(txt), txt.count("parse profile"), txt.splitlines()[:1]))
    import datetime
    times = []
    for m in re.finditer(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) \[boot\] parse profile",
                         txt, re.M):
        try:
            times.append(datetime.datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S"))
        except ValueError:
            pass
    attempts = len(times)
    gaps = [(times[i + 1] - times[i]).total_seconds() for i in range(len(times) - 1)]
    bounded = attempts <= 21
    spaced = all(g >= 4.5 for g in gaps) if gaps else False
    ok = attempts >= 2 and spaced and bounded
    record(cid, name, "PASS" if ok else "FAIL",
           "故障注入=非法 profile（parse 必失败）；观察窗内拉起 %d 次；"
           "相邻拉起间隔(s)=%s（-R 5 → 应 ≥5s）；有界(<=21)=%s；日志行=%s"
           % (attempts, [round(g, 1) for g in gaps][:12], bounded,
              [t.strftime("%H:%M:%S") for t in times][:8]))
    # 收尾：还原 rc.conf 并拉回 RUNNING
    sh("sudo -n cp -p %s %s" % (bak, rcconf), timeout=20)
    _svc(ctx, "stop")
    time.sleep(2)
    _svc(ctx, "start")
    time.sleep(4)
    code, _ = http("GET", "http://127.0.0.1:%d/readyz" % ctx.svc_port)
    log("   [r23] 收尾还原后 /readyz=%d" % code)


# ----------------------------------------------------------------------------
# R9 / R4 / R10：鉴权与 MCP
# ----------------------------------------------------------------------------

def r9_probe_exempt(ctx, cid, name):
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r9",
                     env_extra={"LIMS_AGENT_TOKENS": ctx.tokens_json})
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        c1, b1 = http("GET", "http://127.0.0.1:%d/livez" % ctx.port)
        c2, b2 = http("GET", "http://127.0.0.1:%d/readyz" % ctx.port)
        c3, b3 = http("GET", "http://127.0.0.1:%d/healthz" % ctx.port)
        c4, b4 = http("GET", "http://127.0.0.1:%d/metrics" % ctx.port)
        # 业务端点必须仍需鉴权（否则豁免扩大 = 安全回退）
        c5, _ = http("GET", "http://127.0.0.1:%d/search?q=x&tenant_id=t1" % ctx.port)
        ok = (c1, c2, c3, c4, c5) == (200, 200, 200, 200, 401)
        record(cid, name, "PASS" if ok else "FAIL",
               "无 token：/livez=%d /readyz=%d /healthz=%d /metrics=%d /search=%d（期望 200/200/200/200/401）"
               % (c1, c2, c3, c4, c5))
    finally:
        p.stop()


def r4_auth_matrix(ctx, cid, name):
    k = start_kernel(ctx, ctx.solo_prof, ctx.port, "r4",
                     env_extra={"LIMS_AGENT_TOKENS": ctx.tokens_json})
    m = None
    try:
        if not k.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % k.logtext()[-400:])
            return
        m = start_mcp(ctx, ctx.mcp_port, ctx.port, "r4mcp",
                      env_extra={"LIMS_AGENT_TOKENS": ctx.tokens_json})
        a = http("GET", "http://127.0.0.1:%d/healthz" % ctx.port)[0]
        b = http("GET", "http://127.0.0.1:%d/search?q=x&tenant_id=t1" % ctx.port)[0]
        c = http("GET", "http://127.0.0.1:%d/search?q=x&tenant_id=t1" % ctx.port,
                 token=ctx.token)[0]
        d = http("POST", "http://127.0.0.1:%d/mcp" % ctx.mcp_port,
                 body={"jsonrpc": "2.0", "id": 1, "method": "tools/list"})[0]
        ok = (a, b, c, d) == (200, 401, 200, 401)
        record(cid, name, "PASS" if ok else "FAIL",
               "鉴权矩阵四项 /healthz=%d /search(无token)=%d /search(token)=%d MCP/mcp(无token)=%d"
               "（期望 200/401/200/401）" % (a, b, c, d))
    finally:
        if m:
            m.stop()
        k.stop()


def r10_mcp_readiness(ctx, cid, name):
    k = start_kernel(ctx, ctx.solo_prof, ctx.port, "r10")
    m = None
    try:
        if not k.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % k.logtext()[-400:])
            return
        m = start_mcp(ctx, ctx.mcp_port, ctx.port, "r10mcp")
        h_up = jget("http://127.0.0.1:%d/healthz" % ctx.mcp_port)
        r_up = http("GET", "http://127.0.0.1:%d/readyz" % ctx.mcp_port)[0]
        k.stop()
        time.sleep(2)
        h_down = jget("http://127.0.0.1:%d/healthz" % ctx.mcp_port)
        r_down = http("GET", "http://127.0.0.1:%d/readyz" % ctx.mcp_port)[0]
        up_status = (h_down[1] or {}).get("upstream_status")
        ok = (h_up[0] == 200 and r_up == 200 and h_down[0] == 200
              and r_down == 503 and up_status == "unreachable")
        record(cid, name, "PASS" if ok else "FAIL",
               "内核在：MCP /healthz=%d /readyz=%d；内核停：MCP /healthz=%d(upstream_status=%s) /readyz=%d"
               "（期望 200/200/200/unreachable/503）"
               % (h_up[0], r_up, h_down[0], up_status, r_down))
    finally:
        if m:
            m.stop()
        k.stop()


# ----------------------------------------------------------------------------
# R11 / R12：调度器（默认关自证 + 生命周期定时器真起）
# ----------------------------------------------------------------------------

def r11_lifecycle_timer(ctx, cid, name):
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r11",
                     env_extra={"LIMS_LIFECYCLE_INTERVAL": "60s"})
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        seen = []
        t0 = time.time()
        while time.time() - t0 < 190:
            code, body = jget("http://127.0.0.1:%d/healthz" % ctx.port)
            v = (body or {}).get("last_lifecycle_run", "")
            if v and (not seen or seen[-1] != v):
                seen.append(v)
            time.sleep(2)
        runs = len(seen)
        txt = p.logtext()
        job_done = len(re.findall(r"\[maint\] job lifecycle done", txt))
        ok = runs >= 2 and job_done >= 2
        record(cid, name, "PASS" if ok else "FAIL",
               "LIMS_LIFECYCLE_INTERVAL=60s / 190s：/healthz.last_lifecycle_run 变化 %d 次 %s；"
               "日志 `[maint] job lifecycle done` %d 次"
               % (runs, seen[:4], job_done))
    finally:
        p.stop()


_VOLATILE_KEY = re.compile(r"(^|_)(at|ts)$|^last_|_ms$|time|timestamp|latency", re.I)

# 设计内「纯加法」字段（任务书 §0.3 B3 已登记）：默认态下新增，不算行为漂移，
# 对拍时剔除；其余字段一律逐字节比对。
_ADDITIVE_KEYS = {"started_at", "version", "last_lifecycle_run"}


def _norm_walk(v):
    if isinstance(v, dict):
        return {k: ("<volatile>" if _VOLATILE_KEY.search(k) else _norm_walk(x))
                for k, x in v.items() if k not in _ADDITIVE_KEYS}
    if isinstance(v, list):
        return [_norm_walk(x) for x in v]
    return v


def _norm(obj):
    """把响应归一化为可逐字节比较的字符串：时间/耗时类字段统一置为 <volatile>。

    比对的是**结构 + 全部非时间字段**——默认关路径的语义等价证据。
    """
    if isinstance(obj, str):
        try:
            obj = json.loads(obj)
        except Exception:
            return obj
    return json.dumps(_norm_walk(obj), sort_keys=True, ensure_ascii=False)


def r12_default_off(ctx, cid, name):
    """默认关自证：不设任何新开关 → 无维护 goroutine/日志/字段；与基线二进制逐字节一致。"""
    ev = []
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r12")
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        code, body = jget("http://127.0.0.1:%d/healthz" % ctx.port)
        no_lc = "last_lifecycle_run" not in (body or {})
        no_schema = "schema_ok" not in (body or {})   # 未配 -data-dir → 自检关
        txt = p.logtext()
        no_maint = "[maint]" not in txt
        n_before, _ = write_memories("http://127.0.0.1:%d" % ctx.port, 3)
        s_code, s_txt = http("GET", "http://127.0.0.1:%d/search?q=R29&tenant_id=t1" % ctx.port)
        ev.append("默认态：healthz 无 last_lifecycle_run=%s、无 schema_ok=%s、日志无 [maint]=%s、写入 %d/3"
                  % (no_lc, no_schema, no_maint, n_before))
        base_equal = None
        if ctx.baseline_bin and os.path.exists(ctx.baseline_bin):
            # 用同一语料起基线二进制（c203db8），比对 /healthz 与 /search
            bport = ctx.port + 30
            prof_b = solo_profile(ctx, "r12_base", ctx.sqlite_b)
            logpath = os.path.join(ctx.workdir, "r12base.log")
            logf = open(logpath, "w", encoding="utf-8")
            popen = subprocess.Popen(
                [ctx.baseline_bin, "-profile", prof_b, "-listen", "127.0.0.1:%d" % bport],
                stdout=logf, stderr=subprocess.STDOUT, cwd=ctx.workdir,
                env=dict(os.environ))
            bp = Proc(popen, bport, logpath, [])
            try:
                wait_port(bport, timeout=25)
                # 同一语料写入基线，保证 /search 有可比对象
                nb, _ = write_memories("http://127.0.0.1:%d" % bport, 3)
                b_code, b_body = jget("http://127.0.0.1:%d/healthz" % bport)
                b_s_code, b_s_txt = http(
                    "GET", "http://127.0.0.1:%d/search?q=R29&tenant_id=t1" % bport)
                h_equal = _norm(body) == _norm(b_body)
                s_equal = s_code == b_s_code and _norm(s_txt) == _norm(b_s_txt)
                base_equal = h_equal and s_equal
                ev.append("与基线 %s 对拍（同语料各写 %d 条）：/healthz 相同=%s；"
                          "/search(%d vs %d) 相同=%s"
                          % (ctx.baseline_ref, nb, h_equal, s_code, b_s_code, s_equal))
            finally:
                bp.stop()
        else:
            ev.append("未提供 --baseline-bin，跳过与 %s 的逐字节对拍（责任人：验收环境）"
                      % ctx.baseline_ref)
        ok = no_lc and no_schema and no_maint and n_before == 3 and (base_equal is not False)
        record(cid, name, "PASS" if ok else "FAIL", "；".join(ev))
    finally:
        p.stop()


# ----------------------------------------------------------------------------
# R13：批量扫描压测
# ----------------------------------------------------------------------------

def r13_batch_p95(ctx, cid, name):
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r13",
                     env_extra={"LIMS_ADMIN_LIFECYCLE": "1",
                                "LIMS_MAINT_BATCH": "1",
                                "LIMS_MAINT_BATCH_SIZE": "200"})
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        n, codes = write_memories("http://127.0.0.1:%d" % ctx.port, 300)
        probe = os.path.join(ctx.repo, "scripts", "perf_probe.py")
        off = os.path.join(ctx.workdir, "p95_off.txt")
        on = os.path.join(ctx.workdir, "p95_on.txt")
        rc1, out1 = sh("%s %s --url http://127.0.0.1:%d --seconds 20 --concurrency 4 "
                       "--path '/search?q=R29&tenant_id=t1' --p95-out %s --label off"
                       % (sys.executable, probe, ctx.port, off), timeout=90)
        # 维护轮（lifecycle 引擎集）在压测期间跑
        import threading
        trig = {}

        def _trig():
            for _ in range(6):
                code, txt = http("POST",
                                 "http://127.0.0.1:%d/admin/lifecycle/run?engines=budget,archive&tenant_id=t1"
                                 % ctx.port, body={})
                trig[code] = trig.get(code, 0) + 1
                time.sleep(3)
        th = threading.Thread(target=_trig)
        th.start()
        rc2, out2 = sh("%s %s --url http://127.0.0.1:%d --seconds 20 --concurrency 4 "
                       "--path '/search?q=R29&tenant_id=t1' --p95-out %s --label on"
                       % (sys.executable, probe, ctx.port, on), timeout=90)
        th.join()
        try:
            p95_off = float(open(off).read().strip())
            p95_on = float(open(on).read().strip())
        except Exception:
            record(cid, name, "FAIL", "压测未产出 P95：%s / %s" % (out1[-200:], out2[-200:]))
            return
        locked = len(re.findall(r"database is locked", p.logtext()))
        ratio = (p95_on / p95_off) if p95_off else 99
        ok = ratio <= 1.20 and locked == 0
        record(cid, name, "PASS" if ok else "FAIL",
               "写入 %d 条%s；P95 off=%.2fms on=%.2fms 劣化=%.1f%%（阈值 ≤20%%）；"
               "`database is locked` 计数=%d；维护触发返回=%s"
               % (n, codes, p95_off, p95_on, (ratio - 1) * 100, locked, trig))
    finally:
        p.stop()


# ----------------------------------------------------------------------------
# R14 / R15：排水与未完成标记
# ----------------------------------------------------------------------------

def r14_sigterm_midflight(ctx, cid, name):
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r14",
                     env_extra={"LIMS_CONSOLIDATE_INTERVAL": "1s",
                                "LIMS_MAINTENANCE_INTERVAL": "1s"},
                     data_dir=ctx.datadir)
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        n, _ = write_memories("http://127.0.0.1:%d" % ctx.port, 300)
        time.sleep(2)
        rc = p.stop(signal.SIGTERM, timeout=40)
        txt = p.logtext()
        waiting = "[drain] waiting" in txt
        stages = re.findall(r"\[drain\] waiting (\S+)", txt)
        completed = re.findall(r"\[drain\] all (\d+) stages completed", txt)
        bad = re.findall(r"(upsert failed|sql: database is closed|use of closed)", txt)
        ok = waiting and len(stages) >= 2 and not bad
        record(cid, name, "PASS" if ok else "FAIL",
               "写入 %d 条后 SIGTERM（rc=%s）：排水阶段=%s；`all N stages completed`=%s；"
               "`upsert failed`/`database is closed` 命中=%d %s"
               % (n, rc, stages, completed, len(bad), bad[:3]))
    finally:
        p.stop()


def r15_marker_and_data(ctx, cid, name):
    """R14 之后：数据全生效或全未生效 + 未完成标记已清理。"""
    txt14 = ""
    for f in os.listdir(ctx.workdir):
        if f.startswith("r14") and f.endswith(".log"):
            txt14 += open(os.path.join(ctx.workdir, f), encoding="utf-8",
                          errors="replace").read()
    markers = []
    if os.path.isdir(ctx.datadir):
        markers = [f for f in os.listdir(ctx.datadir) if f.endswith(".drain.unfinished")]
    # 重启读条数（走 /healthz rows 需要 -data-dir）
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r15", data_dir=ctx.datadir)
    rows = None
    try:
        if p.alive():
            code, body = jget("http://127.0.0.1:%d/healthz" % ctx.port)
            rows = (body or {}).get("rows")
        wait_ok = "[drain] waiting" in txt14
        ok = wait_ok and not markers
        record(cid, name, "PASS" if ok else "FAIL",
               "R14 排水等待=%s；残留标记=%s；重启后 /healthz.rows=%s（= R14 写入条数，全生效）"
               % (wait_ok, markers, rows))
    finally:
        p.stop()


# ----------------------------------------------------------------------------
# R16 / R22：耐久与 pragma 基线
# ----------------------------------------------------------------------------

def r16_kill9_durability(ctx, cid, name):
    ddir = os.path.join(ctx.workdir, "r16data")
    shutil.rmtree(ddir, ignore_errors=True)
    os.makedirs(ddir, exist_ok=True)
    prof = solo_profile(ctx, "r16", os.path.join(ddir, "lims.sqlite"))
    p = start_kernel(ctx, prof, ctx.port, "r16", data_dir=ddir)
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        n, codes = write_memories("http://127.0.0.1:%d" % ctx.port, 5)
        code, body = jget("http://127.0.0.1:%d/healthz" % ctx.port)
        rows_before = (body or {}).get("rows")
        p.kill9()
        wait_gone(ctx.port, timeout=15)
        time.sleep(1)
        p2 = start_kernel(ctx, prof, ctx.port, "r16b", data_dir=ddir)
        try:
            if not p2.alive():
                record(cid, name, "FAIL", "kill -9 后无法重启：%s" % p2.logtext()[-400:])
                return
            code2, body2 = jget("http://127.0.0.1:%d/healthz" % ctx.port)
            rows_after = (body2 or {}).get("rows")
            txt = p2.logtext()
            recovery = "recovery report" in txt
            # 注：/healthz.rows 是**启动自检快照**（非实时计数）。故判据是
            # 「kill -9 后重启，启动自检读到 5 条」——这正是「崩溃不丢已确认写」的证据。
            ok = n == 5 and rows_after == "5" and recovery
            record(cid, name, "PASS" if ok else "FAIL",
                   "写 %d 条(%s)；kill -9 前启动快照 rows=%s（启动时点，非实时）；"
                   "kill -9 后重启的启动自检 rows=%s；recovery report=%s"
                   % (n, codes, rows_before, rows_after, recovery))
        finally:
            p2.stop()
    finally:
        p.stop()


def r22_pragma_baseline(ctx, cid, name):
    db = os.path.join(ctx.workdir, "r16data", "lims.sqlite")
    if not os.path.exists(db):
        record(cid, name, "SKIP", "R16 未产出数据库文件 %s" % db)
        return
    code = (
        "import sqlite3\n"
        "c=sqlite3.connect('file:%s?mode=ro',uri=True)\n"
        "for p in ('journal_mode','synchronous','wal_autocheckpoint','busy_timeout'):\n"
        "    print(p,'=',c.execute('PRAGMA '+p).fetchone()[0])\n" % db)
    rc, out = sh('%s -c "%s"' % (sys.executable, code.replace('"', '\\"')), timeout=30)
    want = {"journal_mode": "wal", "synchronous": "2", "wal_autocheckpoint": "1000",
            "busy_timeout": "5000"}
    got = dict(re.findall(r"(\w+) = (\S+)", out))
    ok = all(got.get(k) == v for k, v in want.items())
    record(cid, name, "PASS" if ok else "FAIL",
           "只读(mode=ro)回读：%s（期望 synchronous=2 FULL / wal_autocheckpoint=1000 / busy_timeout=5000）"
           % out.replace("\n", " / "))


# ----------------------------------------------------------------------------
# R25：探针风暴 + 业务 P95
# ----------------------------------------------------------------------------

def r25_probe_storm(ctx, cid, name):
    p = start_kernel(ctx, ctx.solo_prof, ctx.port, "r25")
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        write_memories("http://127.0.0.1:%d" % ctx.port, 50)
        probe = os.path.join(ctx.repo, "scripts", "perf_probe.py")
        p95f = os.path.join(ctx.workdir, "p95_r25.txt")
        # 并发：1s × 60 打 /readyz，同时压 /search
        import threading
        codes = {}

        def _storm():
            for _ in range(60):
                c, _b = http("GET", "http://127.0.0.1:%d/readyz" % ctx.port, timeout=3)
                codes[c] = codes.get(c, 0) + 1
                time.sleep(1)
        th = threading.Thread(target=_storm)
        th.start()
        rc, out = sh("%s %s --url http://127.0.0.1:%d --seconds 62 --concurrency 4 "
                     "--path '/search?q=R29&tenant_id=t1' --p95-out %s --label r25"
                     % (sys.executable, probe, ctx.port, p95f), timeout=120)
        th.join()
        p95 = float(open(p95f).read().strip()) if os.path.exists(p95f) else 999
        mcode, mtxt = http("GET", "http://127.0.0.1:%d/metrics" % ctx.port)
        m = re.search(r"backend_probe_ms_count\s+([0-9.]+)", mtxt)
        probes = float(m.group(1)) if m else -1
        ok = (set(codes) <= {200}) and probes >= 30 and probes <= 70 and p95 <= 50.12
        record(cid, name, "PASS" if ok else "FAIL",
               "60×1s /readyz 返回分布=%s；backend_probe_ms_count=%.0f（TTL=1s，期望 ≈60）；"
               "并发压测下 /search P95=%.2fms（红线 ≤50.12）" % (codes, probes, p95))
    finally:
        p.stop()


# ----------------------------------------------------------------------------
# R6 / R7 / R8 / R24：PG 就绪判定
# ----------------------------------------------------------------------------

def _pg_ctl(ctx, verb):
    return sh("%s %s" % (ctx.pg_ctl, verb), timeout=90)


def _pg_running(ctx):
    rc, out = _pg_ctl(ctx, "status")
    return "server is running" in out.lower() or rc == 0


def _psql(ctx, sql, db=None, superuser=False, timeout=60):
    """执行一条 SQL。superuser 且给了 unix socket 目录时走 socket（local trust），
    避免把超级用户口令写进命令行/进程表。"""
    db = db or ctx.pg_db
    if superuser and ctx.pg_super_socket:
        cmd = ("%s -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=1 -tAc %s"
               % (ctx.psql, ctx.pg_super_socket, ctx.pg_port, ctx.pg_super_user,
                  db, _q(sql)))
    else:
        user = ctx.pg_super_user if superuser else ctx.pg_user
        pw = ctx.pg_super_pass if superuser else ctx.pg_password
        cmd = ("PGPASSWORD=%s %s -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=1 -tAc %s"
               % (_q(pw), ctx.psql, ctx.pg_host, ctx.pg_port, user, db, _q(sql)))
    return sh(cmd, timeout=timeout)


def _q(s):
    return "'" + str(s).replace("'", "'\\''") + "'"


def _pg_env(ctx):
    return {
        "LIMS_PG_HOST": ctx.pg_host,
        "LIMS_PG_PORT": str(ctx.pg_port),
        "LIMS_PG_USER": ctx.pg_user,
        "LIMS_PG_DB": ctx.pg_db,
        "LIMS_PG_PASSWORD": ctx.pg_password,
    }


def r6_pg_down(ctx, cid, name):
    prof = smb_profile(ctx, "pg_smb")
    p = start_kernel(ctx, prof, ctx.port, "r6", env_extra=_pg_env(ctx))
    try:
        if not p.alive():
            record(cid, name, "FAIL", "PG 档内核未起来：%s" % p.logtext()[-500:])
            return
        c_ok, b_ok = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        _pg_ctl(ctx, "stop -m fast")
        time.sleep(3)
        c_down, b_down = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        c_live, _ = http("GET", "http://127.0.0.1:%d/livez" % ctx.port)
        _pg_ctl(ctx, "start -w -l %s" % os.path.join(ctx.workdir, "pg_restart.log"))
        time.sleep(5)
        c_back, _ = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        ok = c_ok == 200 and c_down == 503 and c_back == 200 and c_live == 200
        record(cid, name, "PASS" if ok else "FAIL",
               "PG 在 → /readyz=%d；PG 停 → /readyz=%d(%s)；PG 停 → /livez=%d；PG 恢复 → /readyz=%d"
               % (c_ok, c_down, (b_down or {}).get("check", ""), c_live, c_back))
        record("R24", "停 PG → /livez 仍 200（liveness 与依赖解耦）",
               "PASS" if c_live == 200 else "FAIL",
               "PG 停止期间 /livez=%d（期望 200）" % c_live)
    finally:
        p.stop()


def r7_missing_table(ctx, cid, name):
    """R7：PG 在、连接正常，但 memories 表在**运行中**消失 → /readyz 必须 503。

    口径说明：启动时缺表已被 B2 启动自检 fail-fast 拦下（进程起不来，见 R18）；
    因此 R7 验证的是**运行期退化**——进程已就绪、表被删后 /readyz 必须转 503，
    而 /livez、/healthz 仍 200。这才是 L-244「健康判定与依赖分离」的诉求。
    """
    prof = smb_profile(ctx, "pg_smb")
    p = start_kernel(ctx, prof, ctx.port, "r7", env_extra=_pg_env(ctx))
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        c0, _ = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        _psql(ctx, "DROP TABLE IF EXISTS memories CASCADE;")
        time.sleep(2.5)          # 越过 /readyz 的 1s TTL 缓存
        c1, b1 = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        c_live, _ = http("GET", "http://127.0.0.1:%d/livez" % ctx.port)
        c_health, _ = jget("http://127.0.0.1:%d/healthz" % ctx.port)
        check = (b1 or {}).get("check", "")
        _restore_migration(ctx)
        time.sleep(2.5)
        c2, _ = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        ok = c0 == 200 and c1 == 503 and c_live == 200 and c2 == 200
        record(cid, name, "PASS" if ok else "FAIL",
               "表在 → /readyz=%d；运行中删表 → /readyz=%d(check=%s)、/livez=%d、/healthz=%d；"
               "恢复迁移 → /readyz=%d（期望 200 / 503 / 200 / 200 / 200）"
               % (c0, c1, check, c_live, c_health, c2))
    finally:
        p.stop()


def _restore_migration(ctx):
    mig = os.path.join(ctx.repo, "kernel", "migrations", "001_pgvector.up.sql")
    rc, out = sh("PGPASSWORD=%s %s -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=1 -f %s"
                 % (_q(ctx.pg_password), ctx.psql, ctx.pg_host, ctx.pg_port,
                    ctx.pg_user, ctx.pg_db, mig), timeout=90)
    log("   [restore] 重新应用迁移 rc=%d %s" % (rc, out[-160:].replace("\n", " ")))
    return rc


def r8_empty_table_ok(ctx, cid, name):
    prof = smb_profile(ctx, "pg_smb")
    _psql(ctx, "TRUNCATE memories;")
    p = start_kernel(ctx, prof, ctx.port, "r8", env_extra=_pg_env(ctx))
    try:
        if not p.alive():
            record(cid, name, "FAIL", "内核未起来：%s" % p.logtext()[-400:])
            return
        c, b = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        n, _ = write_memories("http://127.0.0.1:%d" % ctx.port, 2, tenant="t1", user="u1")
        c2, _ = jget("http://127.0.0.1:%d/readyz" % ctx.port)
        ok = c == 200 and c2 == 200 and n == 2
        record(cid, name, "PASS" if ok else "FAIL",
               "空表 → /readyz=%d；写 2 条(成功 %d) → /readyz=%d（期望 200/2/200）" % (c, n, c2))
    finally:
        p.stop()


def r18_pg_unmigrated_failfast(ctx, cid, name):
    empty_db = "r29f_empty"
    _psql(ctx, "DROP DATABASE IF EXISTS %s;" % empty_db, db="postgres", superuser=True)
    rc, out = _psql(ctx, "CREATE DATABASE %s;" % empty_db, db="postgres", superuser=True)
    log("   [setup] create empty db rc=%d %s" % (rc, out[-120:]))
    prof = smb_profile(ctx, "pg_smb")
    env = dict(_pg_env(ctx))
    env["LIMS_PG_DB"] = empty_db
    ddir = os.path.join(ctx.workdir, "r18data")
    os.makedirs(ddir, exist_ok=True)
    p = start_kernel(ctx, prof, ctx.port, "r18", env_extra=env, data_dir=ddir, wait=20)
    try:
        time.sleep(1)
        rc_exit = p.p.poll()
        txt = p.logtext()
        fast_fail = (rc_exit not in (None, 0)) and ("boot report" in txt or "boot self-check" in txt)
        record(cid, name, "PASS" if fast_fail else "FAIL",
               "未迁移空库 + -data-dir → exit_code=%s；日志：%s"
               % (rc_exit, [l for l in txt.splitlines() if "boot" in l][-2:]))
    finally:
        p.stop()
        _psql(ctx, "DROP DATABASE IF EXISTS %s;" % empty_db, db="postgres", superuser=True)


def r19_smb_connects_primary(ctx, cid, name):
    prof = smb_profile(ctx, "pg_smb")
    p = start_kernel(ctx, prof, ctx.port, "r19", env_extra=_pg_env(ctx))
    try:
        if not p.alive():
            record(cid, name, "FAIL", "SMB 档未起来：%s" % p.logtext()[-400:])
            return
        code, body = jget("http://127.0.0.1:%d/healthz" % ctx.port)
        backend = (body or {}).get("backend")
        n, _ = write_memories("http://127.0.0.1:%d" % ctx.port, 2)
        c2, s = http("GET", "http://127.0.0.1:%d/search?q=R29&tenant_id=t1" % ctx.port)
        ok1 = backend == "pgvector" and n == 2 and c2 == 200
        record(cid, name, "PASS" if ok1 else "FAIL",
               "SMB profile + LIMS_PG_* 覆盖 → /healthz.backend=%s；写 2 条成功 %d；/search=%d"
               % (backend, n, c2))
        # B1 反证：pg.host 置空 → 启动必须明确报错
        bad = smb_profile(ctx, "pg_smb_nohost", drop_host=True)
        pb = start_kernel(ctx, bad, ctx.port + 1, "r19b1", env_extra={}, wait=15)
        try:
            time.sleep(1)
            rc = pb.p.poll()
            txt = pb.logtext()
            b1 = (rc not in (None, 0)) and ("storage.pg.host" in txt)
            record("R19-B1", "pg 档缺 storage.pg.host → 启动 fail-fast（B1 行为变更）",
                   "PASS" if b1 else "FAIL",
                   "exit_code=%s；日志：%s" % (rc, [l for l in txt.splitlines() if "host" in l][:2]))
        finally:
            pb.stop()
    finally:
        p.stop()


# ----------------------------------------------------------------------------
# R21 + 红线复核（static）
# ----------------------------------------------------------------------------

def r21_readme_restart_proof(ctx, cid, name):
    path = os.path.join(ctx.repo, "integrations", "lims-088-rc.d", "README.md")
    txt = open(path, encoding="utf-8", errors="replace").read()
    keys = ["重启自证", "kill -9", "端口唯一", "条数不丢"]
    miss = [k for k in keys if k not in txt]
    record(cid, name, "PASS" if not miss else "FAIL",
           "README 命中 %s；缺 %s" % ([k for k in keys if k in txt], miss))


def redline_checks(ctx, cid, name):
    repo = ctx.repo
    ev = []
    rc, out = sh("git -C %s diff --stat %s..HEAD -- kernel/migrations go.mod go.sum"
                 % (repo, ctx.baseline_ref), timeout=30)
    clean = (rc == 0 and not out.strip())
    ev.append("migrations/go.mod/go.sum 相对 %s 零变化=%s%s"
              % (ctx.baseline_ref, clean, "" if clean else " :: " + out.strip()[:200]))
    rc2, out2 = sh("git -C %s diff %s..HEAD -- kernel/storage.go kernel/model.go | head -40"
                   % (repo, ctx.baseline_ref), timeout=30)
    ev.append("storage.go/model.go 无 diff=%s" % (not out2.strip()))
    # ScanAll 契约：接口定义（consolidate.go:MemoryScanner）+ 两个后端实现签名一字不变
    want = "ScanAll(ctx context.Context, max int) ([]*Memory, error)"
    hits = {}
    for f in ("kernel/consolidate.go", "kernel/backend_sqlite.go", "kernel/backend_pg.go"):
        rc4, out4 = sh("grep -cF '%s' %s" % (want, os.path.join(repo, f)), timeout=10)
        hits[f] = out4.strip()
    sig_ok = all(v.isdigit() and int(v) >= 1 for v in hits.values())
    ev.append("ScanAll 签名未变=%s %s" % (sig_ok, hits))
    ok = clean and not out2.strip() and sig_ok
    record(cid, name, "PASS" if ok else "FAIL", "；".join(ev))


# ----------------------------------------------------------------------------
# 单测组（B/C/D/E-unit）
# ----------------------------------------------------------------------------

def unit_group(ctx, cid, name, pattern):
    rc, out = sh("cd %s/kernel && go test -count=1 ./ -run '%s' 2>&1 | tail -25"
                 % (ctx.repo, pattern), timeout=600)
    ok = rc == 0 and "FAIL" not in out
    record(cid, name, "PASS" if ok else "FAIL", "go test -run '%s' rc=%d :: %s"
           % (pattern, rc, out.strip().splitlines()[-3:] if out.strip() else ""))


# ----------------------------------------------------------------------------
# main
# ----------------------------------------------------------------------------

def build_ctx(args):
    ctx = Ctx()
    ctx.repo = os.path.abspath(args.repo)
    ctx.workdir = os.path.abspath(args.workdir)
    os.makedirs(ctx.workdir, exist_ok=True)
    ctx.bin = args.bin
    ctx.mcp_bin = args.mcp_bin
    ctx.baseline_bin = args.baseline_bin
    ctx.baseline_ref = args.baseline_ref
    ctx.port = args.port
    ctx.mcp_port = args.mcp_port
    ctx.token = args.token
    ctx.tokens_json = json.dumps({args.token: {
        "agent_id": "r29f-agent", "user_id": "u1", "tenant_id": "t1"}})
    ctx.sqlite_b = os.path.join(ctx.workdir, "r12base.sqlite")
    ctx.datadir = os.path.join(ctx.workdir, "r14data")
    os.makedirs(ctx.datadir, exist_ok=True)
    ctx.solo_prof = solo_profile(ctx, "r29f_solo", os.path.join(ctx.workdir, "solo.sqlite"))
    ctx.service = args.service_name
    ctx.service_bin_pat = args.service_bin_pat
    ctx.svc_port = args.svc_port
    ctx.svc_log = args.svc_log
    ctx.svc_chdir = args.svc_chdir
    rc, _ = sh("sudo -n true", timeout=10)
    ctx.sudo_ok = (rc == 0)
    ctx.pg_host = args.pg_host
    ctx.pg_port = args.pg_port
    ctx.pg_db = args.pg_db
    ctx.pg_user = args.pg_user
    ctx.pg_password = args.pg_password
    ctx.pg_super_user = args.pg_super_user
    ctx.pg_super_pass = args.pg_super_pass
    ctx.pg_super_socket = args.pg_super_socket
    ctx.pg_ctl = args.pg_ctl
    ctx.psql = args.psql
    return ctx


def main():
    global LOG_FH
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="all",
                    choices=["static", "solo", "pg", "service", "all"])
    ap.add_argument("--repo", default="/home/ai/lightmem")
    ap.add_argument("--workdir", default="/tmp/r29f")
    ap.add_argument("--bin", default="")
    ap.add_argument("--mcp-bin", default="")
    ap.add_argument("--baseline-bin", default="")
    ap.add_argument("--baseline-ref", default="c203db8")
    ap.add_argument("--token", default="t-r29f-accept")
    ap.add_argument("--port", type=int, default=18911)
    ap.add_argument("--mcp-port", type=int, default=18912)
    ap.add_argument("--service-name", default="lightmem")
    ap.add_argument("--service-bin-pattern", dest="service_bin_pat",
                    default="lightmem-kernel", help="pgrep -x 精确进程名")
    ap.add_argument("--svc-port", type=int, default=18443)
    ap.add_argument("--svc-log", default="/var/log/lightmem.log")
    ap.add_argument("--svc-chdir", default="/usr/local/lightmem",
                    help="rc.d 的 lightmem_chdir（调用 service 前 cd 进去，规避 F-1）")
    ap.add_argument("--pg-host", default="127.0.0.1")
    ap.add_argument("--pg-port", type=int, default=15432)
    ap.add_argument("--pg-db", default="r29f")
    ap.add_argument("--pg-user", default="lightmem_app")
    ap.add_argument("--pg-password", default="")
    ap.add_argument("--pg-super-user", default="workbuddy")
    ap.add_argument("--pg-super-pass", default="")
    ap.add_argument("--pg-super-socket", default="",
                    help="superuser 走 unix socket 目录（local trust），免口令")
    ap.add_argument("--pg-ctl", default="")
    ap.add_argument("--psql", default="/usr/local/bin/psql")
    ap.add_argument("--only", default="")
    ap.add_argument("--skip", default="")
    ap.add_argument("--log", default="")
    args = ap.parse_args()

    if args.log:
        LOG_FH = open(args.log, "w", encoding="utf-8")
    ctx = build_ctx(args)

    log("R29 F 线验收  mode=%s  repo=%s  workdir=%s" % (args.mode, ctx.repo, ctx.workdir))
    log("kernel=%s  mcp=%s  baseline=%s  sudo=%s  pg=%s:%d/%s"
        % (ctx.bin, ctx.mcp_bin, ctx.baseline_bin, ctx.sudo_ok,
           ctx.pg_host, ctx.pg_port, ctx.pg_db))

    only = set(x.strip() for x in args.only.split(",") if x.strip())
    skip = set(x.strip() for x in args.skip.split(",") if x.strip())

    def enabled(cid):
        if only:
            return cid in only
        return cid not in skip

    plan = []
    if args.mode in ("static", "all"):
        plan += [("R21", "README 含「重启自证」四步", r21_readme_restart_proof),
                 ("RED", "红线复核（migrations/go.mod/ScanAll 未变）", redline_checks)]
    if args.mode in ("solo", "all"):
        plan += [
            ("R9", "运维端点免鉴权（5 端点）", r9_probe_exempt),
            ("R4", "鉴权矩阵四项", r4_auth_matrix),
            ("R10", "MCP 就绪探上游", r10_mcp_readiness),
            ("R12", "默认关自证（无调度/无自检/与基线逐字节一致）", r12_default_off),
            ("R11", "生命周期定时器真起（60s×3min）", r11_lifecycle_timer),
            ("R16", "写 N 条 → kill -9 → 重启条数一致", r16_kill9_durability),
            ("R22", "SQLite pragma 只读基线", r22_pragma_baseline),
            ("R14", "整理中 SIGTERM → 排水等待、无 use-after-close", r14_sigterm_midflight),
            ("R15", "R14 后：标记清理 + 条数可读", r15_marker_and_data),
            ("R13", "批量扫描 P95 劣化 ≤20% + 无 database is locked", r13_batch_p95),
            ("R25", "探针风暴下 /search P95 与 backend_probe_ms", r25_probe_storm),
            ("B-unit", "B 线单测（Probe|Health|Readyz|MCP）", lambda cx, c, n: unit_group(
                cx, c, n, "Probe|Health|Readyz|MCP")),
            ("C-unit", "C 线单测（Maintenance|Scheduler|Window|Backoff|Consolidate）",
             lambda cx, c, n: unit_group(cx, c, n, "Maintenance|Scheduler|Window|Backoff|Consolidate")),
            ("D-unit", "D 线单测（Drain|Batched）", lambda cx, c, n: unit_group(
                cx, c, n, "Drain|Batched")),
            ("E-unit", "E 线单测（Boot|Durability|Pragma|Checkpoint）", lambda cx, c, n: unit_group(
                cx, c, n, "Boot|Durability|Pragma|Checkpoint")),
        ]
    if args.mode in ("pg", "all"):
        plan += [
            ("R6", "停 PG → /readyz 503；恢复 → 200", r6_pg_down),
            ("R7", "PG 在但 memories 表缺失 → /readyz 503", r7_missing_table),
            ("R8", "空表 → /readyz 200", r8_empty_table_ok),
            ("R18", "PG 未迁移空库 → 启动 fail-fast（B2）", r18_pg_unmigrated_failfast),
            ("R19", "SMB 档连对主库 + 缺 host 明确报错（B1）", r19_smb_connects_primary),
        ]
    if args.mode in ("service", "all"):
        plan += [
            ("R1", "service stop 后 20s 不复活", r1_stop_no_resurrect),
            ("R2", "kill -9 → 15s 内自动恢复 + /readyz 200", r2_kill9_autorecover),
            ("R3", "端口监听恰好 1 行", r3_single_listener),
            ("R23", "崩溃循环有界（间隔 ≥5s、不刷屏）", r23_restart_bounded),
        ]

    t0 = time.time()
    for cid, name, fn in plan:
        if not enabled(cid):
            log("")
            log("[%s] SKIP  %s :: 按 --only/--skip 排除" % (cid, name))
            RESULTS.append({"id": cid, "name": name, "status": "SKIP",
                            "evidence": "--only/--skip 排除"})
            continue
        guard(cid, name, fn, ctx)

    # R5 / R20：本轮不脚本化（需重启窗口 / ZFS 池），显式登记 SKIP
    if args.mode in ("service", "all"):
        RESULTS.append({"id": "R5", "name": "reboot 后自启", "status": "SKIP",
                        "evidence": "需重启窗口（会中断 0.82 上的其他验证），转运维窗口执行"})
        log("[R5] SKIP  reboot 后自启 :: 需重启窗口，转运维窗口执行")
    if args.mode in ("service", "all"):
        RESULTS.append({"id": "R17", "name": "断电实测（掉电后恢复）", "status": "SKIP",
                        "evidence": "任务书 §0.4 明确出范围：需停机/断电窗口，列入运维计划（第 2 轮后）"})
        log("[R17] SKIP  断电实测 :: 需停机窗口（§0.4 出范围），转运维计划")
    if args.mode in ("solo", "pg", "all"):
        RESULTS.append({"id": "R20", "name": "zfs_backup list + 真实 rollback", "status": "SKIP",
                        "evidence": "0.82/0.88 验收机无独立测试池（rollback 会毁生产快照），转运维计划"})
        log("[R20] SKIP  zfs 回滚演练 :: 无独立测试池，转运维计划")

    # 汇总
    log("")
    log("=" * 78)
    log("== 汇总（mode=%s，用时 %.0fs）" % (args.mode, time.time() - t0))
    log("=" * 78)
    order = {"PASS": 0, "FAIL": 1, "SKIP": 2}
    counts = {"PASS": 0, "FAIL": 0, "SKIP": 0}
    for r in RESULTS:
        counts[r["status"]] = counts.get(r["status"], 0) + 1
        log("  %-6s %-7s %s" % (r["id"], r["status"], r["name"]))
    log("")
    log("PASS: %d    FAIL: %d    SKIP: %d" % (counts["PASS"], counts["FAIL"], counts["SKIP"]))
    fails = [r["id"] for r in RESULTS if r["status"] == "FAIL"]
    if fails:
        log("FAIL 项：%s" % ", ".join(fails))
    if LOG_FH:
        LOG_FH.close()
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
