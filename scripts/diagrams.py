#!/usr/bin/env python3
"""Draw the doc diagrams as theme-aware SVG, English and Chinese.

Usage: python3 scripts/diagrams.py docs/assets
"""
import sys
from html import escape

OUT = sys.argv[1]

FONT = ('-apple-system, BlinkMacSystemFont, &quot;Segoe UI&quot;, &quot;PingFang SC&quot;, '
        '&quot;Hiragino Sans GB&quot;, &quot;Microsoft YaHei&quot;, &quot;Noto Sans CJK SC&quot;, '
        '&quot;Noto Sans SC&quot;, Helvetica, Arial, sans-serif')
MONO = 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace'

STYLE = f"""<style>
  svg {{ font-family: {FONT}; }}
  .bg {{ fill: #ffffff; }}
  .frame {{ fill: #f2f8f8; stroke: #0d7b84; stroke-width: 1.5; }}
  .box {{ fill: #ffffff; stroke: #c9d3d8; stroke-width: 1; }}
  .key {{ fill: #ffffff; stroke: #0d7b84; stroke-width: 1.5; }}
  .pill {{ fill: #f5f7f8; stroke: #c9d3d8; stroke-width: 1; }}
  .warn {{ fill: #fff7ec; stroke: #e0b26a; stroke-width: 1; }}
  .t {{ fill: #14212b; font-size: 14px; font-weight: 600; }}
  .s {{ fill: #56656f; font-size: 12px; }}
  .p {{ fill: #14212b; font-size: 13px; }}
  .lbl {{ fill: #56656f; font-size: 12px; }}
  .sec {{ fill: #56656f; font-size: 12px; font-weight: 600; letter-spacing: .04em; }}
  .brand {{ fill: #0d7b84; font-size: 15px; font-weight: 700; }}
  .num {{ fill: #ffffff; font-size: 12px; font-weight: 700; }}
  .dot {{ fill: #0d7b84; }}
  .mono {{ font-family: {MONO}; font-size: 11.5px; fill: #0d7b84; }}
  .a {{ stroke: #0d7b84; stroke-width: 1.6; fill: none; }}
  .r {{ stroke: #7a8a94; stroke-width: 1.4; fill: none; stroke-dasharray: 4 3; }}
  .g {{ stroke: #9aa8b0; stroke-width: 1.2; fill: none; }}
  .life {{ stroke: #c9d3d8; stroke-width: 1; stroke-dasharray: 3 4; }}
  .ha {{ fill: #0d7b84; }} .hr {{ fill: #7a8a94; }} .hg {{ fill: #9aa8b0; }}
  @media (prefers-color-scheme: dark) {{
    .bg {{ fill: #0d1117; }}
    .frame {{ fill: #0f2226; stroke: #3fb8b0; }}
    .box, .key {{ fill: #161b22; }}
    .box, .pill {{ stroke: #30404a; }}
    .key {{ stroke: #3fb8b0; }}
    .pill {{ fill: #161b22; }}
    .warn {{ fill: #2a2112; stroke: #8a6a35; }}
    .t, .p {{ fill: #e6edf3; }}
    .s, .lbl, .sec {{ fill: #9aa7b2; }}
    .brand, .mono {{ fill: #5fd6cc; }}
    .dot, .ha {{ fill: #3fb8b0; }}
    .num {{ fill: #0d1117; }}
    .a {{ stroke: #3fb8b0; }}
    .r {{ stroke: #8b98a2; }} .hr {{ fill: #8b98a2; }}
    .g {{ stroke: #5c6a74; }} .hg {{ fill: #5c6a74; }}
    .life {{ stroke: #30404a; }}
  }}
</style>"""

DEFS = """<defs>
  <marker id="ma" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" class="ha"/></marker>
  <marker id="mr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" class="hr"/></marker>
  <marker id="mg" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" class="hg"/></marker>
</defs>"""


