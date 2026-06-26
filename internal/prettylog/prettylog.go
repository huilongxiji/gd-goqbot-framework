// Package prettylog 把关键事件渲染成带 ANSI 颜色的边框块日志。
// 颜色按输出是否为终端自动开关：重定向到文件/管道/journald 时不带任何转义符。
package prettylog

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// colorOn 仅当 stderr 是终端时上色；写入文件/管道时关闭，避免文件中出现 ANSI 转义符。
var colorOn = func() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

// ANSI 颜色码
const (
	cReset   = "\x1b[0m"
	cBold    = "\x1b[1m"
	cCyan    = "\x1b[36m"
	cGreen   = "\x1b[32m"
	cRed     = "\x1b[31m"
	cYellow  = "\x1b[33m"
	cMagenta = "\x1b[35m"
	cGray    = "\x1b[90m"
)

// Tone 决定标题与边框配色
type Tone int

const (
	ToneRecv   Tone = iota // 收消息
	ToneSend               // 发消息
	ToneNotice             // 通知事件
	ToneErr                // 失败/错误
)

// toneColor 返回某 Tone 对应的 ANSI 颜色码
func toneColor(t Tone) string {
	switch t {
	case ToneSend:
		return cGreen
	case ToneNotice:
		return cMagenta
	case ToneErr:
		return cRed
	default:
		return cCyan
	}
}

// paint 给文本套上颜色；colorOn=false 或无色时原样返回
func paint(s, color string) string {
	if !colorOn || color == "" {
		return s
	}
	return color + s + cReset
}

// 语义着色辅助：colorOn=false 时原样返回。
// Ok 成功（绿）
func Ok(s string) string { return paint(s, cGreen) }

// Fail 失败（红）
func Fail(s string) string { return paint(s, cRed) }

// Warn 警告（黄）
func Warn(s string) string { return paint(s, cYellow) }

// Dim 次要信息（灰）
func Dim(s string) string { return paint(s, cGray) }

// Key 行键名（灰）
func Key(s string) string { return paint(s, cGray) }

// dispWidth 计算显示宽度：跳过 ANSI 转义序列，CJK/全角字符算 2 列，其余算 1 列。
func dispWidth(s string) int {
	w := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 0x1b { // ESC，跳过到 'm' 为止
			for i < len(rs) && rs[i] != 'm' {
				i++
			}
			continue
		}
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isWide 粗略判定全角/宽字符（覆盖中日韩、全角符号、常用标点）。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK 部首/笔画/标点
		r >= 0x3041 && r <= 0x33FF, // 平假名/片假名/注音/CJK 符号
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
		r >= 0xA000 && r <= 0xA4CF, // 彝文
		r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容
		r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6, // 全角符号
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B+
		return true
	}
	return false
}

// Block 一个边框日志块：顶部带标题，中间若干 key/value 行，底部收口。
type Block struct {
	title string
	tone  Tone
	rows  [][2]string
}

// New 创建一个块，tone 决定标题/边框配色。
func New(title string, tone Tone) *Block {
	return &Block{title: title, tone: tone}
}

// Row 追加一行 key/value（value 可已含语义颜色）。
func (b *Block) Row(key, val string) *Block {
	b.rows = append(b.rows, [2]string{key, val})
	return b
}

// minWidth 边框块最小宽度（列）
const minWidth = 48

// Emit 渲染整块并一次性写出到 stderr（单次 Write，避免被其它日志插断）。
func (b *Block) Emit() {
	fmt.Fprint(os.Stderr, b.render())
}

// render 生成整块文本。
func (b *Block) render() string {
	tc := toneColor(b.tone)
	stamp := time.Now().Format("15:04:05")

	// 标题行明文宽度（含时间戳）：┌─ <title>  <time> ─...
	titlePlain := b.title + "  " + stamp
	topUsed := 3 + dispWidth(titlePlain) + 1 // "┌─ " + title + 尾随空格

	keyCol := 0
	for _, r := range b.rows {
		if w := dispWidth(r[0]); w > keyCol {
			keyCol = w
		}
	}

	width := topUsed
	if width < minWidth {
		width = minWidth
	}
	for _, r := range b.rows {
		if rw := 2 + keyCol + 2 + dispWidth(r[1]); rw > width {
			width = rw
		}
	}

	var sb strings.Builder

	// 顶部
	titleColored := paint(b.title, tc+cBold) + "  " + Dim(stamp)
	fill := width - topUsed
	if fill < 1 {
		fill = 1
	}
	sb.WriteString(paint("┌─ ", tc))
	sb.WriteString(titleColored)
	sb.WriteByte(' ')
	sb.WriteString(paint(strings.Repeat("─", fill), tc))
	sb.WriteByte('\n')

	// 行
	for _, r := range b.rows {
		pad := keyCol - dispWidth(r[0]) + 2
		sb.WriteString(paint("│", tc))
		sb.WriteByte(' ')
		sb.WriteString(Key(r[0]))
		sb.WriteString(strings.Repeat(" ", pad))
		sb.WriteString(r[1])
		sb.WriteByte('\n')
	}

	// 底部
	sb.WriteString(paint("└"+strings.Repeat("─", width-1), tc))
	sb.WriteByte('\n')

	return sb.String()
}
