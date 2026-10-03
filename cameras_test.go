package main

import (
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/zalando/go-keyring"
)

func TestParseHost(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{in: "camera.local", wantHost: "camera.local", wantPort: 554},
		{in: "192.168.1.50:8554", wantHost: "192.168.1.50", wantPort: 8554},
		{in: " cam:554 ", wantHost: "cam", wantPort: 554},
		{in: "[fe80::1]:8554", wantHost: "fe80::1", wantPort: 8554},
		{in: "[fe80::1]", wantHost: "fe80::1", wantPort: 554},
		{in: "fe80::1", wantHost: "fe80::1", wantPort: 554},
		{in: "", wantErr: true},
		{in: "cam:", wantHost: "cam", wantPort: 554},
		{in: "cam:0", wantErr: true},
		{in: "cam:65536", wantErr: true},
		{in: "cam:abc", wantErr: true},
		{in: ":8554", wantErr: true},
	}
	for _, tt := range tests {
		host, port, err := parseHost(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseHost(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (host != tt.wantHost || port != tt.wantPort) {
			t.Errorf("parseHost(%q) = %q, %d; want %q, %d", tt.in, host, port, tt.wantHost, tt.wantPort)
		}
	}
}

func TestCameraAddrRoundTrip(t *testing.T) {
	for _, c := range []savedCamera{
		{Host: "cam", Port: 554},
		{Host: "cam", Port: 8554},
		{Host: "fe80::1", Port: 554},
		{Host: "fe80::1", Port: 8554},
	} {
		host, port, err := parseHost(c.addr())
		if err != nil || host != c.Host || port != c.Port {
			t.Errorf("parseHost(%q) = %q, %d, %v; want %q, %d", c.addr(), host, port, err, c.Host, c.Port)
		}
	}
}

func TestRememberCamera(t *testing.T) {
	a := savedCamera{Host: "a", Port: 554, Name: "x"}
	b := savedCamera{Host: "b", Port: 554, Name: "x"}
	cams := rememberCamera(rememberCamera(nil, a), b)
	cams = rememberCamera(cams, a)
	if len(cams) != 2 || !cams[0].sameAs(a) || !cams[1].sameAs(b) {
		t.Fatalf("got %+v; want [a b]", cams)
	}
	for i := range maxSavedCams + 5 {
		cams = rememberCamera(cams, savedCamera{Host: "h", Port: 554 + i, Name: "x"})
	}
	if len(cams) != maxSavedCams {
		t.Fatalf("len = %d; want %d", len(cams), maxSavedCams)
	}
	if got := forgetCamera([]savedCamera{a, b}, a); len(got) != 1 || !got[0].sameAs(b) {
		t.Fatalf("forgetCamera = %+v; want [b]", got)
	}
}

func TestSaveLoadCameras(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cameras.json")
	if cams, err := loadCameras(path); err != nil || cams != nil {
		t.Fatalf("loading missing file = %v, %v; want nil, nil", cams, err)
	}
	want := []savedCamera{{Host: "cam", Port: 8554, User: "admin", Name: "live"}}
	if err := saveCameras(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCameras(path)
	if err != nil || len(got) != 1 || !got[0].sameAs(want[0]) {
		t.Fatalf("loadCameras = %+v, %v; want %+v", got, err, want)
	}
}

// press feeds keys to the picker: named keys like "enter", or text to type.
func press(m pickerModel, keys ...string) pickerModel {
	named := map[string]rune{"enter": tea.KeyEnter, "esc": tea.KeyEscape, "down": tea.KeyDown}
	for _, k := range keys {
		if code, ok := named[k]; ok {
			next, _ := m.Update(tea.KeyPressMsg{Code: code})
			m = next.(pickerModel)
			continue
		}
		for _, r := range k {
			next, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
			m = next.(pickerModel)
		}
	}
	return m
}

func TestPickerWizard(t *testing.T) {
	base := config{hwaccel: "auto"}
	m := press(newPicker(base, nil), "enter") // empty host is rejected
	if m.err == "" || m.step != stepHost {
		t.Fatalf("empty host accepted: step %d, err %q", m.step, m.err)
	}
	m = press(m, "cam:8554", "enter", "live/ch0", "enter", "admin", "enter", "s3cret", "enter")
	if m.screen != screenRemember || m.result != nil {
		t.Fatalf("expected remember prompt; screen %d, err %q", m.screen, m.err)
	}
	if back := press(m, "esc"); back.screen != screenWizard || back.step != stepPass {
		t.Fatalf("esc from remember prompt went to screen %d step %d", back.screen, back.step)
	}
	m = press(m, "y")
	if m.result == nil {
		t.Fatalf("wizard did not finish; step %d, err %q", m.step, m.err)
	}
	want := config{host: "cam", port: 8554, name: "live/ch0", user: "admin", pass: "s3cret", hwaccel: "auto", passChoice: passRemember}
	if *m.result != want {
		t.Fatalf("result = %+v; want %+v", *m.result, want)
	}

	// Without a username the password step is skipped.
	m = press(newPicker(base, nil), "cam", "enter", "live", "enter", "enter")
	if m.result == nil || m.result.port != 554 || m.result.user != "" {
		t.Fatalf("result = %+v; want port 554 and no user", m.result)
	}
}

func TestPickerSaved(t *testing.T) {
	cams := []savedCamera{
		{Host: "open", Port: 554, Name: "a"},
		{Host: "locked", Port: 8554, User: "admin", Name: "b"},
	}
	m := press(newPicker(config{}, cams), "enter")
	if m.result == nil || m.result.host != "open" {
		t.Fatalf("result = %+v; want host open", m.result)
	}

	m = press(newPicker(config{}, cams), "down", "enter")
	if m.screen != screenPassword || m.result != nil {
		t.Fatalf("expected password prompt, screen %d", m.screen)
	}
	m = press(m, "pw", "enter", "n")
	if m.result == nil || m.result.host != "locked" || m.result.port != 8554 || m.result.pass != "pw" || m.result.passChoice != passForget {
		t.Fatalf("result = %+v; want locked:8554 with password, not remembered", m.result)
	}

	m = press(newPicker(config{}, cams), "d")
	if len(m.forgotten) != 1 || len(m.cams) != 1 || m.cams[0].Host != "locked" {
		t.Fatalf("forget: cams = %+v", m.cams)
	}

	m = press(newPicker(config{}, cams), "down", "down", "enter")
	if m.screen != screenWizard {
		t.Fatalf("New camera did not open the wizard")
	}
	if m = press(m, "esc"); m.screen != screenList {
		t.Fatalf("esc from first wizard step did not return to the list")
	}
}

// selectSaved presses enter on the first saved camera and runs the keyring
// lookup it starts.
func selectSaved(t *testing.T, cam savedCamera) pickerModel {
	t.Helper()
	next, cmd := newPicker(config{}, []savedCamera{cam}).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := next.(pickerModel)
	if m.screen != screenUnlocking || cmd == nil {
		t.Fatalf("expected keyring lookup; screen %d", m.screen)
	}
	next, _ = m.Update(cmd())
	return next.(pickerModel)
}

func TestPickerStoredPassword(t *testing.T) {
	keyring.MockInit()
	cam := savedCamera{Host: "cam", Port: 554, User: "admin", Name: "live", RememberPassword: true}

	// Nothing in the keyring: fall back to asking.
	if m := selectSaved(t, cam); m.screen != screenPassword || m.err == "" {
		t.Fatalf("missing password: screen %d, err %q", m.screen, m.err)
	}

	if err := keyring.Set(keyringService, cam.keyringUser(), "pw"); err != nil {
		t.Fatal(err)
	}
	m := selectSaved(t, cam)
	if m.result == nil || m.result.pass != "pw" || m.result.passChoice != passKeep {
		t.Fatalf("result = %+v; want stored password, kept", m.result)
	}
}

func TestRecordOpenedPassword(t *testing.T) {
	keyring.MockInit()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config{host: "cam", port: 554, user: "admin", pass: "pw", name: "live"}
	user := cameraFromConfig(cfg).keyringUser()

	cfg.passChoice = passRemember
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := keyring.Get(keyringService, user); err != nil || got != "pw" {
		t.Fatalf("keyring = %q, %v; want pw", got, err)
	}
	if pass, ok := rememberedPassword(config{host: "cam", port: 554, user: "admin", name: "live"}); !ok || pass != "pw" {
		t.Fatalf("rememberedPassword = %q, %v; want pw", pass, ok)
	}

	// Opening it again from the command line keeps the saved password.
	cfg.passChoice = passKeep
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := rememberedPassword(cfg); !ok {
		t.Fatal("password lost after passKeep")
	}

	cfg.passChoice = passForget
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keyringService, user); err != keyring.ErrNotFound {
		t.Fatalf("keyring after forget: %v; want ErrNotFound", err)
	}
	if _, ok := rememberedPassword(cfg); ok {
		t.Fatal("rememberedPassword still found after passForget")
	}
}
