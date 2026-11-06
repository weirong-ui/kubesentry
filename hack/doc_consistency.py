#!/usr/bin/env python3
"""文档一致性门：同一事实在多份文档里必须只有一个数字。

起因（2026-11-05 外部评审）：README 徽章写 139 用例，同一文件"开发"节还写着
113，STATUS 正文是 139，ROADMAP 的 envtest 已完成但 DESIGN 仍标"计划中"。
都是同步滞后，不是造假——但一个主打"文档即唯一可信账本"的项目，账本自己
对不上就是硬伤。人工核对靠不住（上次就是脚本替换漏了一处），所以做成门。

检查两类：
1. 数字：所有"当前时态"的用例数必须等于徽章数字。
   历史流水账（形如 "103 → 113 用例"）豁免——记录演进是正当的。
2. 现实：仓库里已经存在的东西，文档里不能还说"计划中/未做"。
   这类断言写死映射，改代码时若文档没跟上，CI 直接红。
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
errors: list[str] = []


def rel(p: pathlib.Path) -> str:
    return str(p.relative_to(ROOT))


# ---------- 1. 用例数一致性 ----------

BADGE = re.compile(r"tests-(\d+)%20cases")
# "139 用例" / "**139 个用例**" / "139 cases"
# 2026-11-05 外部评审指出的漏网写法："100 个单元测试用例"——数字和"用例"之间
# 隔着"个单元测试"，旧正则只允许一个"个"，直接放过。单独加一支收口。
CLAIM = re.compile(
    r"(\d+)\s*个\s*(?:单元|集成)?测试用例"      # 100 个单元测试用例
    r"|(\d+)\s*(?:个\s*)?用例"                  # 139 用例 / 139 个用例
    r"|(\d+)\s+cases"                            # 139 cases
)
ARROW_HISTORY = re.compile(
    r"\d+\s*(?:个\s*)?(?:测试)?用例\s*→\s*\d+"       # "113 用例 → 137"
    r"|\d+\s*→\s*\d+\s*(?:个\s*)?(?:测试)?用例"       # "113 → 137 用例"
)
# 带日期的台账行（"| 2026-11-03 | P1 ... 31 个单测 ... |"）陈述的是那天的状态，
# 和箭头演进同理，属于历史记录而非当前声明
DATE_ROW = re.compile(r"^\|\s*\d{4}-\d{2}-\d{2}\s*\|")


def is_history(line: str) -> bool:
    """历史演进行豁免：'103 → 113 用例'、'113 → 137 用例'、日期台账行。

    2026-11-06 外部评审：这里原来是 `or "→" in line`，任何带箭头的行整行豁免，
    像 "139 用例（真集群 1→5→1 验证）" 这种当前时态声明会被静默跳过。
    现在箭头必须紧邻用例计数，1→5 之类的普通箭头不再构成豁免。
    """
    stripped = line.strip()
    return bool(ARROW_HISTORY.search(line)) or bool(DATE_ROW.match(stripped))


def collect_counts() -> tuple[int | None, list[tuple[str, int, int, str]]]:
    badge = None
    claims: list[tuple[str, int, int, str]] = []
    for md in sorted(ROOT.rglob("*.md")):
        # juejin/ 是已对外发布的稿件快照，数字属于"发表时的事实"，不回改
        if any(part in {".git", "vendor", "node_modules", "juejin"} for part in md.parts):
            continue
        for i, line in enumerate(md.read_text(encoding="utf-8").splitlines(), 1):
            b = BADGE.search(line)
            if b:
                badge = int(b.group(1))
            for m in CLAIM.finditer(line):
                if is_history(line):
                    continue
                n = int(m.group(1) or m.group(2) or m.group(3))
                # 压测段落里会出现 "137m"、时间之类的数字，先靠上面正则收窄；
                # 小于 20 的数字基本不是用例总数（如"4 条链路"）
                if n >= 20:
                    claims.append((rel(md), i, n, line.strip()[:100]))
    return badge, claims


badge, claims = collect_counts()
if badge is None:
    errors.append("README 徽章里找不到 tests-N%20cases，无法建立基准")
else:
    for path, line_no, n, text in claims:
        if n != badge:
            errors.append(f"{path}:{line_no} 写 {n} 用例，徽章是 {badge} —— {text}")

# ---------- 2. 现实 vs 文档陈述 ----------

# (产物存在则说明已落地, 文档文件, 不该再出现的说法（正则）)
REALITY = [
    ("test/envtest", "docs/DESIGN.md", r"envtest[^\n]{0,120}(计划中|未实现|未做|⬜)"),
    ("deploy/monitoring/grafana", "docs/STATUS.md", r"Grafana 面板[^\n]{0,60}(未做|计划中|仍未)"),
    ("charts/kubestasis", "docs/ROADMAP.md", r"Helm Chart[^\n]{0,60}未"),
    # README 也写过同一类过期话（2026-11-05：Roadmap 节把已入库的 envtest/Grafana 标"未完成"），
    # "未完成/计划中"可能写在名词前面，两个方向都要查
    ("test/envtest", "README.md", r"(未完成|计划中|未做)[^\n]{0,120}envtest|envtest[^\n]{0,120}(未完成|计划中|未做)"),
    ("deploy/monitoring/grafana", "README.md", r"(未完成|计划中|未做|仍未)[^\n]{0,120}Grafana|Grafana[^\n]{0,120}(未完成|计划中|未做|仍未)"),
]

for artifact, doc, pattern in REALITY:
    if not (ROOT / artifact).exists():
        continue
    p = ROOT / doc
    if not p.exists():
        continue
    for i, line in enumerate(p.read_text(encoding="utf-8").splitlines(), 1):
        if re.search(pattern, line, flags=re.IGNORECASE):
            errors.append(
                f"{doc}:{i} 仍称未完成，但 {artifact} 已存在 —— {line.strip()[:100]}"
            )

if errors:
    print("文档一致性检查失败：")
    for e in errors:
        print("  -", e)
    sys.exit(1)

print(f"文档一致：用例数 {badge}（{len(claims)} 处声明），现实断言 {len(REALITY)} 条")
