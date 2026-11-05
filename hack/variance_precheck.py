#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""方差预检：从三轮压测 CSV 的 HPA 侧提取同指标，估算组内噪声。

背景：run1（对称迟滞带 bug）、run4（渐进缩容跨步 bug）、run5（修复后）三轮里，
bug 都只影响 KubeStasis 侧，HPA 侧配置（同镜像、CPU 50%、1..5 副本、同一时间轴）
从头到尾没变过，因此 HPA 侧构成同一配置下的 3 次准重复——可以直接拿来量噪声底。

口径与 hack/benchmark_report.py 完全一致：
- 扩容响应 = 突发#1 起点（120s）后 hpa_current 首次 > 1 的间隔
- 到峰值 = 突发起点后首次达到全程最大副本数
- 副本-分钟 = 真实采样间隔的阶梯积分 / 60（不是固定 5s）

注意：这 3 次"重复"不纯净——三轮里 KubeStasis 侧行为不同，其副本数不同，
对 HPA 侧造成的 CPU 争抢也不同。σ 里混着组间污染，只能作预检量级参考，
不能当正式方差。正式重复实验见压测扩项方案（串行 + R 轮）。

用法：
    python hack/variance_precheck.py
"""
import csv
import statistics as st
import sys

BURST1_START = 120

FILES = {
    "run1": "docs/benchmark/data-run1-symmetric-hysteresis.csv",
    "run4": "docs/benchmark/data-run4-double-step-race.csv",
    "run5": "docs/benchmark/data.csv",
}


def load(path):
    rows = []
    with open(path, encoding="utf-8") as f:
        for r in csv.DictReader(f):
            rows.append({"ts": int(r["ts"]), "hpa_current": int(r["hpa_current"])})
    return rows


def scaleup_response(rows, burst_start=BURST1_START):
    """突发起点后首次 >1 副本的间隔（秒）。"""
    for r in rows:
        if r["ts"] >= burst_start and r["hpa_current"] > 1:
            return r["ts"] - burst_start
    return None


def time_at_peak(rows, burst_start=BURST1_START):
    """突发起点后首次到达全程峰值副本数（秒）。"""
    peak = max(r["hpa_current"] for r in rows)
    for r in rows:
        if r["ts"] >= burst_start and r["hpa_current"] >= peak:
            return r["ts"] - burst_start
    return None


def replica_minutes(rows):
    """Σ replicas × 实际采样间隔 / 60（与 benchmark_report.replica_seconds 同口径）。"""
    total = 0
    prev_ts = prev_v = None
    for r in rows:
        if prev_ts is not None:
            total += prev_v * (r["ts"] - prev_ts)
        prev_ts, prev_v = r["ts"], r["hpa_current"]
    return total / 60.0


def main():
    metrics = {"扩容响应(s)": scaleup_response, "到峰值(s)": time_at_peak,
               "副本-分钟": replica_minutes}
    cols = list(FILES)
    data = {name: {m: fn(load(path)) for m, fn in metrics.items()}
            for name, path in FILES.items()}

    print(f"{'指标':<12}" + "".join(f"{c:>10}" for c in cols)
          + f"{'均值':>10}{'σ':>8}{'极差':>8}")
    for m in metrics:
        vals = [data[c][m] for c in cols]
        print(f"{m:<12}" + "".join(f"{v:>10.1f}" for v in vals)
              + f"{st.mean(vals):>10.1f}{st.stdev(vals):>8.2f}{max(vals)-min(vals):>8.1f}")

    print()
    resp = [data[c]["扩容响应(s)"] for c in cols]
    sigma = st.stdev(resp)
    print(f"HPA 侧扩容响应组内 σ ≈ {sigma:.1f}s（n=3，仅作量级参考）")
    print("参照：报告主张的组间差异（KS 23s vs HPA 40s）= 17s，"
          f"效应量 ≈ {17/sigma:.1f}σ——与单次组内波动同量级。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
