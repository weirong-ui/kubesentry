#!/usr/bin/env python
"""把 hack/benchmark.sh 采到的 CSV 变成对比表。

所有指标都从原始采样算出来，不手填——读者可以拿 data.csv 复算。
用法：python hack/benchmark_report.py docs/benchmark/data.csv
"""

import csv
import sys
from pathlib import Path

BURST_STARTS = (120, 480)
BURST_END = 600
IDLE1_END = 480
IDLE2_START = 600


def load(path):
    rows = []
    with open(path, newline="", encoding="utf-8") as f:
        for r in csv.DictReader(f):
            rows.append({k: (int(float(v)) if k != "phase" else v) for k, v in r.items()})
    return rows


def series(rows, key):
    return [(r["ts"], r[key]) for r in rows]


def first_above(rows, key, threshold, after):
    """after 秒之后，key 首次 > threshold 的时刻（返回相对 burst 起点的秒数）。"""
    for r in rows:
        if r["ts"] >= after and r[key] > threshold:
            return r["ts"] - after
    return None


def value_near(rows, key, t):
    """t 时刻（附近最近一次采样）key 的值。"""
    best = min(rows, key=lambda r: abs(r["ts"] - t))
    return best[key]


def back_to_peak(rows, key, after):
    """burst 起点后回到峰值副本数的用时；从未离开峰值说明 idle 期间没缩容。"""
    p = peak(rows, key)
    start = value_near(rows, key, after)
    if start >= p:
        return "0（未缩容）"
    for r in rows:
        if r["ts"] >= after and r[key] >= p:
            return f"{r['ts'] - after}s"
    return "未回到峰值"


def peak(rows, key):
    return max(r[key] for r in rows)


def time_at_peak(rows, key, after):
    p = peak(rows, key)
    for r in rows:
        if r["ts"] >= after and r[key] == p:
            return r["ts"] - after
    return None


def changes(rows, key):
    """副本数变更次数（相邻采样不同即计一次）。"""
    n = 0
    prev = None
    for _, v in series(rows, key):
        if prev is not None and v != prev:
            n += 1
        prev = v
    return n


def replica_seconds(rows, key):
    """Σ replicas × 采样间隔（5s），/60 得 replica-minutes。"""
    total = 0
    prev_ts = None
    prev_v = None
    for r in rows:
        if prev_ts is not None:
            total += prev_v * (r["ts"] - prev_ts)
        prev_ts, prev_v = r["ts"], r[key]
    return total / 60.0


def recovery(rows, key, after):
    """burst 结束（600s）后回到 1 副本的时刻（相对 600s 的秒数）。"""
    for r in rows:
        if r["ts"] >= after and r[key] <= 1:
            return r["ts"] - after
    return None


def fmt(v, unit=""):
    return "—" if v is None else f"{v}{unit}"


def main():
    path = Path(sys.argv[1] if len(sys.argv) > 1 else "docs/benchmark/data.csv")
    rows = load(path)
    if not rows:
        sys.exit("empty csv")

    # HPA 用 status.currentReplicas（真实副本），KubeStasis 用 status.currentReplicas
    # （就绪副本，同样来自 Deployment.status.readyReplicas），口径一致。
    sides = [
        ("HPA（CPU 50%）", "hpa_current"),
        ("KubeStasis（QPS 16/Pod）", "ks_ready"),
    ]

    print(f"# 采样：{len(rows)} 行，{rows[0]['ts']}s → {rows[-1]['ts']}s\n")
    print("| 指标 | " + " | ".join(s[0] for s in sides) + " |")
    print("|---|" + "---|" * len(sides))

    def row(name, fn):
        print(f"| {name} | " + " | ".join(fn(k) for _, k in sides) + " |")

    # 扩容响应：突发 #1 起点到首次 >1 副本
    row("扩容响应（突发#1，秒）", lambda k: fmt(first_above(rows, k, 1, BURST_STARTS[0]), "s"))
    row("峰值副本数", lambda k: fmt(peak(rows, k)))
    row("到达峰值用时（突发#1，秒）", lambda k: fmt(time_at_peak(rows, k, BURST_STARTS[0]), "s"))
    row("空闲#1 结束时副本数", lambda k: fmt(value_near(rows, k, IDLE1_END)))
    row("突发#2 回到峰值用时", lambda k: back_to_peak(rows, k, BURST_STARTS[1]))
    row("副本变更次数（全程）", lambda k: fmt(changes(rows, k)))
    row("副本-分钟（成本代理）", lambda k: f"{replica_seconds(rows, k):.1f}")
    row("空闲后回到 1 副本（秒）", lambda k: fmt(recovery(rows, k, IDLE2_START), "s"))

    print("\n## 关键窗口副本数（就近取 30s 刻度的采样）\n")
    print("| t(s) | 阶段 | " + " | ".join(s[0] for s in sides) + " |")
    print("|---|---|" + "---|" * len(sides))
    for tick in range(0, rows[-1]["ts"] + 1, 30):
        best = min(rows, key=lambda r: abs(r["ts"] - tick))
        if abs(best["ts"] - tick) > 7:
            continue
        print(f"| {tick} | {best['phase']} | " + " | ".join(str(best[k]) for _, k in sides) + " |")

    print("\n## 吞吐与 CPU 对照（突发窗口均值）\n")
    print("| 组 | 平均 QPS | 平均 CPU(m) | 每千 QPS 的 CPU 核数 |")
    print("|---|---|---|---|")
    for label, qk, ck in [("HPA", "hpa_qps", "hpa_cpu_m"), ("KubeStasis", "ks_qps", "ks_cpu_m")]:
        win = [r for r in rows if BURST_STARTS[0] + 60 <= r["ts"] <= 360]
        if not win:
            continue
        qps = sum(r[qk] for r in win) / len(win)
        cpu = sum(r[ck] for r in win) / len(win)
        cores = (cpu / 1000.0) / (qps / 1000.0) if qps else 0
        print(f"| {label} | {qps:.0f} | {cpu:.0f} | {cores:.1f} |")


if __name__ == "__main__":
    main()
