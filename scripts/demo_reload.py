#!/usr/bin/env python3
"""配置热重载（C1）端到端验收：改文件 → 轮询重载 → 观察路由与运行时状态。

用法：
    python scripts/demo_reload.py [配置文件路径] [网关日志路径]

需要先用 GW_CONFIG 指向该配置文件启动网关（脚本会写文件，但不会起进程），例如 PowerShell：
    $env:GW_CONFIG="$env:TEMP\\gw-hot.json"
    Start-Process -FilePath bin\\gateway -RedirectStandardOutput bin\\gw8.log

判定逻辑（关键在第三步）：
    1) 初始配置 hot-svc → 19001，/hot 返回 backend A；
    2) 把 19001 打坏（/__control?fail=1），打到熔断打开（503 circuit_open）；
    3) **改写配置文件**把 hot-svc 换成 19002（健康节点），等热重载；
       重载后立刻请求：**仍然是 503 circuit_open** —— 这说明熔断器状态被保留（AGENTS.md §4 坑①）。
       如果熔断器被重建，这里会打到健康的 19002 并返回 200，脚本会直接判定失败。
    4) 等 OpenFor=8s 冷却 → 半开探测打到 19002 → 200（backend B），说明新上游确实生效了。
    5) 网关日志里应出现「配置已热重载」。
"""
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

BASE = "http://127.0.0.1:18080"
ADMIN = "http://127.0.0.1:18081"
OP = urllib.request.build_opener(urllib.request.ProxyHandler({}))

CONFIG = sys.argv[1] if len(sys.argv) > 1 else os.path.join(tempfile.gettempdir(), "gw-hot.json")
LOG = sys.argv[2] if len(sys.argv) > 2 else r"D:\www\gateway\bin\gw8.log"

RESULTS = []


def check(name, ok, detail):
    RESULTS.append((name, bool(ok), detail))
    print("  %s %-40s %s" % ("PASS" if ok else "FAIL", name, detail))


def http(url, headers=None, timeout=20):
    try:
        with OP.open(urllib.request.Request(url, headers=headers or {}), timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        return 0, json.dumps({"transport_error": str(e)}, ensure_ascii=False)


def write_config(upstream_addr):
    """把 hot-svc 指向指定节点（其余配置保持同样的形态）。"""
    doc = {
        "listen_addr": "127.0.0.1:18080",
        "admin_listen_addr": "127.0.0.1:18081",
        "jwt_secret": "demo-secret-do-not-use-in-prod",
        "api_key": "ak_live_9f2c41d7",
        "services": {
            "self": {"name": "self", "balance": "round_robin", "timeout": "1s"},
            "hot-svc": {
                "name": "hot-svc",
                "balance": "round_robin",
                "upstreams": [{"addr": upstream_addr, "backend": 1 if upstream_addr.endswith("19001") else 2}],
                "timeout": "2s",
                "breaker": {"window_size": 10, "fail_ratio": 0.5, "min_requests": 4,
                            "open_for": "8s", "half_open_max": 1},
            },
        },
        "routes": [
            {"name": "health", "host": "*", "path": "/healthz", "path_type": "exact", "upstream": "self"},
            {"name": "hot", "host": "*", "path": "/hot", "path_type": "exact", "upstream": "hot-svc"},
        ],
    }
    with open(CONFIG, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
    return doc


def backend_of(body):
    try:
        return json.loads(body).get("backend", "?")
    except Exception:
        return "?"


def main():
    print("配置文件: %s\n网关日志: %s\n" % (CONFIG, LOG))

    print("[1] 初始配置：hot-svc → 19001")
    s, body = http(BASE + "/hot")
    check("/hot 200 且来自 backend A", s == 200 and backend_of(body) == "A", "status=%d backend=%s" % (s, backend_of(body)))

    print("\n[2] 打坏 19001，把熔断器打到打开（min_requests=4 / fail_ratio=0.5）")
    http("http://127.0.0.1:19001/__control?fail=1")
    seq = []
    for _ in range(8):
        s, body = http(BASE + "/hot")
        seq.append(s)
        if s == 503 and "circuit_open" in body:
            break
    check("熔断器打开（503 circuit_open）", 503 in seq, "状态码序列=%s" % seq)

    print("\n[3] 改写配置文件：hot-svc → 19002（健康），等轮询热重载（3s 间隔，等 4.5s）")
    write_config("127.0.0.1:19002")
    time.sleep(4.5)
    s, body = http(BASE + "/hot")
    ok = s == 503 and "circuit_open" in body
    check("重载后仍处于熔断（熔断器状态被保留）", ok,
          "status=%d body=%s" % (s, body.strip()[:70]))
    if not ok:
        print("      ↑ 如果这里变成 200，说明熔断器被重建了 —— 这正是 AGENTS.md §4 记录的坑①")

    print("\n[4] 等 OpenFor=8s 冷却 → 半开探测应打到新上游 19002")
    time.sleep(5.0)
    codes, backends = [], []
    for _ in range(3):
        s, body = http(BASE + "/hot")
        codes.append(s)
        backends.append(backend_of(body))
        time.sleep(0.2)
    check("半开后恢复 200 且来自 backend B", 200 in codes and "B" in backends,
          "状态码=%s backend=%s" % (codes, backends))

    print("\n[5] 网关日志里应出现热重载记录")
    try:
        with open(LOG, encoding="utf-8", errors="replace") as f:
            lines = [l for l in f.read().splitlines() if "配置已热重载" in l]
        check("日志有「配置已热重载」", len(lines) > 0, lines[-1] if lines else "（没找到）")
    except FileNotFoundError:
        check("日志有「配置已热重载」", False, "日志文件不存在: %s" % LOG)

    http("http://127.0.0.1:19001/__control?fail=0")
    print("\n（已复位 19001 的故障注入）")

    passed = sum(1 for _n, ok, _d in RESULTS if ok)
    print("\n" + "=" * 62)
    print("热重载验收：%d/%d 项通过" % (passed, len(RESULTS)))
    for name, ok, detail in RESULTS:
        if not ok:
            print("  FAIL %-38s %s" % (name, detail))
    return 0 if passed == len(RESULTS) else 1


if __name__ == "__main__":
    sys.exit(main())
