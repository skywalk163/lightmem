#!/usr/bin/env python3
# R23 前台服务端到端验收：起 kernel(admin) + frontend，模拟浏览器走注册→登录→工作台→API→登出。
import hashlib, json, os, sqlite3, subprocess, sys, time, urllib.request, urllib.error, urllib.parse, http.cookiejar, signal, datetime

HERE = "/home/workbuddy/lightmem"
KERNEL = "/tmp/lmk"
FRONT = "/tmp/lmf"
HTTP = "http://127.0.0.1:18556"     # kernel 业务
ADMIN = "http://127.0.0.1:18092"    # kernel admin
FE = "http://127.0.0.1:18090"       # frontend
KDB = os.path.join(HERE, "data", "lims.sqlite")
FDB = "/tmp/fe.db"
ADMIN_TOKEN = "t-admin-accept-x"
results = []

def log(m): print("  " + m, flush=True)
def check(name, cond, detail=""):
    results.append(cond)
    print(f"  [{'PASS' if cond else 'FAIL'}] {name} {detail}", flush=True)

def seed_admin():
    h = hashlib.sha256(ADMIN_TOKEN.encode()).hexdigest()
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    db = sqlite3.connect(KDB)
    db.execute("INSERT OR REPLACE INTO auth_tokens (id, token_hash, token_hint, user_id, tenant_id, roles, scope, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)",
        ("at-admin-1", h, ADMIN_TOKEN[:8], "admin", "dshteam", "admin", "*", "bootstrap", now))
    db.commit(); db.close()

class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None  # 阻止 urllib 自动跟随 3xx

class Browser:
    """模拟浏览器：cookie jar + JSON/表单请求；不跟随重定向（便于断言 302）。"""
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar),
            _NoRedirect(),
        )
        self.csrf = ""
    def get(self, path, allow_redirect=True):
        req = urllib.request.Request(self.base + path, headers={"User-Agent": "r23-accept"})
        try:
            r = self.op.open(req, timeout=5)
            return r.status, r.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode(errors="replace")
    def post_form(self, path, form):
        data = urllib.parse.urlencode(form).encode()
        req = urllib.request.Request(self.base + path, data=data, headers={"User-Agent": "r23-accept", "Content-Type": "application/x-www-form-urlencoded"})
        try:
            r = self.op.open(req, timeout=8)
            return r.status, r.headers.get("Location", ""), r.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            loc = e.headers.get("Location", "") if e.headers else ""
            return e.code, loc, e.read().decode(errors="replace")
    def post_json(self, path, body):
        data = json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, headers={"User-Agent": "r23-accept", "Content-Type": "application/json"})
        try:
            r = self.op.open(req, timeout=8)
            return r.status, json.loads(r.read().decode(errors="replace") or "{}")
        except urllib.error.HTTPError as e:
            raw = e.read().decode(errors="replace")
            try: return e.code, json.loads(raw or "{}")
            except Exception: return e.code, {"raw": raw[:120]}
    def refresh_csrf(self, path):
        self.get(path)
        for ck in self.jar:
            if ck.name == "lightmem_csrf":
                self.csrf = ck.value

def _mid(r):
    if not isinstance(r, dict):
        return ""
    if r.get("memory_id"):
        return r["memory_id"]
    m = r.get("memory") or {}
    return m.get("memory_id", "") if isinstance(m, dict) else ""

def wait_up(url, tries=20):
    for _ in range(tries):
        try:
            urllib.request.urlopen(url, timeout=2); return True
        except Exception: time.sleep(1)
    return False