class SVG:
    def __init__(self, w, h, title):
        self.w, self.h, self.title = w, h, title
        self.parts = []

    def add(self, s):
        self.parts.append(s)

    def rect(self, x, y, w, h, cls, rx=8):
        self.add(f'<rect x="{x:.1f}" y="{y:.1f}" width="{w:.1f}" height="{h:.1f}" rx="{rx}" class="{cls}"/>')

    def text(self, x, y, s, cls, anchor="start"):
        self.add(f'<text x="{x:.1f}" y="{y:.1f}" class="{cls}" text-anchor="{anchor}">{escape(s)}</text>')

    def line(self, pts, cls, start=False, end=True):
        m = {"a": "ma", "r": "mr", "g": "mg"}[cls]
        d = "M" + " L".join(f"{x:.1f},{y:.1f}" for x, y in pts)
        ms = (f' marker-start="url(#{m})"' if start else "") + (f' marker-end="url(#{m})"' if end else "")
        self.add(f'<path d="{d}" class="{cls}"{ms}/>')

    def node(self, x, y, w, h, title, subs, cls="box", tcls="t"):
        self.rect(x, y, w, h, cls)
        n = len(subs)
        top = y + h / 2 - (18 * n) / 2 - 2
        self.text(x + w / 2, top + 6, title, tcls, "middle")
        for i, s in enumerate(subs):
            self.text(x + w / 2, top + 26 + i * 17, s, "s", "middle")

    def render(self):
        head = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.w} {self.h}" '
                f'width="{self.w}" height="{self.h}" role="img" aria-label="{escape(self.title)}">')
        return "\n".join([head, f"<title>{escape(self.title)}</title>", STYLE, DEFS,
                          f'<rect width="{self.w}" height="{self.h}" rx="12" class="bg"/>']
                         + self.parts + ["</svg>\n"])


# ---------------------------------------------------------------- overview
OV = {
    "en": dict(
        title="plugrelay architecture overview",
        clients="CLIENTS", clients_note="change the base URL and key, nothing else",
        steps=[("Decode", ["which protocol,", "what the request needs"]),
               ("Plugins", ["auth · limits", "safety · reserve"]),
               ("Route", ["capabilities · sticky", "cooldown · breaker"]),
               ("Send", ["passthrough · patch", "convert"]),
               ("Egress", ["fixed proxy · SSRF", "timeouts"])],
        console=("Console", ["admin API"]),
        store=("Business data", ["SQLite · Postgres · MySQL"]),
        state=("Runtime state", ["memory · SQL · Redis"]),
        prov=("Upstream plugins", ["manifest · process · Go"]),
        edit="writes", snap="read-only snapshot", live="health", how="auth, errors",
        ups="UPSTREAMS", ups_note="108 ready-made manifests; private protocols via external plugins",
        upills=["Vendor APIs", "Clouds", "Inference hosts", "Gateways", "Self-hosted"],
        legend_req="request", legend_resp="streamed back as it arrives: usage, terminal event, sealed state",
    ),
    "zh": dict(
        title="plugrelay 架构总览",
        clients="客户端", clients_note="只改 base URL 和 key",
        steps=[("解码", ["认出协议", "读出请求要用的能力"]),
               ("插件", ["鉴权 · 限流", "内容安全 · 预扣费"]),
               ("选路", ["能力匹配 · 会话粘性", "冷却 · 熔断"]),
               ("发送", ["透传 · 改字段", "转换"]),
               ("出站", ["固定代理 · 防 SSRF", "超时"])],
        console=("控制台", ["管理 API"]),
        store=("业务数据", ["SQLite · PostgreSQL · MySQL"]),
        state=("运行时状态", ["内存 · 数据库 · Redis"]),
        prov=("上游插件", ["YAML 清单 · 外部进程 · Go"]),
        edit="改配置", snap="只读快照", live="健康状态", how="怎么鉴权、怎么判错",
        ups="上游", ups_note="108 份现成清单；私有协议写外部插件",
        upills=["厂商 API", "云平台", "推理服务商", "聚合网关", "自部署"],
        legend_req="请求", legend_resp="响应边收边转：记用量、补结束事件、封装私有状态",
    ),
}


