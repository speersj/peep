package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	labelStyle  = lipgloss.NewStyle().Bold(true)
	doneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errCanceled = errors.New("canceled")
)

type screen int

const (
	screenList      screen = iota // pick a saved camera or start the wizard
	screenWizard                  // enter connection details step by step
	screenPassword                // password for a saved camera
	screenRemember                // offer to keep the password in the keyring
	screenUnlocking               // reading a saved password from the keyring
)

// storedPassMsg carries the result of reading a password from the keyring.
type storedPassMsg struct {
	cam  savedCamera
	pass string
	err  error
}

// Wizard steps, in order.
const (
	stepHost = iota
	stepName
	stepUser
	stepPass
	numSteps
)

var stepLabels = [numSteps]string{"Host", "Stream name", "Username", "Password"}

// pickerModel is the bubbletea model for choosing what to connect to.
type pickerModel struct {
	base   config
	cams   []savedCamera
	screen screen
	cursor int // list position; len(cams) is "New camera"

	step   int
	inputs [numSteps]textinput.Model
	picked savedCamera // camera awaiting a password on screenPassword

	pending      config // connection awaiting an answer on screenRemember
	rememberFrom screen // where esc on screenRemember returns to

	err       string
	result    *config
	forgotten []savedCamera // cameras removed from the list
}

func newPicker(base config, cams []savedCamera) pickerModel {
	m := pickerModel{base: base, cams: cams}
	placeholders := [numSteps]string{
		"camera.local or 192.168.1.50:8554 (port defaults to 554)",
		"live/ch0",
		"optional; leave empty for none",
		"",
	}
	for i := range m.inputs {
		in := textinput.New()
		in.Prompt = "› "
		in.Placeholder = placeholders[i]
		in.SetWidth(60)
		m.inputs[i] = in
	}
	m.inputs[stepPass].EchoMode = textinput.EchoPassword
	m.inputs[stepName].SetValue(base.name)
	m.inputs[stepUser].SetValue(base.user)
	m.inputs[stepPass].SetValue(base.pass)
	if len(cams) == 0 {
		m.screen = screenWizard
		m.inputs[stepHost].Focus()
	}
	return m
}

func (m pickerModel) Init() tea.Cmd { return textinput.Blink }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if res, ok := msg.(storedPassMsg); ok {
		return m.gotStoredPassword(res)
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if key.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.screen {
		case screenList:
			return m.updateList(key)
		case screenRemember:
			return m.rememberKey(key)
		case screenUnlocking:
			return m, nil
		case screenWizard:
			if mm, cmd, handled := m.wizardKey(key); handled {
				return mm, cmd
			}
		case screenPassword:
			if mm, cmd, handled := m.passwordKey(key); handled {
				return mm, cmd
			}
		}
	}
	return m.updateInput(msg)
}

// updateInput forwards msg to the focused text input.
func (m pickerModel) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.screen {
	case screenWizard:
		m.inputs[m.step], cmd = m.inputs[m.step].Update(msg)
	case screenPassword:
		m.inputs[stepPass], cmd = m.inputs[stepPass].Update(msg)
	}
	return m, cmd
}

func (m pickerModel) updateList(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.err = ""
	switch key.String() {
	case "up", "k", "shift+tab":
		m.cursor = (m.cursor + len(m.cams)) % (len(m.cams) + 1)
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % (len(m.cams) + 1)
	case "q", "esc":
		return m, tea.Quit
	case "d", "delete", "x":
		if m.cursor < len(m.cams) {
			m.forgotten = append(m.forgotten, m.cams[m.cursor])
			m.cams = forgetCamera(m.cams, m.cams[m.cursor])
			m.cursor = min(m.cursor, len(m.cams))
		}
	case "e":
		if m.cursor < len(m.cams) {
			return m.startWizard(m.cams[m.cursor])
		}
	case "enter", "space":
		if m.cursor == len(m.cams) {
			return m.startWizard(savedCamera{})
		}
		cam := m.cams[m.cursor]
		if cam.User == "" {
			return m.finish(cam.config(m.base))
		}
		m.picked = cam
		if cam.RememberPassword {
			m.screen = screenUnlocking
			return m, func() tea.Msg {
				pass, err := cam.storedPassword()
				return storedPassMsg{cam, pass, err}
			}
		}
		return m, m.askPassword()
	}
	return m, nil
}

