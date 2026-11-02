#!/usr/bin/env python3
"""
detect_gap.py — 滑块缺口检测（浏览器外，Python PIL）
用法: python3 detect_gap.py <bg.png> <puzzle.png>
输出: JSON {"bestX": int, "W": int, "PW": int, "scores_top5": [...]}
方法: 模板匹配（拼图不透明像素 vs 背景窗口）
"""
import json
import sys
from PIL import Image


def main():
    bg = Image.open(sys.argv[1]).convert('RGB')
    pz = Image.open(sys.argv[2]).convert('RGBA')
    W, H = bg.size
    PW, PH = pz.size
    bgpx = bg.load()
    pzpx = pz.load()

    # 预提取拼图不透明像素
    opaque = []
    for py in range(0, H, 2):
        for px in range(0, PW, 2):
            r, g, b, a = pzpx[px, py]
            if a >= 128:
                opaque.append((px, py, r, g, b))

    scores = []
    for x in range(W - PW + 1):
        diff = 0
        for px, py, r, g, b in opaque:
            br, bg_, bb = bgpx[x + px, py]
            diff += abs(r - br) + abs(g - bg_) + abs(b - bb)
        scores.append((diff / len(opaque), x))
    scores.sort()
    print(json.dumps({
        "bestX": scores[0][1],
        "W": W, "H": H, "PW": PW,
        "bestScore": round(scores[0][0], 1),
        "top5": [(x, round(s, 1)) for s, x in scores[:5]],
    }))


if __name__ == "__main__":
    main()