def overview(lang):
    L = OV[lang]
    W, H = 920, 540
    g = SVG(W, H, L["title"])
    # clients
    g.text(20, 28, L["clients"], "sec")
    g.text(20 + (len(L["clients"]) * 13 if lang == "zh" else 70), 28, L["clients_note"], "lbl")
    pills = ["Claude Code", "Codex", "Gemini CLI", "Cursor", "Cline", "OpenCode"]
    pw = (880 - 5 * 12) / 6
    cs = []
    for i, p in enumerate(pills):
        x = 20 + i * (pw + 12)
        g.rect(x, 38, pw, 32, "pill", 16)
        g.text(x + pw / 2, 59, p, "p", "middle")
        cs.append(x + pw / 2)
        g.add(f'<line x1="{x + pw / 2:.1f}" y1="70" x2="{x + pw / 2:.1f}" y2="80" class="g"/>')
    g.add(f'<line x1="{cs[0]:.1f}" y1="80" x2="{cs[-1]:.1f}" y2="80" class="g"/>')

    # frame
    g.rect(20, 92, 880, 316, "frame", 14)
    g.text(884, 114, "plugrelay", "brand", "end")

    bw, gap, by, bh = 145.6, 28, 126, 84
    xs = [40 + i * (bw + gap) for i in range(5)]
    for i, (t, s) in enumerate(L["steps"]):
        g.node(xs[i], by, bw, bh, t, s, "key")
    for i in range(4):
        x0, x1 = xs[i] + bw + 3, xs[i + 1] - 3
        g.line([(x0, by + 30), (x1, by + 30)], "a")
        g.line([(x1, by + 54), (x0, by + 54)], "r")
    c0 = xs[0] + bw / 2
    g.line([(c0 - 8, 80), (c0 - 8, by - 2)], "a")
    g.line([(c0 + 8, by - 2), (c0 + 8, 80)], "r")

    # bottom row
    ry, rh = 300, 76
    A = (40, 124)
    B = (214, 180)
    C = (414, 150)
    D = (590, 170)
    g.node(A[0], ry, A[1], rh, *L["console"])
    g.node(B[0], ry, B[1], rh, *L["store"])
    g.node(C[0], ry, C[1], rh, *L["state"])
    g.node(D[0], ry, D[1], rh, *L["prov"])
    g.line([(A[0] + A[1] + 2, ry + rh / 2), (B[0] - 2, ry + rh / 2)], "g")
    g.text((A[0] + A[1] + B[0]) / 2, ry + rh / 2 - 7, L["edit"], "lbl", "middle")

    def up(x, label):
        g.line([(x, ry - 2), (x, by + bh + 2)], "g")
        g.text(x + 8, (ry + by + bh) / 2 + 4, label, "lbl")

    up(xs[1] + bw / 2 + 10, L["snap"])
    up(xs[2] + bw / 2 + 10, L["live"])
    up(xs[3] + bw / 2, L["how"])

    # upstreams
    c4 = xs[4] + bw / 2
    uy = 444
    g.line([(c4 - 8, by + bh + 2), (c4 - 8, uy - 12)], "a")
    g.line([(c4 + 8, uy - 12), (c4 + 8, by + bh + 2)], "r")
    up_ = L["upills"]
    pw = (880 - 4 * 12) / 5
    cs = []
    for i, p in enumerate(up_):
        x = 20 + i * (pw + 12)
        g.rect(x, uy, pw, 32, "pill", 16)
        g.text(x + pw / 2, uy + 21, p, "p", "middle")
        cs.append(x + pw / 2)
        g.add(f'<line x1="{x + pw / 2:.1f}" y1="{uy - 10}" x2="{x + pw / 2:.1f}" y2="{uy}" class="g"/>')
    g.add(f'<line x1="{cs[0]:.1f}" y1="{uy - 10}" x2="{cs[-1]:.1f}" y2="{uy - 10}" class="g"/>')
    g.text(20, uy + 56, L["ups"], "sec")
    g.text(20 + (len(L["ups"]) * 13 + 10 if lang == "zh" else 90), uy + 56, L["ups_note"], "lbl")

    # legend
    ly = H - 20
    lx = W - 20
    resp_w = sum(12 if ord(c) > 0x2E80 else 6.4 for c in L["legend_resp"])
    req_w = sum(12 if ord(c) > 0x2E80 else 6.4 for c in L["legend_req"])
    x = lx - resp_w
    g.text(x, ly, L["legend_resp"], "lbl")
    g.line([(x - 34, ly - 4), (x - 8, ly - 4)], "r")
    x2 = x - 34 - 22 - req_w
    g.text(x2, ly, L["legend_req"], "lbl")
    g.line([(x2 - 34, ly - 4), (x2 - 8, ly - 4)], "a")
    return g.render()


