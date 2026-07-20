package tui

import (
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
)

// Cached renderer, rebuilt when the wrap width changes. Only touched
// from the Bubble Tea update goroutine (or the one-shot CLI path), so
// no locking is needed.
var (
	mdRenderer *glamour.TermRenderer
	mdWidth    int
)

func markdownRenderer(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	if width > 120 {
		width = 120
	}
	if mdRenderer == nil || mdWidth != width {
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(TerminalMarkdownStyle()),
			glamour.WithWordWrap(width),
		)
		if err == nil {
			mdRenderer, mdWidth = r, width
		}
	}
	return mdRenderer
}

// RenderMarkdown renders markdown for terminal display, falling back to
// the raw text when glamour fails.
func RenderMarkdown(content string, width int) string {
	r := markdownRenderer(width)
	if r == nil {
		return content
	}
	out, err := r.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimRight(out, "\n")
}

// TerminalMarkdownStyle picks glamour's light or dark style from the
// COLORFGBG convention (final field ≥7 means a light background).
func TerminalMarkdownStyle() string {
	if fields := strings.Split(os.Getenv("COLORFGBG"), ";"); len(fields) > 0 {
		if bg, err := strconv.Atoi(fields[len(fields)-1]); err == nil && bg >= 7 {
			return "light"
		}
	}
	return "dark"
}

// CompleteMarkdownPrefix returns the length of the leading portion of a
// streamed response that is safe to render without guessing how an
// unfinished Markdown construct ends. A blank line closes normal blocks
// (paragraphs, lists, tables); fenced code is held until its closing
// fence arrives, even when it contains blank lines.
func CompleteMarkdownPrefix(text string) int {
	inFence := false
	lastComplete := 0
	lineStart := 0
	for lineStart < len(text) {
		relEnd := strings.IndexByte(text[lineStart:], '\n')
		if relEnd < 0 {
			break
		}
		lineEnd := lineStart + relEnd
		line := strings.TrimSpace(text[lineStart:lineEnd])
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			if !inFence {
				lastComplete = lineEnd + 1
			}
		} else if !inFence && line == "" {
			lastComplete = lineEnd + 1
		}
		lineStart = lineEnd + 1
	}
	return lastComplete
}
