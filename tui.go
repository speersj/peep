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
	screenWizard                  // enter camera details step by step
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
	stepName = iota
	stepHost
	stepStream
	stepDetect // a yes/no choice rather than text
	stepUser
	stepPass
	numSteps
)

var stepLabels = [numSteps]string{"Camera name", "Host", "Stream", "Detection", "Username", "Password"}

// pickerModel is the bubbletea model for choosing what to connect to.
type pickerModel struct {
	base   config
	cams   []savedCamera
	screen screen
	cursor int  // list position; len(cams) is "New camera"
	direct bool // started for one camera, with no list to return to

	step    int
	inputs  [numSteps]textinput.Model
	editing string      // name of the camera being edited in the wizard
	detect  bool        // the wizard's detection choice
	picked  savedCamera // camera awaiting a password on screenPassword

	pending      config // connection awaiting an answer on screenRemember
	rememberFrom screen // where esc on screenRemember returns to

	err       string
	result    *config
	forgotten []savedCamera // cameras removed from the list
	changed   bool          // cams edited in the list and needing saving
}

// newPicker starts on the list of cams, or in the wizard when there are none.
func newPicker(base config, cams []savedCamera) pickerModel {
	m := pickerModel{base: base, cams: cams}
	placeholders := [numSteps]string{
		"e.g. frontdoor; used as `peep frontdoor`",
		"camera.local or 192.168.1.50:8554 (port defaults to 554)",
		"live/ch0",
		"",
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
	if len(cams) == 0 {
		m.openWizard(savedCamera{}, stepName)
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
		if m.step != stepDetect {
			m.inputs[m.step], cmd = m.inputs[m.step].Update(msg)
		}
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
			m.cams = forgetCamera(m.cams, m.cams[m.cursor].Name)
			m.cursor = min(m.cursor, len(m.cams))
		}
	case "e":
		if m.cursor < len(m.cams) {
			return m, m.openWizard(m.cams[m.cursor], stepName)
		}
	case "t":
		if m.cursor < len(m.cams) {
			m.cams[m.cursor].Detect = !m.cams[m.cursor].Detect
			m.changed = true
		}
	case "enter", "space":
		if m.cursor == len(m.cams) {
			return m, m.openWizard(savedCamera{}, stepName)
		}
		return m.connect(m.cams[m.cursor])
	}
	return m, nil
}

