// Package prettylog 把关键事件渲染成带 ANSI 颜色的边框块日志。
// 颜色按输出是否为终端自动开关：重定向到文件/管道/journald 时不带任何转义符。
package prettylog

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"goqbot/internal/filelog"
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
		r >= 0x2E80 && r <= 0x303E,   // CJK 部首/笔画/标点
		r >= 0x3041 && r <= 0x33FF,   // 平假名/片假名/注音/CJK 符号
		r >= 0x3400 && r <= 0x4DBF,   // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK 统一表意
		r >= 0xA000 && r <= 0xA4CF,   // 彝文
		r >= 0xAC00 && r <= 0xD7A3,   // 谚文音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容
		r >= 0xFE30 && r <= 0xFE4F,   // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60,   // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B+
		return true
	}
	return false
}

// item 一行键值，或嵌套的子块。
type item struct {
	key, val string
	nest     *Block
}

// Block 一个边框日志块：顶部带标题，中间若干 key/value 行（可嵌套子块），底部收口。
type Block struct {
	title   string
	tone    Tone
	compact bool // 嵌套内框：标题不带时间戳
	items   []item
}

// New 创建一个块，tone 决定标题/边框配色。
func New(title string, tone Tone) *Block {
	return &Block{title: title, tone: tone}
}

// Row 追加一行 key/value（value 可已含语义颜色）。
func (b *Block) Row(key, val string) *Block {
	b.items = append(b.items, item{key: key, val: val})
	return b
}

// Nest 把子块嵌进本框；子块无时间戳，每行前加本框的 │。
func (b *Block) Nest(inner *Block) *Block {
	if inner == nil {
		return b
	}
	inner.compact = true
	b.items = append(b.items, item{nest: inner})
	return b
}

// minWidth 边框块最小宽度（列）
const minWidth = 48

// maxTokenCols 单个不可拆 token 超过此列才加省略号（远大于一行）
const maxTokenCols = 512

// termWidth 终端列数：ioctl / COLUMNS，失败则 80；不低于 minWidth。
func termWidth() int {
	n := ioctlCols()
	if n <= 0 {
		if v := strings.TrimSpace(os.Getenv("COLUMNS")); v != "" {
			if c, err := strconv.Atoi(v); err == nil {
				n = c
			}
		}
	}
	if n <= 0 {
		n = 80
	}
	if n < minWidth {
		return minWidth
	}
	return n
}

// Emit 渲染整块并一次性写出到 stderr（单次 Write，避免被其它日志插断）；同时去色写入本地日志文件。
func (b *Block) Emit() {
	rendered := b.renderWidth(termWidth(), 0)
	fmt.Fprint(os.Stderr, rendered)
	if filelog.Enabled() {
		filelog.WriteFile([]byte(rendered))
	}
}