// askPassword shows the password prompt for m.picked.
func (m *pickerModel) askPassword() tea.Cmd {
	m.screen = screenPassword
	m.inputs[stepPass].SetValue("")
	return m.inputs[stepPass].Focus()
}

// gotStoredPassword connects with a password read from the keyring, or
// falls back to asking for it.
func (m pickerModel) gotStoredPassword(res storedPassMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenUnlocking || !res.cam.sameAs(m.picked) {
		return m, nil
	}
	if res.err != nil {
		m.err = fmt.Sprintf("couldn't read the saved password: %v", res.err)
		return m, m.askPassword()
	}
	cfg := res.cam.config(m.base)
	cfg.pass = res.pass
	return m.finish(cfg)
}

// offerRemember asks whether to keep cfg's password, or connects straight
// away when there is no password to keep.
func (m pickerModel) offerRemember(cfg config) (tea.Model, tea.Cmd) {
	if cfg.user == "" || cfg.pass == "" {
		cfg.passChoice = passForget
		return m.finish(cfg)
	}
	m.inputs[stepPass].Blur()
	m.pending = cfg
	m.rememberFrom = m.screen
	m.screen = screenRemember
	return m, nil
}

func (m pickerModel) rememberKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cfg := m.pending
	switch key.String() {
	case "y", "Y":
		cfg.passChoice = passRemember
	case "n", "N", "enter":
		cfg.passChoice = passForget
	case "esc":
		m.screen = m.rememberFrom
		return m, m.inputs[stepPass].Focus()
	default:
		return m, nil
	}
	return m.finish(cfg)
}

// startWizard opens the wizard, prefilled from cam when editing one.
func (m pickerModel) startWizard(cam savedCamera) (tea.Model, tea.Cmd) {
	m.screen = screenWizard
	m.step = stepHost
	m.err = ""
	if cam.Host != "" {
		m.inputs[stepHost].SetValue(cam.addr())
		m.inputs[stepName].SetValue(cam.Name)
		m.inputs[stepUser].SetValue(cam.User)
		m.inputs[stepPass].SetValue("")
	}
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	return m, m.inputs[stepHost].Focus()
}

func (m pickerModel) wizardKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch key.String() {
	case "enter", "tab":
		if err := m.validateStep(); err != nil {
			m.err = err.Error()
			return m, nil, true
		}
		m.err = ""
		next := m.step + 1
		if next == stepPass && strings.TrimSpace(m.inputs[stepUser].Value()) == "" {
			next = numSteps // no username, so no password
		}
		if next == numSteps {
			cfg, err := m.wizardConfig()
			if err != nil {
				m.err = err.Error()
				return m, nil, true
			}
			mm, cmd := m.offerRemember(cfg)
			return mm, cmd, true
		}
		return m, m.focusStep(next), true
	case "esc", "shift+tab":
		m.err = ""
		if m.step > stepHost {
			return m, m.focusStep(m.step - 1), true
		}
		if len(m.cams) == 0 {
			return m, tea.Quit, true
		}
		m.inputs[m.step].Blur()
		m.screen = screenList
		return m, nil, true
	}
	return m, nil, false
}

func (m *pickerModel) focusStep(step int) tea.Cmd {
	m.inputs[m.step].Blur()
	m.step = step
	m.inputs[step].CursorEnd()
	return m.inputs[step].Focus()
}

func (m pickerModel) validateStep() error {
	v := strings.TrimSpace(m.inputs[m.step].Value())
	switch m.step {
	case stepHost:
		_, _, err := parseHost(v)
		return err
	case stepName:
		if v == "" {
			return errors.New("stream name is required, e.g. live/ch0")
		}
	}
	return nil
}

func (m pickerModel) wizardConfig() (config, error) {
	cfg := m.base
	var err error
	cfg.host, cfg.port, err = parseHost(m.inputs[stepHost].Value())
	if err != nil {
		return cfg, err
	}
	cfg.name = strings.TrimSpace(m.inputs[stepName].Value())
	cfg.user = strings.TrimSpace(m.inputs[stepUser].Value())
	cfg.pass = ""
	if cfg.user != "" {
		cfg.pass = m.inputs[stepPass].Value()
	}
	return cfg, nil
}

