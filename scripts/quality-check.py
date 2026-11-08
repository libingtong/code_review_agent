#!/usr/bin/env python3
"""把一份报告和 known-findings.json 对齐，输出命中/漏报，作为每轮审计的客观分数。

用法：scripts/quality-check.py reports/headless-XXXX/report.json
"""
import json
import sys
from pathlib import Path


def load_findings(report_path):
    data = json.loads(Path(report_path).read_text())
    findings = data.get("findings") or []
    return [(f.get("path", ""), f.get("line", 0), f.get("title", "")) for f in findings]


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    report = Path(sys.argv[1])
    known = json.loads(Path("scripts/known-findings.json").read_text())["items"]
    findings = load_findings(report)
    hits, misses = [], []
    used = {}
    for item in known:
        name = item["path"].split("/")[-1]
        hit = next(
            (
                f
                for f in findings
                if f[0].endswith(name) and abs((f[1] or 0) - item["line"]) <= 20
            ),
            None,
        )
        if hit:
            reused = used.get(id(hit), 0)
            used[id(hit)] = reused + 1
            hits.append((item, hit, reused > 0))
        else:
            misses.append(item)
    total = len(known)
    print(f"报告：{report}")
    print(f"命中 {len(hits)}/{total} · 独立发现 {len({id(h) for _, h, _ in hits})} 条")
    for item, hit, reused in hits:
        note = "  （同一条发现覆盖多项）" if reused else ""
        print(f"  ✅ {item['id']}  {hit[0]}:{hit[1]}  {hit[2][:60]}{note}")
    for item in misses:
        print(f"  ❌ {item['id']}  {item['path']}:{item['line']}  {item['title']}")
    extra = [
        f
        for f in findings
        if not any(f[0].endswith(k["path"].split("/")[-1]) for k in known)
    ]
    if extra:
        print(f"另有 {len(extra)} 条不在基线里的新发现（可能是真的新漏洞，需人工确认）")
        for f in extra[:10]:
            print(f"  ➕ {f[0]}:{f[1]}  {f[2][:60]}")
    return 0 if not misses else 1


if __name__ == "__main__":
    sys.exit(main())