# ---------------------------------------------------------------- request flow
RF = {
    "en": dict(
        title="One request through plugrelay",
        steps=[
            ("Read", "sniff model, stream flag and session id; decode once into IR to learn what it needs"),
            ("Request plugins", "auth → OnRequest hooks in config order → meter reserves; any of them may reply or reject"),
            ("Plan", "router lists candidates by group, capability, stickiness and health; selector may only reorder"),
            ("Attempt", "OnAttempt → provider.Prepare → egress sends it as passthrough, patch or convert"),
            ("Stream back", "bytes go out as they arrive; a read-only decoder picks out usage, the terminal event and private state"),
            ("Finish", "cost every sent attempt, charge the delivered one, settle or refund once, bind the session, OnFinish in reverse, queue the record"),
        ],
        fail="failed, nothing sent to the client yet",
        verdict="provider.Classify → verdict → cooldown / quarantine / breaker",
        retry="next candidate (up to 4 attempts)",
        ok="ok",
    ),
    "zh": dict(
        title="一次请求在 plugrelay 里怎么走",
        steps=[
            ("读请求", "嗅出模型、是否流式、会话 id；解码一次，得到这个请求要用哪些能力"),
            ("前置插件", "鉴权 → 按配置顺序跑 OnRequest → meter 预扣费；任何一步都可以直接回复或拒绝"),
            ("排候选", "router 按分组、能力、粘性、健康状态排出候选；selector 只能调顺序，不能加"),
            ("尝试", "OnAttempt → provider.Prepare → 出站层按透传、改字段或转换发出去"),
            ("流回客户端", "字节边到边转发；只读解码器顺带取出用量、结束事件和私有状态"),
            ("收尾", "发出去的每次尝试都记成本，只对送达的那次收费；结算或退款一次；绑定会话；OnFinish 倒序；日志异步入队"),
        ],
        fail="失败，且还没给客户端发内容",
        verdict="provider.Classify 给出判定 → 冷却 / 隔离 / 熔断",
        retry="换下一个候选（最多 4 次）",
        ok="成功",
    ),
}


def wrap(s, maxw, fs=12.5):
    """Greedy wrap by estimated width; breaks at spaces for Latin, anywhere for CJK."""
    def cw(c):
        return fs if ord(c) > 0x2E80 else fs * 0.55
    lines, cur, w = [], "", 0.0
    tokens = []
    buf = ""
    for c in s:
        if ord(c) > 0x2E80 or c in "，；、（）":
            if buf:
                tokens.append(buf)
                buf = ""
            tokens.append(c)
        elif c == " ":
            buf += c
            tokens.append(buf)
            buf = ""
        else:
            buf += c
    if buf:
        tokens.append(buf)
    for t in tokens:
        tw = sum(cw(c) for c in t)
        if w + tw > maxw and cur.strip():
            lines.append(cur.rstrip())
            cur, w = t.lstrip(), sum(cw(c) for c in t.lstrip())
        else:
            cur += t
            w += tw
    if cur.strip():
        lines.append(cur.rstrip())
    return lines


def flow(lang):
    L = RF[lang]
    W = 780
    g = SVG(W, 10, L["title"])
    cx = 44
    tx = 74
    textw = 530
    y = 26
    rows = []
    for i, (t, d) in enumerate(L["steps"]):
        lines = wrap(d, textw)
        h = 30 + 17 * len(lines)
        rows.append((y, h, t, lines))
        y += h + (78 if i == 3 else 14)
    H = y + 6
    g.h = H
    # spine
    g.add(f'<line x1="{cx}" y1="{rows[0][0] + 14}" x2="{cx}" y2="{rows[-1][0] + 14}" class="life"/>')
    for i, (ry, rh, t, lines) in enumerate(rows):
        cls = "key" if i == 3 else "box"
        g.rect(tx - 12, ry, textw + 40, rh, cls)
        g.add(f'<circle cx="{cx}" cy="{ry + 15}" r="11" class="dot"/>')
        g.text(cx, ry + 19, str(i + 1), "num", "middle")
        g.text(tx, ry + 20, t, "t")
        for j, ln in enumerate(lines):
            g.text(tx, ry + 38 + j * 17, ln, "s")
    # retry loop on the right of step 4
    ay, ah = rows[3][0], rows[3][1]
    bx = tx - 12 + textw + 40
    fy = ay + ah + 14
    # failure box under attempt
    fx = tx + 30
    fw = textw - 30
    g.rect(fx, fy, fw, 50, "warn")
    g.text(fx + 14, fy + 20, L["fail"], "p")
    g.text(fx + 14, fy + 38, L["verdict"], "s")
    g.line([(fx + 40, ay + ah + 1), (fx + 40, fy - 1)], "g")
    # loop back up on the right
    rx = bx + 30
    g.line([(fx + fw + 1, fy + 25), (rx, fy + 25), (rx, ay + ah / 2), (bx + 3, ay + ah / 2)], "a")
    g.text(rx + 8, (fy + 25 + ay + ah / 2) / 2 - 2, L["retry"].split("（")[0].split(" (")[0], "lbl")
    rest = L["retry"][len(L["retry"].split("（")[0].split(" (")[0]):].strip()
    if rest:
        g.text(rx + 8, (fy + 25 + ay + ah / 2) / 2 + 14, rest, "lbl")
    return g.render()


