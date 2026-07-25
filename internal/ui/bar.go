package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultWidth = 80
	minWidth     = 40
	maxWidth     = 200
	minBarWidth  = 10
)

func detectWidth(env func(string) string) int {
	if env == nil {
		return defaultWidth
	}
	width, err := strconv.Atoi(env("COLUMNS"))
	if err != nil || width <= 0 {
		return defaultWidth
	}
	return clampWidth(width)
}

func clampWidth(width int) int {
	if width < minWidth {
		return minWidth
	}
	if width > maxWidth {
		return maxWidth
	}
	return width
}

// renderBar renders one fixed-width, ANSI-free progress line. Width is
// measured in runes so truncation never splits UTF-8 input.
func renderBar(step, total, width int, name, sub, elapsed string) string {
	return renderBarFraction(step, total, width, name, sub, elapsed, fraction(step, total))
}

func renderBarFraction(step, total, width int, name, sub, elapsed string, complete float64) string {
	width = clampWidth(width)
	numeric := fmt.Sprintf("[%d/%d] ", step, total)

	// Keep a useful bar by trimming optional detail first, then the name.
	detailBudget := max(0, width-runeLen(numeric)-runeLen(name)-minBarWidth-4)
	detail := fitDetail(sub, elapsed, detailBudget)
	detailCost := 0
	if detail != "" {
		detailCost = runeLen(detail) + 1
	}
	nameBudget := width - runeLen(numeric) - minBarWidth - 3 - detailCost
	name = truncateRunes(name, max(0, nameBudget))

	prefix := numeric + name
	barWidth := width - runeLen(prefix) - detailCost - 3
	if barWidth < 1 {
		barWidth = 1
	}
	complete = clampFraction(complete)
	filled := int(complete*float64(barWidth) + 0.5)
	if filled > barWidth {
		filled = barWidth
	}
	line := prefix + " [" + strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled) + "]"
	if detail != "" {
		line += " " + detail
	}
	if n := width - runeLen(line); n > 0 {
		line += strings.Repeat(" ", n)
	} else if n < 0 {
		line = truncateRunes(line, width)
	}
	return line
}

// fitDetail packs the progress detail ("sub elapsed") into budget runes while
// protecting elapsed: it is short and fixed, so sub (progress text + metrics)
// is truncated first. Elapsed itself is only trimmed as a last resort when the
// budget cannot even hold it.
func fitDetail(sub, elapsed string, budget int) string {
	sub = strings.TrimSpace(sub)
	elapsed = strings.TrimSpace(elapsed)
	if elapsed == "" {
		return truncateRunes(sub, budget)
	}
	if runeLen(elapsed) >= budget {
		return truncateRunes(elapsed, budget)
	}
	if sub == "" {
		return elapsed
	}
	sub = truncateRunes(sub, budget-runeLen(elapsed)-1)
	if sub == "" {
		return elapsed
	}
	return sub + " " + elapsed
}

func fraction(step, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(step) / float64(total)
}

func clampFraction(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func runeLen(value string) int { return utf8.RuneCountInString(value) }

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 0 {
		return ""
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}
