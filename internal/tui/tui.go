// Package tui is the interactive terminal front-end for `fastclaw
// chat`. It is a thin client over the gateway's /api/chat endpoints:
// the agent loop runs server-side; this package renders the event
// stream and manages input, sessions, and agent switching.
package tui

import tea "github.com/charmbracelet/bubbletea"

// Run starts the chat TUI and blocks until the user exits.
func Run(opts Options) error {
	m := NewModel(opts)
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.SetProgram(p)
	_, err := p.Run()
	return err
}