# ---------------------------------------------------------------- sealing
SE = {
    "en": dict(
        title="Private state sealed across protocols",
        who=["Codex (responses)", "plugrelay", "Upstream A (Anthropic)"],
        t1="turn 1", t2="turn 2",
        m=[(0, 1, "request", "a"), (1, 2, "converted to messages", "a"),
           (2, 1, "thinking block + signature", "r"),
           ("self", 1, "seal {issuer A, data} with AES-256-GCM"),
           (1, 0, "reasoning item, encrypted_content = prs1.…", "r"),
           ("gap",),
           (0, 1, "echoes encrypted_content", "a"),
           ("self", 1, "open, check issuer"),
           (1, 2, "routed to A again: original block + signature", "a"),
           ("else", "otherwise (other issuer, tampered, wrong key): dropped, no error")],
    ),
    "zh": dict(
        title="上游私有状态跨协议加密往返",
        who=["Codex（responses 协议）", "plugrelay", "上游 A（Anthropic）"],
        t1="第 1 轮", t2="第 2 轮",
        m=[(0, 1, "请求", "a"), (1, 2, "转成 messages 协议", "a"),
           (2, 1, "思考块 + 签名", "r"),
           ("self", 1, "用 AES-256-GCM 封装 {签发方 A, 数据}"),
           (1, 0, "reasoning 条目，encrypted_content = prs1.…", "r"),
           ("gap",),
           (0, 1, "原样带回 encrypted_content", "a"),
           ("self", 1, "解开，核对签发方"),
           (1, 2, "又路由到 A：还原原来的思考块和签名", "a"),
           ("else", "否则（换了签发方、被篡改或密钥不对：）：直接丢掉，不报错")],
    ),
}


def seal(lang):
    L = SE[lang]
    W = 820
    cols = [130, 410, 690]
    g = SVG(W, 10, L["title"])
    for x, n in zip(cols, L["who"]):
        g.rect(x - 105, 18, 210, 34, "key" if n == "plugrelay" else "pill", 17)
        g.text(x, 40, n, "p", "middle")
    y = 92
    marks = []
    for m in L["m"]:
        if m[0] == "gap":
            marks.append(("sep", y + 4))
            y += 34
            continue
        if m[0] == "self":
            x = cols[m[1]]
            g.rect(x - 150, y - 16, 300, 26, "box", 6)
            g.text(x, y + 2, m[2], "s", "middle")
            y += 40
            continue
        if m[0] == "else":
            x = cols[1]
            g.rect(x - 70, y - 16, 420, 26, "warn", 6)
            g.text(x - 58, y + 2, m[1], "s")
            y += 36
            continue
        a, b, label, cls = m
        xa, xb = cols[a], cols[b]
        d = 1 if xb > xa else -1
        g.line([(xa + d * 4, y), (xb - d * 4, y)], cls)
        g.text((xa + xb) / 2, y - 7, label, "lbl", "middle")
        y += 38
    H = y + 4
    g.h = H
    # lifelines first (insert at start of parts so they're under everything)
    lines = [f'<line x1="{x}" y1="54" x2="{x}" y2="{H - 14}" class="life"/>' for x in cols]
    g.parts = lines + g.parts
    for kind, sy in marks:
        g.add(f'<line x1="20" y1="{sy}" x2="{W - 20}" y2="{sy}" class="g" stroke-dasharray="2 4"/>')
    g.text(20, 76, L["t1"], "sec")
    g.text(20, marks[0][1] + 22, L["t2"], "sec")
    return g.render()


for lang in ("en", "zh"):
    suf = "" if lang == "en" else ".zh-CN"
    for name, fn in (("overview", overview), ("request-flow", flow), ("sealing", seal)):
        with open(f"{OUT}/{name}{suf}.svg", "w") as f:
            f.write(fn(lang))
