#!/usr/bin/env python3
"""负载/容量测试工具（纯标准库，不需要 wrk / k6 / ab）。

为什么自己写一个：本机没有 wrk/k6，而 shell 里 `for curl` 循环在 Windows 上每次
进程启动约 200ms，根本压不出速率（见 AGENTS.md §3）。这个脚本用线程池 + keep-alive
连接，能稳定压出几 k RPS，并给出延迟分位与状态码分布。

用法：
    python scripts/loadtest.py --url http://127.0.0.1:18080 --path "/slow?ms=5" -c 20 -d 5
    python scripts/loadtest.py --path /healthz -c 50 -d 10 --header "Host: api.example.com"
    python scripts/loadtest.py --path /api/orders -m POST --json '{"sku":"A100"}' --header "Authorization: Bearer xxx"

输出：请求数、RPS、状态码分布、延迟 p50/p95/p99/max、以及被限流/被熔断的比例。
注意：脚本自身是单进程多线程，压到 1 万 RPS 以上时客户端可能先成为瓶颈 —— 报告里会
同时给出「客户端 CPU 时间」，避免把客户端瓶颈误读成网关瓶颈。
"""
import argparse
import http.client
import json
import statistics
import sys
import threading
import time
import urllib.parse

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass


class Result:
    __slots__ = ("status", "latency_ms", "err")

    def __init__(self, status, latency_ms, err=""):
        self.status = status
        self.latency_ms = latency_ms
        self.err = err


def one_request(conn, method, path, headers, body, timeout):
    """在**复用的连接**上发一次请求；连接坏了返回 (结果, None) 让调用方重连。

    ⚠️ 必须复用连接：每请求新建 TCP 连接时，压测会把本机临时端口（Windows 默认约 1.6 万个）
    连同 TIME_WAIT 一起打满 —— 那时测出来的是"端口耗尽"，不是网关容量。
    本机实测（未复用的旧版）：同一配置三次跑出 297 / 486 / 432 RPS，并发 40 时甚至掉到 15 RPS，
    而 c=80 又能跑出 589 RPS —— 这种自相矛盾的数字全部来自端口耗尽，与网关无关。
    """
    t0 = time.perf_counter()
    try:
        conn.request(method, path, body=body, headers=headers)
        resp = conn.getresponse()
        resp.read()
        return Result(resp.status, (time.perf_counter() - t0) * 1000), conn
    except Exception as e:  # 连接失败/超时/被重置
        try:
            conn.close()
        except Exception:
            pass
        return Result(0, (time.perf_counter() - t0) * 1000, type(e).__name__ + ": " + str(e)[:60]), None


def worker(stop_at, args, headers, body, out, lock, cpu_before):
    local = []
    conn = http.client.HTTPConnection(args.host, args.port, timeout=args.timeout)
    while time.perf_counter() < stop_at:
        res, conn = one_request(conn, args.method, args.path, headers, body, args.timeout)
        local.append(res)
        if conn is None:  # 只有连接真的坏了才重连
            conn = http.client.HTTPConnection(args.host, args.port, timeout=args.timeout)

        # 每 50 个请求汇总一次，避免长时间持锁
        if len(local) >= 50:
            with lock:
                out.extend(local)
            local = []
    try:
        conn.close()
    except Exception:
        pass
    with lock:
        out.extend(local)


def parse_target(url):
    u = urllib.parse.urlsplit(url)
    if u.scheme not in ("http", "https"):
        raise SystemExit("只支持 http/https，收到: %r" % url)
    return u.hostname, u.port or (443 if u.scheme == "https" else 80)


def main():
    ap = argparse.ArgumentParser(description="stdlib 负载测试")
    ap.add_argument("--url", default="http://127.0.0.1:18080", help="目标 base url")
    ap.add_argument("--path", default="/healthz", help="请求路径（可带 query）")
    ap.add_argument("-c", "--concurrency", type=int, default=10)
    ap.add_argument("-d", "--duration", type=float, default=5.0, help="压测时长（秒）")
    ap.add_argument("-m", "--method", default="GET")
    ap.add_argument("--header", action="append", default=[], help="可重复，格式 'K: V'")
    ap.add_argument("--json", default="", help="作为 JSON 请求体发送")
    ap.add_argument("--timeout", type=float, default=10.0)
    args = ap.parse_args()

    args.host, args.port = parse_target(args.url)

    headers = {"Connection": "keep-alive"}
    for h in args.header:
        if ":" not in h:
            raise SystemExit("--header 需要 'K: V' 形式: %r" % h)
        k, v = h.split(":", 1)
        headers[k.strip()] = v.strip()
    body = None
    if args.json:
        body = args.json.encode()
        headers.setdefault("Content-Type", "application/json")
    if body is not None:
        headers["Content-Length"] = str(len(body))

    print("目标 %s%s  并发 %d  时长 %.1fs  方法 %s" %
          (args.url, args.path, args.concurrency, args.duration, args.method))
    if headers:
        print("请求头: %s" % {k: v for k, v in headers.items() if k.lower() != "authorization"})

    results = []
    lock = threading.Lock()
    stop_at = time.perf_counter() + args.duration
    cpu0 = time.process_time()
    threads = [
        threading.Thread(target=worker, args=(stop_at, args, headers, body, results, lock, cpu0), daemon=True)
        for _ in range(args.concurrency)
    ]
    t0 = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    wall = time.perf_counter() - t0
    cpu = time.process_time() - cpu0

    with lock:
        data = list(results)
    if not data:
        print("没有完成任何请求")
        return 1

    status = {}
    for r in data:
        key = str(r.status) if r.status else ("ERR " + r.err.split(":")[0])
        status[key] = status.get(key, 0) + 1
    lat = sorted(r.latency_ms for r in data)

    def pct(p):
        idx = min(len(lat) - 1, int(len(lat) * p))
        return lat[idx]

    print()
    print("完成请求 : %d   墙钟 %.2fs   RPS %.0f" % (len(data), wall, len(data) / wall))
    print("状态码   : %s" % dict(sorted(status.items(), key=lambda kv: -kv[1])))
    print("延迟(ms) : p50 %.1f  p90 %.1f  p95 %.1f  p99 %.1f  max %.1f  平均 %.1f" %
          (pct(0.50), pct(0.90), pct(0.95), pct(0.99), lat[-1], statistics.fmean(lat)))
    print("客户端   : CPU %.2fs（占墙钟 %.0f%%）—— 接近 100%% 时先怀疑客户端是瓶颈" %
          (cpu, 100 * cpu / wall if wall else 0))
    limited = sum(v for k, v in status.items() if k == "429")
    if limited:
        print("被限流   : %d（%.1f%%）" % (limited, 100 * limited / len(data)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
