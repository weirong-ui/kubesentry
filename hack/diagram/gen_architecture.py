#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 KubeStasis 架构图（docs/assets/architecture.png）。

用法：
    python hack/diagram/gen_architecture.py

设计约定：
- 最终画布 1600x980，内部按 SC=2 超采样绘制后降采样，保证文字锐利；
- 浅色主题（GitHub README / 掘金编辑器均可读）；
- 配色语义：蓝=控制面，紫=准入/学习模块，绿=伸缩，橙=自愈/指标。
"""

import math
import os

from PIL import Image, ImageDraw, ImageFont

SC = 2  # 超采样倍数
W, H = 1600, 980

FONT_REG = r"C:\Windows\Fonts\msyh.ttc"
FONT_BOLD = r"C:\Windows\Fonts\msyhbd.ttc"

# ---------- 配色 ----------
INK = "#0F172A"
BODY = "#475569"
MUTED = "#94A3B8"
LINE = "#E2E8F0"

BLUE, BLUE_BG, BLUE_BD = "#2563EB", "#EFF6FF", "#93C5FD"
GREEN, GREEN_BG, GREEN_BD = "#059669", "#ECFDF5", "#6EE7B7"
ORANGE, ORANGE_BG, ORANGE_BD = "#EA580C", "#FFF7ED", "#FDBA74"
PURPLE, PURPLE_BG, PURPLE_BD = "#7C3AED", "#F5F3FF", "#C4B5FD"
SLATE_BG, SLATE_BD = "#F8FAFC", "#CBD5E1"


def S(v):
    return int(round(v * SC))


img = Image.new("RGB", (W * SC, H * SC), "#FFFFFF")
d = ImageDraw.Draw(img)

_font_cache = {}


def font(size, bold=False):
    key = (size, bold)
    if key not in _font_cache:
        _font_cache[key] = ImageFont.truetype(FONT_BOLD if bold else FONT_REG, S(size))
    return _font_cache[key]


def text(x, y, s, size=15, color=INK, bold=False, anchor="la"):
    d.text((S(x), S(y)), s, font=font(size, bold), fill=color, anchor=anchor)


def box(x, y, w, h, fill, outline, width=2, radius=14):
    d.rounded_rectangle([S(x), S(y), S(x + w), S(y + h)], radius=S(radius),
                        fill=fill, outline=outline, width=S(width))


def _round_perimeter(x, y, w, h, r, step=3):
    pts = []

    def line(p0, p1):
        n = max(2, int(math.dist(p0, p1) / step))
        for i in range(n + 1):
            pts.append((p0[0] + (p1[0] - p0[0]) * i / n, p0[1] + (p1[1] - p0[1]) * i / n))

    def arc(cx, cy, a0, a1):
        n = max(3, int(abs(a1 - a0) / 6))
        for i in range(n + 1):
            a = math.radians(a0 + (a1 - a0) * i / n)
            pts.append((cx + r * math.cos(a), cy + r * math.sin(a)))

    line((x + r, y), (x + w - r, y))
    arc(x + w - r, y + r, -90, 0)
    line((x + w, y + r), (x + w, y + h - r))
    arc(x + w - r, y + h - r, 0, 90)
    line((x + w - r, y + h), (x + r, y + h))
    arc(x + r, y + h - r, 90, 180)
    line((x, y + h - r), (x, y + r))
    arc(x + r, y + r, 180, 270)
    return pts


def dashed_box(x, y, w, h, fill, outline, width=2, radius=14, dash=9, gap=6):
    d.rounded_rectangle([S(x), S(y), S(x + w), S(y + h)], radius=S(radius), fill=fill)
    pts = _round_perimeter(x, y, w, h, radius)
    cum = 0.0
    for i in range(1, len(pts)):
        cum += math.dist(pts[i - 1], pts[i])
        if cum % (dash + gap) < dash:
            d.line([S(pts[i - 1][0]), S(pts[i - 1][1]), S(pts[i][0]), S(pts[i][1])],
                   fill=outline, width=S(width))


def arrow(x1, y1, x2, y2, color, width=2, head=10, dashed=False, both=False):
    ang = math.atan2(y2 - y1, x2 - x1)
    bx, by = x2 - head * 0.85 * math.cos(ang), y2 - head * 0.85 * math.sin(ang)
    if dashed:
        cum = 0.0
        for i in range(1, 41):
            px = x1 + (bx - x1) * i / 40
            py = y1 + (by - y1) * i / 40
            qx = x1 + (bx - x1) * (i - 1) / 40
            qy = y1 + (by - y1) * (i - 1) / 40
            cum += math.dist((qx, qy), (px, py))
            if cum % 16 < 9:
                d.line([S(qx), S(qy), S(px), S(py)], fill=color, width=S(width))
    else:
        d.line([S(x1), S(y1), S(bx), S(by)], fill=color, width=S(width))
    hw = head * 0.5
    d.polygon([
        (S(x2), S(y2)),
        (S(bx + hw * math.sin(ang)), S(by - hw * math.cos(ang))),
        (S(bx - hw * math.sin(ang)), S(by + hw * math.cos(ang))),
    ], fill=color)
    if both:
        sx, sy = x1 + head * 0.85 * math.cos(ang), y1 + head * 0.85 * math.sin(ang)
        d.polygon([
            (S(x1), S(y1)),
            (S(sx + hw * math.sin(ang)), S(sy - hw * math.cos(ang))),
            (S(sx - hw * math.sin(ang)), S(sy + hw * math.cos(ang))),
        ], fill=color)


# ---------------- 标题区 ----------------
text(60, 38, "KubeStasis 架构总览", 36, INK, bold=True)
text(62, 92, "Go 1.22 + controller-runtime · 云原生应用自愈与智能弹性伸缩 Operator", 17, BODY)

# 图例
legend = [(BLUE, "控制面"), (PURPLE, "准入 / 学习模块"), (GREEN, "伸缩"), (ORANGE, "自愈 / 指标")]
lx = 1540
for color, label in reversed(legend):
    wlabel = len(label) * 13 + 26
    text(lx, 104, label, 13, BODY, anchor="ra")
    d.ellipse([S(lx - wlabel + 2), S(109), S(lx - wlabel + 14), S(121)], fill=color)
    lx -= (wlabel + 22)

d.line([S(60), S(148), S(1540), S(148)], fill=LINE, width=S(1.5))

# ---------------- 开发者 ----------------
box(50, 360, 170, 110, SLATE_BG, SLATE_BD, radius=14)
text(135, 396, "开发者 / CI", 16, INK, bold=True, anchor="mm")
text(135, 426, "kubectl apply", 13, MUTED, anchor="mm")

arrow(226, 415, 294, 415, BLUE, width=2.5)
text(228, 384, "App CR", 12.5, BLUE, bold=True)

# ---------------- API Server ----------------
box(300, 330, 240, 168, BLUE_BG, BLUE_BD, radius=14)
text(420, 368, "Kubernetes", 18, INK, bold=True, anchor="mm")
text(420, 396, "API Server", 18, INK, bold=True, anchor="mm")
text(420, 434, "etcd · apiserver", 12.5, MUTED, anchor="mm")
text(420, 462, "watch / status 回写", 12.5, MUTED, anchor="mm")

# Admission Webhook
dashed_box(300, 175, 240, 112, PURPLE_BG, PURPLE_BD, radius=12)
text(420, 200, "Admission Webhook", 15, INK, bold=True, anchor="mm")
text(420, 228, "Validating 校验", 12.5, BODY, anchor="mm")
text(420, 252, "Defaulting 安全默认值", 12.5, BODY, anchor="mm")
arrow(420, 322, 420, 292, PURPLE, width=2)
text(430, 306, "准入请求", 11.5, MUTED, anchor="lm")

# ---------------- Operator 容器 ----------------
dashed_box(620, 230, 520, 470, "#FAFBFF", BLUE, radius=18, dash=11, gap=7)
text(648, 250, "KubeStasis Operator", 18, INK, bold=True)
text(1116, 256, "controller-runtime", 11.5, MUTED, anchor="ra")

box(645, 292, 470, 84, "#DEEAFF", BLUE, radius=12)
text(667, 308, "App Controller  ·  Reconcile", 15.5, INK, bold=True)
text(667, 336, "监听 App CR，把期望状态收敛到集群", 12.5, BODY)

box(645, 404, 225, 96, GREEN_BG, GREEN_BD, radius=12)
text(757, 424, "Scaler 伸缩引擎", 15, INK, bold=True, anchor="mm")
text(757, 452, "多信号 · 冷却窗口 · 迟滞", 11.8, BODY, anchor="mm")
text(757, 474, "QPS / P99 / 错误率", 11.2, MUTED, anchor="mm")

box(890, 404, 225, 96, ORANGE_BG, ORANGE_BD, radius=12)
text(1002, 424, "Healer 自愈引擎", 15, INK, bold=True, anchor="mm")
text(1002, 452, "扩容 → 降级 → 回滚", 11.8, BODY, anchor="mm")
text(1002, 474, "金丝雀观察 · 自动回滚", 11.2, MUTED, anchor="mm")

box(645, 524, 470, 68, PURPLE_BG, PURPLE_BD, radius=12)
text(667, 540, "pkg/miniinformer", 14, INK, bold=True)
text(667, 562, "手写 ListWatch → DeltaFIFO → WorkQueue（学习模块）", 11.8, BODY)

dashed_box(645, 610, 470, 60, "#FFFFFF", SLATE_BD, radius=12)
text(667, 626, "Status + Kubernetes Events", 13.5, INK, bold=True)
text(667, 648, "每个决策都留痕：可审计、可回溯", 11.8, MUTED)

# ---------------- 工作负载 ----------------
box(1220, 300, 300, 300, "#FFFFFF", SLATE_BD, radius=16)
text(1370, 328, "Deployment / Service", 16.5, INK, bold=True, anchor="mm")

pod_specs = [(1248, "Pod"), (1332, "Pod"), (1416, "canary")]
for px, label in pod_specs:
    if label == "canary":
        dashed_box(px, 356, 76, 60, ORANGE_BG, ORANGE, radius=10)
        text(px + 38, 378, "Pod", 12, INK, anchor="mm")
        text(px + 38, 398, "canary", 9.5, ORANGE, anchor="mm")
    else:
        box(px, 356, 76, 60, "#F1F5F9", SLATE_BD, radius=10)
        text(px + 38, 386, label, 12, INK, anchor="mm")

text(1370, 448, "滚动更新 · 金丝雀发布", 12.2, MUTED, anchor="mm")
box(1248, 470, 244, 42, BLUE_BG, BLUE_BD, radius=10)
text(1370, 491, "Service (ClusterIP)", 12.5, BLUE, bold=True, anchor="mm")
text(1370, 540, "受管副本 2 → 20", 12, MUTED, anchor="mm")
text(1370, 566, "安全上下文由 Webhook 注入", 11.2, MUTED, anchor="mm")

arrow(1118, 338, 1212, 338, BLUE, width=2.2, both=True)
text(1124, 300, "CreateOrUpdate / watch 回写", 10.8, BLUE, bold=True)

# ---------------- Prometheus ----------------
box(1200, 660, 320, 104, ORANGE_BG, ORANGE_BD, radius=14)
d.ellipse([S(1224), S(680), S(1244), S(700)], fill="#E6522C")
text(1252, 676, "Prometheus", 15, INK, bold=True)
text(1252, 706, "QPS / P99 / 错误率", 12, BODY)
text(1252, 730, "PromQL 查询（带冷却，避免高频拉取）", 10.8, MUTED)

arrow(1204, 690, 1062, 506, ORANGE, width=1.8, dashed=True)
text(1212, 642, "拉取指标", 11, ORANGE, bold=True)

# ---------------- 决策流水线 ----------------
d.line([S(60), S(772), S(1540), S(772)], fill=LINE, width=S(1.5))
text(60, 792, "Reconcile 决策流水线（每一步幂等、可重入）", 15, INK, bold=True)

chips = [
    ("① 读取 App CR", "Informer 缓存读取，不压 API", BLUE_BG, BLUE_BD),
    ("② 金丝雀观察", "错误率超阈值 → 立即回滚", ORANGE_BG, ORANGE_BD),
    ("③ 健康检查", "CrashLoop / OOM / 5xx", SLATE_BG, SLATE_BD),
    ("④ 自愈 / 伸缩决策", "一次只执行一级动作", GREEN_BG, GREEN_BD),
    ("⑤ 写回 Status", "决策留痕，可审计", PURPLE_BG, PURPLE_BD),
]
cx = 60
for i, (title, desc, fill, bd) in enumerate(chips):
    box(cx, 828, 232, 64, fill, bd, radius=12)
    text(cx + 16, 842, title, 13.5, INK, bold=True)
    text(cx + 16, 864, desc, 11, BODY)
    if i < len(chips) - 1:
        arrow(cx + 234, 860, cx + 258, 860, SLATE_BD, width=1.8, head=8)
    cx += 260

text(60, 916, "github.com/weirong-ui/kubestasis", 12, MUTED)
text(1540, 916, "掘金系列 · 手写一个 K8s Operator", 12, MUTED, anchor="ra")

# ---------------- 降采样输出 ----------------
out_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "docs", "assets")
out_dir = os.path.normpath(out_dir)
os.makedirs(out_dir, exist_ok=True)
out_path = os.path.join(out_dir, "architecture.png")

img = img.resize((W, H), Image.LANCZOS)
img.save(out_path, "PNG", optimize=True)
print("saved:", out_path)
