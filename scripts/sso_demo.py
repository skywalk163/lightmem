#!/usr/bin/env python3
# R23.3 SSO 一键试跑：起本地测试 OIDC IdP（oidc-stub）+ kernel + frontend，
# 模拟浏览器走完 /login/sso → IdP 授权 → 回调换令牌验签 → 建会话 → 进工作台。
# 用法: python3 scripts/sso_demo.py
import hashlib, json, os, sqlite3, subprocess, sys, time, urllib.request, urllib.error, urllib.parse, http.cookiejar, signal, datetime

HERE = "/home/workbuddy/lightmem"
IDP = "/tmp/oidc-stub"
KERNEL = "/tmp/lmk"
FRONT = "/tmp/lmf"
IDP_URL = "http://127.0.0.1:18091"     # 测试 IdP
HTTP = "http://127.0.0.1:18556"        # kernel 业务
ADMIN = "http://127.0.0.1:18092"       # kernel admin
FE = "http://127.0.0.1:18090"          # frontend
CB = FE + "/login/sso/callback"        # 前台回调地址
CID = "demo-client"
CSEC = "demo-secret"
KDB = os.path.join(HERE, "data", "lims.sqlite")
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
        return None

class Browser:
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar), _NoRedirect())
    def get(self, path):
        # 支持绝对 URL（跨进程：跳转到 IdP 时 base 不同）。
        url = path if path.startswith("http") else self.base + path
        req = urllib.request.Request(url, headers={"User-Agent": "sso-demo"})
        try:
            r = self.op.open(req, timeout=8)
            return r.status, r.headers.get("Location", ""), r.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            loc = e.headers.get("Location", "") if e.headers else ""
            return e.code, loc, e.read().decode(errors="replace")
    def cookies(self, name):
        return [c.value for c in self.jar if c.name == name]

def wait_up(url, tries=30):
    for _ in range(tries):
        try:
            urllib.request.urlopen(url, timeout=2); return True
        except Exception: time.sleep(1)
    return False

def main():
    print("===== R23.3 SSO 一键试跑（本地测试 OIDC IdP）=====", flush=True)
    os.chdir(HERE)
    procs = []
    def start(bin, args, logname):
        p = subprocess.Popen([bin] + args, stdout=open(f"/tmp/{logname}.log", "w"), stderr=subprocess.STDOUT, preexec_fn=os.setsid)
        procs.append(p); return p
    try:
        log("启动测试 OIDC IdP (oidc-stub)…")
        start(IDP, ["-listen", "127.0.0.1:18091", "-issuer", IDP_URL, "-client-id", CID, "-client-secret", CSEC,
                    "-user-name", "Test User", "-user-email", "test@example.com", "-user-sub", "user-test-001"], "idp")
        log("启动 kernel（业务+admin）…")
        start(KERNEL, ["-profile", "profiles/lims-solo.profile.yaml", "-listen", "127.0.0.1:18556", "-admin-listen", "127.0.0.1:18092"], "k")
        log("启动 frontend（带 SSO 配置）…")
        start(FRONT, ["-listen", "127.0.0.1:18090", "-store", "/tmp/fe_sso.db", "-backend", HTTP, "-admin", ADMIN,
                      "-service-token", ADMIN_TOKEN, "-tenant", "dshteam", "-roles", "user",
                      "-oidc-issuer", IDP_URL, "-oidc-client-id", CID, "-oidc-client-secret", CSEC,
                      "-oidc-redirect-uri", CB], "f")

        for p in (KDB, "/tmp/fe_sso.db"):
            if os.path.exists(p): os.remove(p)
        time.sleep(0.2)
        check("oidc-stub 起来", wait_up(IDP_URL + "/jwks"))
        check("kernel /healthz 起来", wait_up(HTTP + "/healthz"))
        seed_admin()
        check("frontend /health 起来", wait_up(FE + "/health"))

        b = Browser(FE)
        # 1) 登录页应显示 SSO 入口（已配置）
        st, _, body = b.get("/login")
        check("登录页显示 SSO 入口", st == 200 and "SSO" in body, f"got {st}")

        # 2) 发起 SSO：302 到 IdP 授权端点，带 state/nonce/PKCE
        st, loc, _ = b.get("/login/sso")
        check("发起 SSO → 302 到 IdP", st in (302, 303) and loc.startswith(IDP_URL + "/auth?"), f"got {st} {loc[:70]}")
        q = urllib.parse.parse_qs(urllib.parse.urlparse(loc).query)
        for k in ("state", "nonce", "code_challenge", "client_id", "redirect_uri"):
            check(f"授权请求带 {k}", bool(q.get(k, [""])[0]), f"{q.get(k)}")
        check("code_challenge_method=S256", q.get("code_challenge_method", [""])[0] == "S256", f"{q.get('code_challenge_method')}")

        # 3) IdP 直接授权：302 回前台回调，带 code + state
        st, loc, _ = b.get(loc)  # loc 已是 IdP 的完整授权 URL
        check("IdP 授权 → 302 回回调", st in (302, 303) and loc.startswith(CB + "?"), f"got {st} {loc[:80]}")
        cbq = urllib.parse.parse_qs(urllib.parse.urlparse(loc).query)
        check("回调带 code", bool(cbq.get("code", [""])[0]))
        check("回调带原 state", bool(cbq.get("state", [""])[0]) and cbq["state"][0] == q["state"][0])

        # 4) 前台回调：换令牌+验签 → 302 /workspace + 会话 cookie
        cb_path = urllib.parse.urlparse(loc).path + "?" + urllib.parse.urlparse(loc).query
        st, loc2, _ = b.get(cb_path)
        sess = b.cookies("lightmem_session")
        check("回调 → 302/303 /workspace", st in (302, 303) and loc2.rstrip("/").endswith("/workspace"), f"got {st} {loc2}")
        check("回调下发会话 cookie", bool(sess), f"cookies={len(sess)}")

        # 5) 用会话进工作台
        st, _, body = b.get("/workspace")
        check("SSO 后 /workspace 200", st == 200, f"got {st}")
        check("工作台显示 SSO 用户身份(sso-…)", "sso-" in body or "Test User" in body, "")

        # 6) 工作台里能直接调 API（会话有效）
        st, _, body = b.get("/api/goals")
        check("SSO 会话可调 /api/goals", st == 200, f"got {st}")

        print("\n===== 试跑汇总 =====", flush=True)
        p = sum(1 for x in results if x)
        print(f"  结果：{p}/{len(results)} PASS", flush=True)
        return 0 if p == len(results) else 1
    finally:
        for p in procs:
            try: os.killpg(os.getpgid(p.pid), signal.SIGTERM)
            except Exception: pass
        time.sleep(1)

if __name__ == "__main__":
    sys.exit(main())