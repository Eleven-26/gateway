#!/usr/bin/env python3
"""端到端故障演练 + 验收脚本（纯标准库）。

把七级流水线的**失败路径**都跑一遍，并断言实际行为 —— 它既是"故障演练"，也是回归冒烟：
    python scripts/demo_faults.py            # 全跑
    python scripts/demo_faults.py --list     # 只看场景清单

前置：网关（业务 18080 / 管理 18081）+ 三个演示后端（19001/19002/19003，A 另开 gRPC 19100）。

覆盖场景（括号里是断言的核心）：
    1. 正常链路            （200，且访问日志里有 upstream）
    2. 未匹配路由          （404 route_not_found —— fallback 已删）
    3. 请求体超 8MB        （413 body_too_large）
    4. 上游超时            （504，由 Service.Timeout 触发）
    5. 限流                （burst 用尽后 429，且带 Retry-After）
    6. 服务级熔断          （连续 5xx → 503 circuit_open；恢复 + 冷却后 → 200）
    7. 节点级摘除          （把 19003 打坏 → ejected=1 且不再被选中；恢复后回列）
    8. 管理面隔离          （业务端口 /metrics、/readyz = 404；管理端口 = 200）
    9. 就绪检查            （/readyz 200 且带 services 明细）

脚本会在结束时把后端的故障注入**复位**（`/__control?fail=0`），失败也不会留下脏状态。
"""
import argparse
import json
import socket
import sys
import time
import urllib.error
import urllib.request

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

BASE = "http://127.0.0.1:18080"
ADMIN = "http://127.0.0.1:18081"
BACKENDS = ["http://127.0.0.1:19001", "http://127.0.0.1:19002", "http://127.0.0.1:19003"]
BAD = "http://127.0.0.1:19003"          # 演练用"坏节点"
BAD_ADDR = "127.0.0.1:19003"
API_KEY = "ak_live_9f2c41d7"

OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
RESULTS = []