func (m pickerModel) passwordKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch key.String() {
	case "enter":
		cfg := m.picked.config(m.base)
		cfg.pass = m.inputs[stepPass].Value()
		mm, cmd := m.offerRemember(cfg)
		return mm, cmd, true
	case "esc":
		m.err = ""
		m.inputs[stepPass].Blur()
		m.screen = screenList
		return m, nil, true
	}
	return m, nil, false
}

func (m pickerModel) finish(cfg config) (tea.Model, tea.Cmd) {
	m.result = &cfg
	return m, tea.Quit
}

func (m pickerModel) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render("peep") + dimStyle.Render(" · RTSP viewer") + "\n\n")
	switch m.screen {
	case screenList:
		b.WriteString(labelStyle.Render("Choose a camera") + "\n\n")
		for i := 0; i <= len(m.cams); i++ {
			text, note := "+ New camera…", ""
			if i < len(m.cams) {
				text = m.cams[i].label()
				if m.cams[i].RememberPassword {
					note = dimStyle.Render("  · password saved")
				}
			}
			if i == m.cursor {
				b.WriteString(selStyle.Render("› "+text) + note + "\n")
			} else {
				b.WriteString("  " + text + note + "\n")
			}
		}
		b.WriteString("\n" + dimStyle.Render("↑/↓ move · enter connect · e edit · d forget · q quit"))
	case screenWizard:
		b.WriteString(labelStyle.Render("New camera") + dimStyle.Render(fmt.Sprintf("  step %d of %d", m.step+1, m.numSteps())) + "\n\n")
		for i := 0; i < m.step; i++ {
			v := strings.TrimSpace(m.inputs[i].Value())
			switch {
			case i == stepPass:
				v = strings.Repeat("*", len(m.inputs[i].Value()))
			case v == "":
				v = "(none)"
			}
			b.WriteString(doneStyle.Render(fmt.Sprintf("  %s: %s", stepLabels[i], v)) + "\n")
		}
		b.WriteString(labelStyle.Render(stepLabels[m.step]) + "\n" + m.inputs[m.step].View() + "\n")
		b.WriteString("\n" + dimStyle.Render("enter next · esc back · ctrl+c quit"))
	case screenPassword:
		b.WriteString(labelStyle.Render("Password for "+m.picked.label()) + "\n")
		b.WriteString(m.inputs[stepPass].View() + "\n")
		b.WriteString("\n" + dimStyle.Render("enter connect · esc back · ctrl+c quit"))
	case screenRemember:
		b.WriteString(labelStyle.Render("Remember the password for "+cameraFromConfig(m.pending).label()+"?") + "\n")
		b.WriteString(dimStyle.Render("It is kept in the system keyring and used to connect automatically.") + "\n")
		b.WriteString("\n" + dimStyle.Render("y remember · n/enter don't · esc back"))
	case screenUnlocking:
		b.WriteString(dimStyle.Render("Reading the saved password for " + m.picked.label() + "…"))
	}
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render(m.err))
	}
	b.WriteString("\n")
	return tea.NewView(b.String())
}

// numSteps is how many wizard steps apply given the username so far.
func (m pickerModel) numSteps() int {
	if strings.TrimSpace(m.inputs[stepUser].Value()) == "" {
		return numSteps - 1
	}
	return numSteps
}

// pickCamera runs the interactive picker and returns the chosen connection,
// or errCanceled when the user quit.
func pickCamera(base config) (config, error) {
	path, pathErr := camerasPath()
	var cams []savedCamera
	if pathErr == nil {
		var err error
		if cams, err = loadCameras(path); err != nil {
			fmt.Fprintf(os.Stderr, "peep: ignoring saved cameras: %v\n", err)
		}
	}
	final, err := tea.NewProgram(newPicker(base, cams)).Run()
	if err != nil {
		return base, err
	}
	m := final.(pickerModel)
	if len(m.forgotten) > 0 && pathErr == nil {
		if err := saveCameras(path, m.cams); err != nil {
			fmt.Fprintf(os.Stderr, "peep: saving cameras: %v\n", err)
		}
		for _, c := range m.forgotten {
			if !c.RememberPassword {
				continue
			}
			if err := c.dropPassword(); err != nil {
				fmt.Fprintf(os.Stderr, "peep: removing password from keyring: %v\n", err)
			}
		}
	}
	if m.result == nil {
		return base, errCanceled
	}
	return *m.result, nil
}