// connect connects to a saved camera, first reading its password from the
// keyring or asking for it.
func (m pickerModel) connect(cam savedCamera) (tea.Model, tea.Cmd) {
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

// askPassword shows the password prompt for m.picked.
func (m *pickerModel) askPassword() tea.Cmd {
	m.screen = screenPassword
	m.inputs[stepPass].SetValue("")
	return m.inputs[stepPass].Focus()
}

// gotStoredPassword connects with a password read from the keyring, or
// falls back to asking for it.
func (m pickerModel) gotStoredPassword(res storedPassMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenUnlocking || !res.cam.is(m.picked.Name) {
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

// openWizard shows the wizard at step, prefilled from cam. A cam with a
// host is being edited; otherwise it is a new camera, possibly named.
func (m *pickerModel) openWizard(cam savedCamera, step int) tea.Cmd {
	m.screen = screenWizard
	m.err = ""
	m.editing = ""
	if cam.Host != "" {
		m.editing = cam.Name
	}
	m.inputs[stepName].SetValue(cam.Name)
	m.inputs[stepHost].SetValue(cam.addrOrEmpty())
	m.inputs[stepStream].SetValue(cam.Stream)
	m.detect = cam.Detect
	m.inputs[stepUser].SetValue(cam.User)
	m.inputs[stepPass].SetValue("")
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	m.step = step
	m.inputs[step].CursorEnd()
	return m.inputs[step].Focus()
}

func (m pickerModel) wizardKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	k := key.String()
	if m.step == stepDetect {
		switch k {
		case "y", "Y":
			m.detect, k = true, "enter"
		case "n", "N":
			m.detect, k = false, "enter"
		case "space", "left", "right", "h", "l":
			m.detect = !m.detect
			return m, nil, true
		case "enter", "tab", "esc", "shift+tab":
		default:
			return m, nil, true
		}
	}
	switch k {
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
		if m.step > stepName {
			return m, m.focusStep(m.step - 1), true
		}
		if m.direct || len(m.cams) == 0 {
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
	if step == stepDetect {
		return nil
	}
	m.inputs[step].CursorEnd()
	return m.inputs[step].Focus()
}

func (m pickerModel) validateStep() error {
	v := strings.TrimSpace(m.inputs[m.step].Value())
	switch m.step {
	case stepName:
		return validCameraName(v, m.cams, m.editing)
	case stepHost:
		_, _, err := parseHost(v)
		return err
	case stepStream:
		if v == "" {
			return errors.New("stream is required, e.g. live/ch0")
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
	cfg.camera = strings.TrimSpace(m.inputs[stepName].Value())
	cfg.stream = strings.TrimSpace(m.inputs[stepStream].Value())
	cfg.detect = m.detect
	cfg.user = strings.TrimSpace(m.inputs[stepUser].Value())
	cfg.pass = ""
	if cfg.user != "" {
		cfg.pass = m.inputs[stepPass].Value()
	}
	if !strings.EqualFold(m.editing, cfg.camera) {
		cfg.replaces = m.editing
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
		if m.direct {
			return m, tea.Quit, true
		}
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
		width := 0
		for _, c := range m.cams {
			width = max(width, len(c.Name))
		}
		for i := 0; i <= len(m.cams); i++ {
			text, note := "+ New camera…", ""
			if i < len(m.cams) {
				c := m.cams[i]
				text = fmt.Sprintf("%-*s", width, c.Name)
				note = dimStyle.Render("  " + c.label())
				if c.Detect {
					note += dimStyle.Render(" · detection")
				}
				if c.RememberPassword {
					note += dimStyle.Render(" · password saved")
				}
			}
			if i == m.cursor {
				b.WriteString(selStyle.Render("› "+text) + note + "\n")
			} else {
				b.WriteString("  " + text + note + "\n")
			}
		}
		b.WriteString("\n" + dimStyle.Render("↑/↓ move · enter connect · e edit · t toggle detection · d forget · q quit"))
	case screenWizard:
		title := "New camera"
		if m.editing != "" {
			title = "Edit " + m.editing
		}
		b.WriteString(labelStyle.Render(title) + dimStyle.Render(fmt.Sprintf("  step %d of %d", m.step+1, m.numSteps())) + "\n\n")
		for i := 0; i < m.step; i++ {
			v := strings.TrimSpace(m.inputs[i].Value())
			switch {
			case i == stepDetect:
				v = onOff(m.detect)
			case i == stepPass:
				v = strings.Repeat("*", len(m.inputs[i].Value()))
			case v == "":
				v = "(none)"
			}
			b.WriteString(doneStyle.Render(fmt.Sprintf("  %s: %s", stepLabels[i], v)) + "\n")
		}
		if m.step == stepDetect {
			yes, no := "  yes", "  no"
			if m.detect {
				yes = selStyle.Render("› yes")
			} else {
				no = selStyle.Render("› no")
			}
			b.WriteString(labelStyle.Render("Detect people, vehicles and animals?") + "\n")
			b.WriteString(dimStyle.Render("Outlines them in the video and notifies you when one appears.") + "\n")
			b.WriteString(yes + "   " + no + "\n")
			b.WriteString("\n" + dimStyle.Render("y/n choose · space toggle · enter next · esc back"))
		} else {
			b.WriteString(labelStyle.Render(stepLabels[m.step]) + "\n" + m.inputs[m.step].View() + "\n")
			b.WriteString("\n" + dimStyle.Render("enter next · esc back · ctrl+c quit"))
		}
	case screenPassword:
		b.WriteString(labelStyle.Render("Password for "+m.picked.Name) + dimStyle.Render("  "+m.picked.label()) + "\n")
		b.WriteString(m.inputs[stepPass].View() + "\n")
		back := "esc back"
		if m.direct {
			back = "esc quit"
		}
		b.WriteString("\n" + dimStyle.Render("enter connect · "+back+" · ctrl+c quit"))
	case screenRemember:
		b.WriteString(labelStyle.Render("Remember the password for "+m.pending.camera+"?") + "\n")
		b.WriteString(dimStyle.Render("It is kept in the system keyring and used to connect automatically.") + "\n")
		b.WriteString("\n" + dimStyle.Render("y remember · n/enter don't · esc back"))
	case screenUnlocking:
		b.WriteString(dimStyle.Render("Reading the saved password for " + m.picked.Name + "…"))
	}
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render(m.err))
	}
	b.WriteString("\n")
	return tea.NewView(b.String())
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// numSteps is how many wizard steps apply given the username so far.
func (m pickerModel) numSteps() int {
	if strings.TrimSpace(m.inputs[stepUser].Value()) == "" {
		return numSteps - 1
	}
	return numSteps
}

// pickCamera decides what to connect to, running the picker when it needs
// input. It returns errCanceled when the user quit.
func pickCamera(base config) (config, error) {
	path, pathErr := camerasPath()
	var cams []savedCamera
	if pathErr == nil {
		var err error
		if cams, err = loadCameras(path); err != nil {
			fmt.Fprintf(os.Stderr, "peep: ignoring saved cameras: %v\n", err)
		}
	}
	m, ready := startPicker(base, cams)
	if ready != nil {
		return *ready, nil
	}

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return base, err
	}
	m = final.(pickerModel)
	if (len(m.forgotten) > 0 || m.changed) && pathErr == nil {
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

// startPicker sets up the picker for base.camera. A saved camera that needs
// no input is returned ready to connect: one without a username, or with a
// password in the keyring. Otherwise the picker asks for the password, or
// starts the wizard to add an unknown camera. With no camera named, it
// lists the saved cameras.
func startPicker(base config, cams []savedCamera) (pickerModel, *config) {
	m := newPicker(base, cams)
	if base.camera == "" {
		return m, nil
	}
	m.direct = true
	cam, ok := findCamera(cams, base.camera)
	if !ok {
		if err := validCameraName(base.camera, cams, ""); err != nil {
			m.openWizard(savedCamera{Name: base.camera}, stepName)
			m.err = err.Error() // show why the name won't do
		} else {
			m.openWizard(savedCamera{Name: base.camera}, stepHost)
		}
		return m, nil
	}
	cfg := cam.config(base)
	if cam.User == "" {
		return m, &cfg
	}
	m.picked = cam
	if cam.RememberPassword {
		pass, err := cam.storedPassword()
		if err == nil {
			cfg.pass = pass
			return m, &cfg
		}
		m.askPassword()
		m.err = fmt.Sprintf("couldn't read the saved password: %v", err)
		return m, nil
	}
	m.askPassword()
	return m, nil
}