func (b *Block) renderWidth(termCols, prefixCols int) string {
	avail := termCols - prefixCols
	if avail < minWidth {
		avail = minWidth
	}

	stamp := ""
	title := b.title
	titlePlain := title
	if !b.compact {
		stamp = time.Now().Format("15:04:05")
		titlePlain = title + "  " + stamp
	}
	topUsed := 3 + dispWidth(titlePlain) + 1
	if topUsed > avail {
		keep := avail - 3 - 1
		if !b.compact {
			keep -= 1 + dispWidth(stamp)
		}
		if keep < 8 {
			keep = 8
		}
		title = clipCols(b.title, keep)
		titlePlain = title
		if !b.compact {
			titlePlain = title + "  " + stamp
		}
		topUsed = 3 + dispWidth(titlePlain) + 1
	}

	keyCol := 0
	for _, it := range b.items {
		if it.nest != nil {
			continue
		}
		if w := dispWidth(it.key); w > keyCol {
			keyCol = w
		}
	}

	width := topUsed
	if width < minWidth {
		width = minWidth
	}
	if width > avail {
		width = avail
	}

	valBudget := width - 2 - keyCol - 2
	if valBudget < 8 {
		valBudget = 8
		width = 2 + keyCol + 2 + valBudget
		if width > avail {
			width = avail
			valBudget = width - 2 - keyCol - 2
			if valBudget < 4 {
				valBudget = 4
			}
		}
	}

	tc := toneColor(b.tone)
	var sb strings.Builder

	titleColored := paint(title, tc+cBold)
	if !b.compact {
		titleColored += "  " + Dim(stamp)
	}
	fill := width - topUsed
	if fill < 1 {
		fill = 1
	}
	sb.WriteString(paint("┌─ ", tc))
	sb.WriteString(titleColored)
	sb.WriteByte(' ')
	sb.WriteString(paint(strings.Repeat("─", fill), tc))
	sb.WriteByte('\n')

	bar := paint("│", tc)
	for _, it := range b.items {
		if it.nest != nil {
			inner := it.nest.renderWidth(termCols, prefixCols+2)
			for _, line := range strings.Split(strings.TrimSuffix(inner, "\n"), "\n") {
				sb.WriteString(bar)
				sb.WriteByte(' ')
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
			continue
		}
		lines := wrapEven(it.val, valBudget)
		if len(lines) == 0 {
			lines = []string{""}
		}
		for i, ln := range lines {
			sb.WriteString(bar)
			sb.WriteByte(' ')
			if i == 0 {
				sb.WriteString(Key(it.key))
				sb.WriteString(strings.Repeat(" ", keyCol-dispWidth(it.key)+2))
			} else {
				sb.WriteString(strings.Repeat(" ", keyCol+2))
			}
			sb.WriteString(ln)
			sb.WriteByte('\n')
		}
	}

	sb.WriteString(paint("└"+strings.Repeat("─", width-1), tc))
	sb.WriteByte('\n')
	return sb.String()
}

// clipCols 按显示列截断并加省略号。
func clipCols(s string, cols int) string {
	if dispWidth(s) <= cols {
		return s
	}
	if cols <= 1 {
		return "…"
	}
	parts := hardWrap(s, cols-1)
	if len(parts) == 0 {
		return "…"
	}
	return parts[0] + "…"
}

// wrapEven 把 s 均分到不超过 maxCol 的若干行；优先在 → / ， / , / 空格切开。
func wrapEven(s string, maxCol int) []string {
	if maxCol < 4 {
		maxCol = 4
	}
	if s == "" {
		return []string{""}
	}
	if dispWidth(s) <= maxCol {
		return []string{s}
	}
	raw := splitTokens(s)
	var pieces []string
	for _, t := range raw {
		if dispWidth(t) <= maxCol {
			pieces = append(pieces, t)
			continue
		}
		if dispWidth(t) > maxTokenCols {
			t = clipCols(t, maxTokenCols)
		}
		pieces = append(pieces, hardWrap(t, maxCol)...)
	}
	total := 0
	for _, p := range pieces {
		total += dispWidth(p)
	}
	n := (total + maxCol - 1) / maxCol
	if n < 1 {
		n = 1
	}
	target := (total + n - 1) / n
	if target > maxCol {
		target = maxCol
	}
	if target < 1 {
		target = 1
	}

	var lines []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		lines = append(lines, strings.TrimRight(cur.String(), " "))
		cur.Reset()
		curW = 0
	}
	for _, p := range pieces {
		pw := dispWidth(p)
		if curW > 0 && (curW+pw > maxCol || curW >= target) {
			flush()
		}
		cur.WriteString(p)
		curW += pw
	}
	flush()
	if len(lines) == 0 {
		return []string{s}
	}
	return lines
}

func splitTokens(s string) []string {
	rs := []rune(s)
	var toks []string
	var buf strings.Builder
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		toks = append(toks, buf.String())
		buf.Reset()
	}
	for i := 0; i < len(rs); i++ {
		if rs[i] == 0x1b {
			j := i
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				buf.WriteString(string(rs[i : j+1]))
				i = j
			}
			continue
		}
		r := rs[i]
		if r == '→' || r == '，' || r == ',' {
			flush()
			buf.WriteRune(r)
			continue
		}
		buf.WriteRune(r)
		if r == ' ' {
			flush()
		}
	}
	flush()
	return toks
}

func hardWrap(s string, maxCol int) []string {
	if maxCol < 1 {
		maxCol = 1
	}
	rs := []rune(s)
	var lines []string
	for len(rs) > 0 {
		w := 0
		i := 0
		for i < len(rs) {
			if rs[i] == 0x1b {
				j := i
				for j < len(rs) && rs[j] != 'm' {
					j++
				}
				if j < len(rs) {
					i = j + 1
					continue
				}
			}
			cw := 1
			if isWide(rs[i]) {
				cw = 2
			}
			if w+cw > maxCol && w > 0 {
				break
			}
			w += cw
			i++
		}
		if i == 0 {
			i = 1
		}
		lines = append(lines, string(rs[:i]))
		rs = rs[i:]
	}
	return lines
}
