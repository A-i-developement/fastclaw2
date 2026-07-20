package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEnterSubmitsTurn(t *testing.T) {
	m := newTestModel()
	for _, r := range "hi" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "hi" {
		t.Fatalf("input value = %q", got)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.querying {
		t.Fatal("enter did not start a turn")
	}
	if cmd == nil {
		t.Fatal("no command returned from submit")
	}
}
