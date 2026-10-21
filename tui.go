package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

func usableTerminal() bool {
	return os.Getenv("TERM") != "dumb" && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
func requireTerminal() error {
	if !usableTerminal() {
		return errors.New("--tui requires terminal stdin/stdout and TERM other than dumb")
	}
	return nil
}

// uiWork owns every command and waits for cancellation before returning to the CLI.
type uiWork struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

func newUIWork() *uiWork {
	ctx, cancel := context.WithCancel(context.Background())
	return &uiWork{ctx: ctx, cancel: cancel}
}
func (w *uiWork) close() { w.mu.Lock(); w.closed = true; w.cancel(); w.mu.Unlock(); w.wg.Wait() }
func (w *uiWork) command(fn func(context.Context) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return nil
		}
		w.wg.Add(1)
		w.mu.Unlock()
		defer w.wg.Done()
		ctx, cancel := context.WithTimeout(w.ctx, 45*time.Second)
		defer cancel()
		if ctx.Err() != nil {
			return nil
		}
		return fn(ctx)
	}
}

// safeText removes control and formatting characters, including terminal escapes.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
func uiView(title, body, help string, width, height, offset int) string {
	width = max(1, width)
	height = max(1, height)
	if height < 4 {
		return ansi.Truncate("Esc/q: exit", width, "")
	}
	lines := strings.Split(ansi.Hardwrap(body, width, true), "\n")
	footer := strings.Split(ansi.Hardwrap(help, width, true), "\n")
	footer = footer[:min(len(footer), max(1, min(3, height/3)))]
	offset = min(max(0, offset), max(0, len(lines)-1))
	end := min(len(lines), offset+height-1-len(footer))
	return ansi.Truncate(safeText(title), width, "") + "\n" + strings.Join(lines[offset:end], "\n") + "\n" + strings.Join(footer, "\n")
}

type panelResult struct {
	body   string
	peers  []peerRow
	err    error
	action string
}
type panelModel struct {
	work                            *uiWork
	title                           string
	readOnly                        bool
	load                            func(context.Context) panelResult
	act                             func(context.Context, string) error
	busy                            bool
	confirm                         string
	pendingAction                   string
	notice, body                    string
	err                             error
	peers                           []peerRow
	selected, offset, width, height int
}

func (m *panelModel) refresh(action string) tea.Cmd {
	m.busy = true
	m.pendingAction = action
	m.notice = ""
	m.confirm = ""
	return m.work.command(func(ctx context.Context) tea.Msg {
		var actionErr error
		if action != "" {
			actionErr = m.act(ctx, action)
		}
		r := m.load(ctx)
		r.action = action
		if actionErr != nil {
			r.err = fmt.Errorf("%s failed (completed effects are not rolled back): %w", action, actionErr)
		}
		return r
	})
}
func (m *panelModel) Init() tea.Cmd { return m.refresh("") }
func (m *panelModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
	case panelResult:
		m.busy = false
		m.body = v.body
		m.peers = v.peers
		m.err = v.err
		m.selected = min(m.selected, max(0, len(m.peers)-1))
		m.offset = 0
		if v.action != "" && v.err == nil {
			m.notice = v.action + " completed"
		}
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" || key == "q" || key == "esc" && m.confirm == "" {
			m.work.cancel()
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if m.confirm != "" {
			if key == "y" {
				return m, m.refresh(m.confirm)
			}
			if key == "n" || key == "esc" {
				m.confirm = ""
			}
			return m, nil
		}
		switch key {
		case "r":
			return m, m.refresh("")
		case "down", "j":
			if len(m.peers) > 0 {
				m.selected = min(m.selected+1, len(m.peers)-1)
				m.offset = 0
			} else {
				m.offset++
			}
		case "up", "k":
			if len(m.peers) > 0 {
				m.selected = max(0, m.selected-1)
				m.offset = 0
			} else {
				m.offset = max(0, m.offset-1)
			}
		case "pgdown":
			m.offset += max(1, m.height-4)
		case "pgup":
			m.offset = max(0, m.offset-max(1, m.height-4))
		case "i", "s", "x", "u":
			if !m.readOnly {
				action := map[string]string{"i": "install", "s": "start", "x": "stop", "u": "uninstall"}[key]
				if key == "x" || key == "u" {
					m.confirm = action
				} else {
					return m, m.refresh(action)
				}
			}
		}
	}
	return m, nil
}
func (m *panelModel) View() string {
	body := m.body
	if len(m.peers) > 0 {
		p := m.peers[m.selected]
		body = fmt.Sprintf("Host %d/%d\nhost_id: %s\nendpoint: %s\nconnected: %t\npending: %t", m.selected+1, len(m.peers), safeText(p.HostID), safeText(p.Endpoint), p.Connected, p.Pending)
	}
	if m.notice != "" {
		body = m.notice + "\n" + body
	}
	if m.err != nil {
		body = "Error: " + safeText(m.err.Error()) + "\n" + body
	}
	if m.busy {
		body = "Working… (q cancels; completed effects remain)\n" + body
	}
	if m.confirm != "" {
		body = "Confirm " + m.confirm + "? y / n\n" + body
	}
	help := "q: exit  r: refresh  ↑↓: browse  PgUp/PgDn: scroll"
	if !m.readOnly {
		help = "q: exit | i: install s: start x: stop u: uninstall | r: refresh | arrows/PgUp/Dn: scroll"
	}
	return uiView(m.title, body, help, m.width, m.height, m.offset)
}
func runPanel(m *panelModel) error {
	if err := requireTerminal(); err != nil {
		return err
	}
	defer m.work.close()
	_, err := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	if m.busy && m.pendingAction != "" {
		return errors.New("service operation interrupted; completed effects were not rolled back; refresh status to verify")
	}
	return m.err
}
