package ui

import (
	"fmt"
	"strings"
	"unicode"

	"cablecheck/internal/model"
)

type boxLine struct {
	text        string
	health      model.HealthClass
	severity    model.Severity
	hasSeverity bool
	sgr         string
}

// writeBox writes a complete, newline-terminated callout. Fixed-width boxes
// wrap without dropping runes; expanding boxes preserve copyable command lines.
// The caller must hold r.mu.
func (r *Renderer) writeBox(lines []boxLine, expand bool) {
	width := r.width
	if expand {
		for _, line := range lines {
			width = max(width, runeLen(line.text)+4)
		}
	}
	contentWidth := max(1, width-4)
	border := "+" + strings.Repeat("-", contentWidth+2) + "+"
	styledBorder := colorize(border, "\x1b[36m", r.color)
	fmt.Fprintln(r.w, styledBorder)
	for _, line := range lines {
		chunks := []string{line.text}
		if !expand {
			chunks = splitRunes(line.text, contentWidth)
		}
		for _, chunk := range chunks {
			padding := strings.Repeat(" ", max(0, contentWidth-runeLen(chunk)))
			fmt.Fprintf(r.w, "%s %s%s %s\n",
				colorize("|", "\x1b[36m", r.color),
				styleBoxLine(chunk, line, r.color), padding,
				colorize("|", "\x1b[36m", r.color))
		}
	}
	fmt.Fprintln(r.w, styledBorder)
}

func styleBoxLine(text string, line boxLine, enabled bool) string {
	switch {
	case line.health != "":
		return HealthColor(line.health, text, enabled)
	case line.hasSeverity:
		return SeverityColor(line.severity, text, enabled)
	case line.sgr != "":
		return colorize(text, line.sgr, enabled)
	default:
		return text
	}
}

func splitRunes(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	if width < 1 {
		width = 1
	}
	runes := []rune(text)
	chunks := make([]string, 0, (len(runes)+width-1)/width)
	for len(runes) > 0 {
		if len(runes) <= width {
			chunks = append(chunks, string(runes))
			break
		}
		cut, consume := width, width
		for i := width; i > 0; i-- {
			if unicode.IsSpace(runes[i-1]) {
				cut, consume = i-1, i
				break
			}
		}
		if cut == 0 { // leading whitespace cannot form a useful line
			cut, consume = width, width
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[consume:]
	}
	return chunks
}

// TokenBanner displays the coordinator token and a complete PC2 command. The
// box expands rather than altering the command, keeping it directly copyable.
func (r *Renderer) TokenBanner(token, pc2Cmd string, generated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	suffix := ""
	if generated {
		suffix = "  (auto-generated)"
	}
	r.writeBox([]boxLine{
		{text: "CableCheck session", sgr: "\x1b[1;36m"},
		{text: "Session token: " + token + suffix, sgr: "\x1b[33m"},
		{text: "On PC2 run:"},
		{text: pc2Cmd, sgr: "\x1b[36m"},
	}, true)
}

// WorkerSummary preserves PC2's compact completion output while coloring the
// coordinator verdict. PC2 cannot render the full summary because it receives
// protocol.Complete, not the evaluated model.Report.
func (r *Renderer) WorkerSummary(class model.HealthClass, verdictLine, path string, transferred bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	verdictLine = HealthColor(class, verdictLine, r.color)
	if transferred {
		fmt.Fprintf(r.w, "\n%s\nReport received from PC1: %s\n", verdictLine, path)
		return
	}
	fmt.Fprintf(r.w, "\n%s\nSummary: %s\n", verdictLine, path)
}
