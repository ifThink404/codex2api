// Package degradejudge is the Go port of pelican_judge.py, the operators'
// degradation ("降智") judge: a fixed prompt asks the model for a pelican
// riding a bicycle as a self-contained HTML document with inline SVG, and the
// SVG's complexity decides. The threshold was calibrated on 60+ hand-graded
// gpt-6-astra samples at medium reasoning effort: coherent outputs scored
// 196–242, degraded ones 90–178, nothing in 179–195, so score >= 187 is
// "not degraded". Byte size is reported but never used for the verdict (a
// compact but complete output can be ~10.5 KB). Extract and Score reproduce
// the script exactly: same patterns, weights and lowercasing.
package degradejudge

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Prompt is the script's PELICAN prompt, verbatim.
const Prompt = "请用 HTML 画一只鹈鹕骑自行车。要求：\n" +
	"1. 输出一个完整、自包含的 HTML 文档（含 <!DOCTYPE html>，所有样式用内联 <style>，图形用内联 <svg>，不引用任何外部资源/CDN/图片/字体）。\n" +
	"2. 浏览器直接打开即可看到一只鹈鹕骑自行车的彩色图画：鹈鹕要有明显的长喙、喉囊、翅膀、身体和脚，自行车要有两个车轮、车架、车把、座椅和脚踏，鹈鹕坐在车上呈骑行姿态。\n" +
	"3. 画面美观、比例协调、细节丰富，可用渐变、阴影、背景。\n" +
	"4. 只输出 HTML 代码本身，从 <!DOCTYPE html> 开始，到 </html> 结束，不要任何解释文字，不要 markdown 代码围栏。"

// DefaultThreshold is the calibrated verdict threshold (gpt-6-astra, medium).
const DefaultThreshold = 187

// DefaultModel and ReasoningEffort are the calibration conditions.
const (
	DefaultModel    = "gpt-6-astra"
	ReasoningEffort = "medium"
)

// Verdicts.
const (
	VerdictOK       = "ok"       // 不降智
	VerdictDegraded = "degraded" // 降智
	VerdictInvalid  = "invalid"  // 无效样本: no HTML, retry; never counts
)

var (
	fencePattern = regexp.MustCompile("(?s)```(?:html)?\\s*(.*?)```")
	colorPattern = regexp.MustCompile(`(?:fill|stroke)="(#[0-9a-f]{3,6})"`)
)

// asciiLower lowercases ASCII letters only, keeping byte offsets aligned with
// the input (the patterns and markers are ASCII).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Extract takes the HTML document out of the model's text: the first fenced
// block if any, then from the first <!doctype html / <html to the last
// </html> (or to the end when unclosed), trimmed.
func Extract(text string) string {
	if m := fencePattern.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	lo := asciiLower(text)
	start := -1
	for _, marker := range []string{"<!doctype html", "<html"} {
		if i := strings.Index(lo, marker); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	end := strings.LastIndex(lo, "</html>")
	switch {
	case start >= 0 && end >= 0 && end > start:
		text = text[start : end+len("</html>")]
	case start >= 0:
		text = text[start:]
	}
	return strings.TrimSpace(text)
}

// Valid reports whether an extracted sample is an HTML document at all; an
// invalid sample is inconclusive (retry), never degraded.
func Valid(html string) bool {
	return strings.Contains(asciiLower(html), "<html")
}

// Metrics is the complexity of one sample. Bytes is the script's len(html),
// i.e. characters, shown but never used for the verdict.
type Metrics struct {
	Score  int `json:"score"`
	Bytes  int `json:"bytes"`
	Shapes int `json:"shapes"`
	Grads  int `json:"gradients"`
	Filter int `json:"filters"`
	Stops  int `json:"stops"`
	Colors int `json:"colors"`
}

// Score is the script's cx(): shapes + gradients*3 + filters*4 + stops +
// distinct hex fill/stroke colors, counted on the lowercased document.
func Score(html string) Metrics {
	lo := asciiLower(html)
	count := func(p string) int { return strings.Count(lo, p) }
	m := Metrics{
		Shapes: count("<path") + count("<circle") + count("<ellipse") + count("<rect") + count("<line") + count("<polygon") + count("<polyline"),
		Grads:  count("<lineargradient") + count("<radialgradient"),
		Filter: count("<filter"),
		Stops:  count("<stop"),
		Bytes:  utf8.RuneCountInString(html),
	}
	colors := map[string]bool{}
	for _, match := range colorPattern.FindAllStringSubmatch(lo, -1) {
		colors[match[1]] = true
	}
	m.Colors = len(colors)
	m.Score = m.Shapes + m.Grads*3 + m.Filter*4 + m.Stops + m.Colors
	return m
}

// Verdict is ok at or above threshold, degraded below it.
func Verdict(score, threshold int) string {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if score >= threshold {
		return VerdictOK
	}
	return VerdictDegraded
}

// Judge extracts, validates and scores a model's full text output.
func Judge(text string, threshold int) (html string, metrics Metrics, verdict string) {
	html = Extract(text)
	if !Valid(html) {
		return html, Score(html), VerdictInvalid
	}
	metrics = Score(html)
	return html, metrics, Verdict(metrics.Score, threshold)
}
