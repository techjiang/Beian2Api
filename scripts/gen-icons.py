#!/usr/bin/env python3
"""生成 Beian2Api 的 PNG 图标（纯标准库，无需 Pillow）。

图形语言：圆角方形渐变底 + 白色对勾（"已备案"）+ 右下角青蓝色校验徽章
（"数据可信"）。用 SDF 解析式 + 超采样做抗锯齿，任意尺寸都锐利。
"""
import math
import struct
import sys
import zlib


def clamp(v, a=0.0, b=1.0):
    return a if v < a else (b if v > b else v)


def mix(a, b, t):
    return a + (b - a) * t


def gradient(t):
    """#3B82F6 -> #6366F1 -> #8B5CF6"""
    t = clamp(t)
    if t < 0.55:
        k, c1, c2 = t / 0.55, (59, 130, 246), (99, 102, 241)
    else:
        k, c1, c2 = (t - 0.55) / 0.45, (99, 102, 241), (139, 92, 246)
    return tuple(mix(c1[i], c2[i], k) for i in range(3))


def sd_round_rect(px, py, cx, cy, hw, hh, r):
    qx = abs(px - cx) - (hw - r)
    qy = abs(py - cy) - (hh - r)
    ax, ay = max(qx, 0.0), max(qy, 0.0)
    return math.hypot(ax, ay) + min(max(qx, qy), 0.0) - r


def sd_segment(px, py, ax, ay, bx, by):
    vx, vy = bx - ax, by - ay
    ll = vx * vx + vy * vy
    t = clamp(((px - ax) * vx + (py - ay) * vy) / ll) if ll else 0.0
    return math.hypot(px - (ax + t * vx), py - (ay + t * vy))


def render(size, ss=4):
    # 设计稿按 96x96 网格描述，渲染时等比放大。
    k = size / 96.0

    def P(v):
        return v * k

    cx = cy = (size - 1) / 2.0
    half = P(38.0)
    corner = P(20.0)

    # 对勾：左下折点 + 右上端点
    c1 = (P(28.0), P(50.0))
    c2 = (P(40.0), P(62.0))
    c3 = (P(68.0), P(34.0))
    stroke = P(7.0) / 2.0

    # 校验徽章
    bx, by, br = P(70.0), P(70.5), P(15.0)
    ring = P(3.0)
    t1, t2, t3 = (P(65.0), P(70.5)), (P(68.5), P(74.0)), (P(75.0), P(66.0))
    tstroke = P(2.4) / 2.0

    px_rows = []
    for y in range(size):
        row = bytearray()
        for x in range(size):
            acc_r = acc_g = acc_b = acc_a = 0.0
            for sy in range(ss):
                for sx in range(ss):
                    px = x + (sx + 0.5) / ss
                    py = y + (sy + 0.5) / ss

                    d = sd_round_rect(px, py, cx, cy, half, half, corner)
                    cov = clamp(0.5 - d)
                    if cov <= 0:
                        continue

                    t = (px + py) / (2.0 * size)
                    col = gradient(t)

                    # 内描边：一圈半透明白，让底板有层次
                    inner = abs(d + P(4.6)) - P(1.2)
                    if inner < 0:
                        col = tuple(mix(col[i], 255, 0.26 * clamp(-inner / P(1.2))) for i in range(3))

                    # 对勾
                    dd = min(sd_segment(px, py, *c1, *c2), sd_segment(px, py, *c2, *c3))
                    ck = clamp((stroke - dd) / max(k * 1.2, 1e-6))
                    if ck > 0:
                        col = tuple(mix(col[i], 249, ck) for i in range(3))

                    # 徽章：白环 + 深青 + 亮青 + 白色小勾
                    db = math.hypot(px - bx, py - by)
                    if db <= br:
                        col = (255, 255, 255)
                    if db <= br - ring:
                        col = (2, 132, 199)
                    if db <= br - ring - P(1.4):
                        col = (14, 165, 233)
                        ds = min(sd_segment(px, py, *t1, *t2), sd_segment(px, py, *t2, *t3))
                        col = tuple(mix(col[i], 255, clamp((tstroke - ds) / max(k * 0.8, 1e-6))) for i in range(3))

                    acc_r += col[0]
                    acc_g += col[1]
                    acc_b += col[2]
                    acc_a += cov

            n = ss * ss
            if acc_a > 0:
                r = int(clamp(acc_r / acc_a, 0, 255))
                g = int(clamp(acc_g / acc_a, 0, 255))
                b = int(clamp(acc_b / acc_a, 0, 255))
                a = int(clamp(255.0 * acc_a / n, 0, 255))
                row += bytes((r, g, b, a))
            else:
                row += b"\x00\x00\x00\x00"
        px_rows.append(bytes(row))

    raw = b"".join(b"\x00" + r for r in px_rows)

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw, 9))
        + chunk(b"IEND", b"")
    )


def main():
    targets = sys.argv[1:] or ["180"]
    for spec in targets:
        path, size = (spec.split(":") + ["512"])[:2] if ":" in spec else (spec, "512")
        data = render(int(size))
        with open(path, "wb") as f:
            f.write(data)
        print(f"{path} ({size}x{size}, {len(data)} bytes)")


if __name__ == "__main__":
    main()