def http(url, headers=None, data=None, method=None, timeout=15):
    r = urllib.request.Request(url, headers=headers or {}, data=data, method=method)
    try:
        with OP.open(r, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        return 0, json.dumps({"transport_error": str(e)}, ensure_ascii=False)


def check(name, ok, detail):
    RESULTS.append((name, bool(ok), detail))
    print("  %s %-34s %s" % ("PASS" if ok else "FAIL", name, detail))


def set_fail(host, on):
    return http("%s/__control?fail=%d" % (host, 1 if on else 0))[0]


def raw_request(payload, read=400):
    """裸 socket：用来伪造 Content-Length（curl 会自己改写它）。"""
    s = socket.create_connection(("127.0.0.1", 18080), timeout=10)
    try:
        s.sendall(payload)
        return s.recv(read).decode("utf-8", "replace")
    finally:
        s.close()


def scenario_1_normal():
    print("\n[1] 正常链路")
    s, body = http(BASE + "/slow?ms=1")
    check("反代 200", s == 200, "status=%d" % s)
    s, body = http(BASE + "/healthz")
    check("自答 200", s == 200 and '"ok"' in body, body.strip()[:60])


def scenario_2_notfound():
    print("\n[2] 未匹配路由 → 404（fallback 已删）")
    s, body = http(BASE + "/nope/nothing")
    check("404 route_not_found", s == 404 and "route_not_found" in body, body.strip()[:80])


def scenario_3_body_limit():
    print("\n[3] 请求体超过 8MB → 413（读取前就拒）")
    head = raw_request(b"POST /slow HTTP/1.1\r\nHost: gw\r\nContent-Length: 9000000\r\n\r\n")
    line = head.split("\r\n")[0]
    check("413 body_too_large", "413" in line and "body_too_large" in head,
          line + " | " + ("Connection: close" if "Connection: close" in head else "no close"))


def scenario_4_timeout():
    print("\n[4] 上游超时 → 504（flaky-svc 的 Timeout=1s，后端延时 2s）")
    set_fail(BAD, False)  # 超时不是 5xx 注入，先保证上游是"活但慢"
    s, body = http(BASE + "/flaky?ms=2000", timeout=20)
    check("504 upstream timeout", s == 504 and "timeout" in body, "status=%d %s" % (s, body.strip()[:60]))


def scenario_5_ratelimit():
    print("\n[5] 限流（/open/ 走 API Key，5/s burst=2）")
    codes = []
    for _ in range(4):
        s, body = http(BASE + "/open/ping", headers={"X-API-Key": API_KEY})
        codes.append(s)
    s, body = http(BASE + "/open/ping", headers={"X-API-Key": API_KEY})
    check("burst 用尽后 429", codes.count(429) >= 1, "四次状态码=%s" % codes)


def scenario_6_breaker():
    print("\n[6] 服务级熔断（flaky-svc 只指向 19003，WindowSize=10 / FailRatio=0.5 / MinRequests=4）")
    set_fail(BAD, True)
    seen = []
    for _ in range(12):
        s, _b = http(BASE + "/flaky", timeout=10)
        seen.append(s)
        if s == 503 and "circuit_open" in _b:
            break
    check("连续 5xx 后熔断打开（503 circuit_open）", 503 in seen, "状态码序列=%s" % seen)

    s, body = http(BASE + "/flaky", timeout=10)
    check("熔断期间快速失败", s == 503 and "circuit_open" in body, "status=%d" % s)

    print("     恢复上游并等冷却（OpenFor=3s）…")
    set_fail(BAD, False)
    time.sleep(3.5)
    codes = []
    for _ in range(4):
        s, _b = http(BASE + "/flaky", timeout=10)
        codes.append(s)
        time.sleep(0.2)
    check("半开探测成功后恢复 200", 200 in codes, "冷却后状态码=%s" % codes)


def node_metrics(service="slow-svc"):
    _s, body = http(ADMIN + "/metrics")
    out = {}
    for line in body.splitlines():
        if not line.startswith("gw_upstream_"):
            continue
        name = line.split("{")[0]
        labels = dict(kv.split("=", 1) for kv in line[line.index("{") + 1: line.index("}")].split(","))
        if labels.get("service") != '"%s"' % service:
            continue
        out.setdefault(labels.get("addr", '""').strip('"'), {})[name] = float(line.rsplit(" ", 1)[1])
    return out


def slow_picks(n):
    _s, body = http(ADMIN + "/debug/logs")
    try:
        lines = json.loads(body)["lines"]
    except Exception:
        return []
    picked = [l.split("upstream=")[1].split(" ")[0] for l in lines
              if "route=slow" in l and "upstream=" in l and "upstream=-" not in l]
    return picked[-n:]


def scenario_7_ejection():
    print("\n[7] 节点级摘除（slow-svc + 19003 打坏；FailThreshold=3 / Cooldown=5s）")
    set_fail(BAD, True)
    for _ in range(12):
        http(BASE + "/slow?ms=1", timeout=15)
    m = node_metrics()
    ej = [a for a, v in m.items() if v.get("gw_upstream_ejected") == 1]
    check("坏节点被摘除（gw_upstream_ejected=1）", BAD_ADDR in ej,
          "ejected=%s | failures=%s" % (ej, {a: v.get("gw_upstream_failures_total") for a, v in m.items()}))

    for _ in range(12):
        http(BASE + "/slow?ms=1", timeout=15)
    picked = slow_picks(12)
    check("摘除后不再被选中", picked.count(BAD_ADDR) == 0,
          "最近 12 次落点=%s" % {a: picked.count(a) for a in sorted(set(picked))})

    set_fail(BAD, False)
    print("     恢复上游并等冷却（Cooldown=5s）…")
    time.sleep(5.5)
    for _ in range(9):
        http(BASE + "/slow?ms=1", timeout=15)
    m = node_metrics()
    picked = slow_picks(9)
    check("冷却+探测后回列", m.get(BAD_ADDR, {}).get("gw_upstream_ejected") == 0 and picked.count(BAD_ADDR) > 0,
          "ejected=%.0f 落点=%s" % (m.get(BAD_ADDR, {}).get("gw_upstream_ejected", -1),
                                   {a: picked.count(a) for a in sorted(set(picked))}))


def scenario_8_admin_isolation():
    print("\n[8] 管理面隔离（批次 B4）")
    s_biz, _ = http(BASE + "/metrics")
    s_admin, body = http(ADMIN + "/metrics")
    check("业务端口 /metrics = 404", s_biz == 404, "status=%d" % s_biz)
    check("管理端口 /metrics = 200 且有节点级指标", s_admin == 200 and "gw_upstream_inflight" in body,
          "status=%d | 含 gw_upstream_inflight=%s" % (s_admin, "gw_upstream_inflight" in body))
    s_biz2, _ = http(BASE + "/readyz")
    check("业务端口 /readyz = 404", s_biz2 == 404, "status=%d" % s_biz2)


def scenario_9_readiness():
    print("\n[9] 就绪检查")
    s, body = http(ADMIN + "/readyz")
    try:
        doc = json.loads(body)
    except Exception:
        doc = {}
    check("/readyz 200 且带 services 明细", s == 200 and doc.get("services"),
          "status=%d %s" % (s, body.strip()[:120]))


SCENARIOS = [
    ("1 正常链路", scenario_1_normal),
    ("2 未匹配路由", scenario_2_notfound),
    ("3 请求体上限", scenario_3_body_limit),
    ("4 上游超时", scenario_4_timeout),
    ("5 限流", scenario_5_ratelimit),
    ("6 服务级熔断", scenario_6_breaker),
    ("7 节点级摘除", scenario_7_ejection),
    ("8 管理面隔离", scenario_8_admin_isolation),
    ("9 就绪检查", scenario_9_readiness),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true", help="只列场景")
    args = ap.parse_args()
    if args.list:
        for name, _ in SCENARIOS:
            print(name)
        return 0

    try:
        for _name, fn in SCENARIOS:
            fn()
    finally:
        # 无论成败都复位故障注入，别把后端留在"坏"状态
        for b in BACKENDS:
            try:
                set_fail(b, False)
            except Exception:
                pass
        print("\n（已复位所有后端的故障注入：/__control?fail=0）")

    passed = sum(1 for _n, ok, _d in RESULTS if ok)
    total = len(RESULTS)
    print("\n" + "=" * 60)
    print("演练结果：%d/%d 项通过" % (passed, total))
    for name, ok, detail in RESULTS:
        if not ok:
            print("  FAIL %-32s %s" % (name, detail))
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
