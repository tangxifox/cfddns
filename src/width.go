package main

import (
	"strings"
	"unicode"
)

// padRight 按「终端显示宽度」而不是字节数补齐。
//
// 为什么不能直接用 fmt 的 %-12s：它按字节补齐，而一个汉字是 3 字节但只占 2 列，
// 于是含中文的列（例如状态列的「待更新」）会把整张表推歪。
//
// 宽度规则遵循 Unicode East Asian Width 的常见近似：
//   - 组合字符（Mn/Me）与零宽字符计 0
//   - 宽字符（Han / Hangul / 全角形式 / 常见符号）计 2
//   - 其余计 1
func padRight(s string, w int) string {
	cur := displayWidth(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0 // 控制字符
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0 // 组合字符 / 格式字符（含 ZWJ、变体选择符）
	case isWide(r):
		return 2
	}
	return 1
}

// isWide 判断是否属于「宽」字符区间（覆盖常见的中日韩文本与全角标点）。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo 初声
		return true
	case r >= 0x2E80 && r <= 0x303E: // CJK 部首、标点
		return true
	case r >= 0x3041 && r <= 0x33FF: // 平假名/片假名/注音/兼容字符
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK 扩展 A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 基本区
		return true
	case r >= 0xA000 && r <= 0xA4CF: // 彝文
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // 韩文音节
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容表意
		return true
	case r >= 0xFE10 && r <= 0xFE19: // 竖排标点
		return true
	case r >= 0xFE30 && r <= 0xFE6F: // CJK 兼容形式
		return true
	case r >= 0xFF00 && r <= 0xFF60: // 全角形式
		return true
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true
	case r == 0x2192 || r == 0x2190 || r == 0x21D2: // → ← ⇒ 在多数终端按双宽渲染
		return true
	case r >= 0x1F300 && r <= 0x1F64F: // 常用 emoji
		return true
	case r >= 0x1F900 && r <= 0x1F9FF:
		return true
	case r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B 及以后
		return true
	}
	return false
}

// truncDisplay 按显示宽度截断，超出部分用省略号代替。
func truncDisplay(s string, w int) string {
	if displayWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	out := make([]rune, 0, len(s))
	cur := 0
	for _, r := range s {
		rw := runeWidth(r)
		if cur+rw > w-1 {
			break
		}
		out = append(out, r)
		cur += rw
	}
	return string(out) + "…"
}
