package tui

import "strings"

// slashCommand is one /command the chat input recognises.
type slashCommand struct {
	Name        string
	Aliases     []string
	Description string
}

var slashCommands = []slashCommand{
	{Name: "/new", Description: "开启新会话"},
	{Name: "/sessions", Aliases: []string{"/resume"}, Description: "浏览并切换历史会话"},
	{Name: "/agents", Aliases: []string{"/agent"}, Description: "切换 agent"},
	{Name: "/rename", Description: "重命名当前会话：/rename <标题>"},
	{Name: "/clear", Description: "清空屏幕（不影响服务端会话）"},
	{Name: "/web", Description: "显示 Web 控制台地址"},
	{Name: "/help", Description: "显示帮助"},
	{Name: "/exit", Aliases: []string{"/quit"}, Description: "退出"},
}

// matchSlashCommands returns commands whose name or alias starts with
// the given prefix (e.g. "/se").
func matchSlashCommands(prefix string) []slashCommand {
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	var out []slashCommand
	for _, c := range slashCommands {
		if strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
			continue
		}
		for _, a := range c.Aliases {
			if strings.HasPrefix(a, prefix) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// canonicalSlash resolves aliases to the primary command name and
// splits off the argument portion. Returns "" when text is not a
// recognised slash command.
func canonicalSlash(text string) (name, args string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	head, rest, _ := strings.Cut(text, " ")
	for _, c := range slashCommands {
		if head == c.Name {
			return c.Name, strings.TrimSpace(rest)
		}
		for _, a := range c.Aliases {
			if head == a {
				return c.Name, strings.TrimSpace(rest)
			}
		}
	}
	return "", ""
}

func helpText() string {
	var b strings.Builder
	b.WriteString("命令：\n")
	for _, c := range slashCommands {
		name := c.Name
		if len(c.Aliases) > 0 {
			name += " (" + strings.Join(c.Aliases, ", ") + ")"
		}
		b.WriteString("  " + padRight(name, 24) + c.Description + "\n")
	}
	b.WriteString("\n快捷键：\n")
	b.WriteString("  Enter          发送（回复中则并入当前回合）\n")
	b.WriteString("  Shift+Enter    换行（或 Ctrl+J）\n")
	b.WriteString("  ↑ / ↓          输入历史\n")
	b.WriteString("  PgUp / PgDn    滚动对话\n")
	b.WriteString("  Esc            回复中：脱离本次回合（服务端继续跑完并保存）\n")
	b.WriteString("  Ctrl+L         清屏\n")
	b.WriteString("  Ctrl+C ×2 / Ctrl+D  退出\n")
	b.WriteString("\n以 ! 开头在本机 shell 执行命令，例如 ! git status\n")
	return strings.TrimRight(b.String(), "\n")
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}
