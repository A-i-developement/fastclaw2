package tui

import (
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	got := RenderMarkdown("**bold**\n\n- one\n- two\n", 100)
	if strings.Contains(got, "**bold**") {
		t.Fatalf("markdown syntax was not rendered: %q", got)
	}
	if !strings.Contains(got, "bold") || !strings.Contains(got, "one") {
		t.Fatalf("rendered output lost content: %q", got)
	}
}

func TestCompleteMarkdownPrefix(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "completed paragraph",
			text: "**bold** paragraph\n\nunfinished",
			want: "**bold** paragraph\n\n",
		},
		{
			name: "unfinished paragraph",
			text: "still arriving",
			want: "",
		},
		{
			name: "blank line inside fence",
			text: "```go\nfmt.Println(1)\n\nmore",
			want: "",
		},
		{
			name: "closed fence",
			text: "```go\nfmt.Println(1)\n\n```\nnext",
			want: "```go\nfmt.Println(1)\n\n```\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cut := CompleteMarkdownPrefix(tt.text)
			if got := tt.text[:cut]; got != tt.want {
				t.Fatalf("complete prefix = %q, want %q", got, tt.want)
			}
		})
	}
}