def main():
    print("===== R23 前台服务端到端验收 =====", flush=True)
    os.chdir(HERE)
    for p in (KDB, FDB):
        if os.path.exists(p): os.remove(p)
    log("启动 kernel（业务+admin）…")
    k = subprocess.Popen([KERNEL, "-profile", "profiles/lims-solo.profile.yaml",
        "-listen", "127.0.0.1:18556", "-admin-listen", "127.0.0.1:18092"],
        stdout=open("/tmp/k.log", "w"), stderr=subprocess.STDOUT, preexec_fn=os.setsid)
    log("启动 frontend…")
    f = subprocess.Popen([FRONT, "-listen", "127.0.0.1:18090", "-store", FDB,
        "-backend", HTTP, "-admin", ADMIN, "-service-token", ADMIN_TOKEN,
        "-tenant", "dshteam", "-roles", "user"],
        stdout=open("/tmp/f.log", "w"), stderr=subprocess.STDOUT, preexec_fn=os.setsid)
    try:
        check("kernel /healthz 起来", wait_up(HTTP + "/healthz"))
        seed_admin()
        check("frontend /health 起来", wait_up(FE + "/health"))

        b = Browser(FE)
        st, body = b.get("/")
        check("首页 200", st == 200 and "Lightmem" in body, f"got {st}")

        # 未登录访问工作台 → 302 /login
        b2 = Browser(FE)
        st, _ = b2.get("/workspace")
        check("未登录 /workspace → 302/303 /login", st in (302, 303), f"got {st}")

        # 注册（先取 csrf）
        b.refresh_csrf("/register")
        st, url, _ = b.post_form("/register", {"_csrf": b.csrf, "username": "alice",
            "password": "s3cret-pass", "password2": "s3cret-pass", "display_name": "Alice"})
        check("注册 → 302/303 /workspace", st in (302, 303) and url.endswith("/workspace"), f"got {st}->{url}")

        st, body = b.get("/workspace")
        check("已登录 /workspace 200 含标题", st == 200 and "记忆工作台" in body, f"got {st}")

        # 录入记忆
        st, r = b.post_json("/api/memory/save", {"fact": "R23 前台服务首次录入", "kind": "fact", "visibility": "private"})
        mid = _mid(r)
        check("POST /api/memory/save → 2xx + memory_id", st in (200, 201) and bool(mid), f"got {st} mid={mid} err={r.get("error","")}")

        # 召回
        st, r = b.post_json("/api/memory/recall", {"text": "R23 前台"})
        items = r.get("results") or r.get("items") or []
        check("POST /api/memory/recall → 200", st == 200, f"got {st} items={len(items)}")

        # 计划
        st, r = b.get("/api/goals")
        check("GET /api/goals → 200", st == 200, f"got {st}")
        st, r = b.post_json("/api/goals/create", {"fact": "R23 验收目标", "goal_detail": {"status": "active", "constraints": ["R23"], "kpi": {"metric": "PASS", "target": 1, "cadence": "once"}, "deadline": "", "progress": 0}})
        gid = _mid(r)
        check("POST /api/goals/create → 200/201 + id", st in (200, 201) and bool(gid), f"got {st} gid={gid} err={r.get("error","")}")
        if gid:
            st, r = b.post_json("/api/goals/" + gid + "/progress", {"progress": 0.5})
            check("POST /api/goals/{id}/progress → 200", st == 200, f"got {st}")
            st, r = b.post_json("/api/goals/" + gid + "/state", {"state": "completed"})
            check("POST /api/goals/{id}/state → 200", st == 200, f"got {st}")

        # 社区/公开浏览
        st, r = b.get("/api/discover?q=test")
        check("GET /api/discover → 200", st == 200, f"got {st}")

        # ===== R23.2 社区公开：public 跨用户可读、private 不泄露 =====
        pub_fact = "R23 公开记忆·共享大脑试点-" + str(int(time.time()))
        st, r = b.post_json("/api/memory/save", {"fact": pub_fact, "kind": "fact", "visibility": "public"})
        pmid = _mid(r)
        check("R23.2 保存 public 记忆 → 2xx + memory_id", st in (200, 201) and bool(pmid), f"got {st} mid={pmid} err={r.get('error','')}")

        bb = Browser(FE); bb.refresh_csrf("/register")
        st, url, _ = bb.post_form("/register", {"_csrf": bb.csrf, "username": "bob-r23", "password": "s3cret-pass", "password2": "s3cret-pass", "display_name": "Bob"})
        check("R23.2 bob 注册 → /workspace", st in (302, 303) and url.endswith("/workspace"), f"got {st}->{url}")

        st, body = bb.get("/api/discover?q=" + urllib.parse.quote("共享大脑"))
        try:
            _d = json.loads(body)
            items = _d if isinstance(_d, list) else (_d.get("items") or _d.get("results") or [])
        except Exception: items = []
        hit = any(pub_fact[:24] in (m.get("fact") or m.get("content") or "") for m in items)
        check("R23.2 他人 discover 读到 public", st == 200 and hit, f"got {st} items={len(items)}")

        priv_fact = "bob私密笔记-" + str(int(time.time()))
        st, r = bb.post_json("/api/memory/save", {"fact": priv_fact, "kind": "fact", "visibility": "private"})
        st, body = b.get("/api/discover?q=" + urllib.parse.quote("私密笔记"))
        try:
            _d = json.loads(body)
            items = _d if isinstance(_d, list) else (_d.get("items") or _d.get("results") or [])
        except Exception: items = []
        leak = any(priv_fact[:18] in (m.get("fact") or m.get("content") or "") for m in items)
        check("R23.2 private 不泄露到社区", st == 200 and not leak, f"got {st} leak={leak}")

        # ===== R23.3 SSO/OIDC：默认关（接线默认关），发起与回调均 503 =====
        st, body = b.get("/login/sso")
        check("R23.3 未配置 SSO：/login/sso → 503", st == 503, f"got {st}")
        st, body = b.get("/login/sso/callback?state=x&code=y")
        check("R23.3 未配置 SSO：回调 → 503", st == 503, f"got {st}")

        # 登出
        b.refresh_csrf("/workspace")
        st, url, _ = b.post_form("/logout", {"_csrf": b.csrf})
        check("登出 → 302/303 /", st in (302, 303) and (url == "/" or url.rstrip("/").endswith("127.0.0.1:18090")), f"got {st}->{url}")
        st, _ = b.get("/workspace")
        check("登出后 /workspace → 302/303 /login", st in (302, 303), f"got {st}")

        # 密码错误
        b3 = Browser(FE); b3.refresh_csrf("/login")
        st, _, body = b3.post_form("/login", {"_csrf": b3.csrf, "username": "alice", "password": "wrong"})
        check("错密码登录 → 200 + 错误提示", st == 200 and "用户名或密码错误" in body, f"got {st}")

        # R23.3 未配置 SSO：未登录会话下登录页不显示 SSO 入口。
        b4 = Browser(FE); b4.refresh_csrf("/login")
        st, body = b4.get("/login")
        check("R23.3 未配置 SSO：登录页无 SSO 入口", st == 200 and "SSO" not in body, f"got {st}")

        print("\n===== 验收汇总 =====", flush=True)
        passed = sum(1 for x in results if x)
        print(f"  结果：{passed}/{len(results)} PASS", flush=True)
        return 0 if passed == len(results) else 1
    finally:
        for p in (f, k):
            try: os.killpg(os.getpgid(p.pid), signal.SIGTERM)
            except Exception: pass
        time.sleep(1)

if __name__ == "__main__":
    sys.exit(main())