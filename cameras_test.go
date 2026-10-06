package main

import (
	"fmt"
	"os"
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
	a := savedCamera{Name: "a", Host: "a", Port: 554, Stream: "x"}
	b := savedCamera{Name: "b", Host: "b", Port: 554, Stream: "x"}
	cams := rememberCamera(rememberCamera(nil, a, ""), b, "")
	a2 := a
	a2.Host = "a2"
	cams = rememberCamera(cams, a2, "") // same name replaces
	if len(cams) != 2 || cams[0].Host != "a2" || cams[1].Name != "b" {
		t.Fatalf("got %+v; want [a(a2) b]", cams)
	}
	c := b
	c.Name = "c"
	cams = rememberCamera(cams, c, "B") // renamed b to c, matched without case
	if len(cams) != 2 || cams[0].Name != "c" || cams[1].Name != "a" {
		t.Fatalf("rename: got %+v; want [c a]", cams)
	}
	for i := range maxSavedCams + 5 {
		cams = rememberCamera(cams, savedCamera{Name: fmt.Sprint(i), Host: "h", Port: 554}, "")
	}
	if len(cams) != maxSavedCams {
		t.Fatalf("len = %d; want %d", len(cams), maxSavedCams)
	}
	if got := forgetCamera([]savedCamera{a, b}, "A"); len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("forgetCamera = %+v; want [b]", got)
	}
}

func TestValidCameraName(t *testing.T) {
	cams := []savedCamera{{Name: "FrontDoor"}}
	for _, tt := range []struct {
		name, except string
		ok           bool
	}{
		{"garage", "", true},
		{"", "", false},
		{"-x", "", false},
		{"frontdoor", "", false},         // taken, ignoring case
		{"frontdoor", "FrontDoor", true}, // keeping its own name
	} {
		if err := validCameraName(tt.name, cams, tt.except); (err == nil) != tt.ok {
			t.Errorf("validCameraName(%q, except %q) = %v; want ok %v", tt.name, tt.except, err, tt.ok)
		}
	}
}

func TestSaveLoadCameras(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cameras.json")
	if cams, err := loadCameras(path); err != nil || cams != nil {
		t.Fatalf("loading missing file = %v, %v; want nil, nil", cams, err)
	}
	want := []savedCamera{{Name: "door", Host: "cam", Port: 8554, User: "admin", Stream: "live"}}
	if err := saveCameras(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCameras(path)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("loadCameras = %+v, %v; want %+v", got, err, want)
	}
}

func TestLoadLegacyCameras(t *testing.T) {
	// Before cameras had names, "name" held the stream path.
	path := filepath.Join(t.TempDir(), "cameras.json")
	legacy := `[
		{"host": "192.168.50.120", "port": 554, "user": "justin", "name": "stream1", "remember_password": true},
		{"host": "192.168.50.120", "port": 554, "name": "stream2"}
	]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cams, err := loadCameras(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []savedCamera{
		{Name: "192.168.50.120", Host: "192.168.50.120", Port: 554, User: "justin", Stream: "stream1", RememberPassword: true},
		{Name: "192.168.50.120-2", Host: "192.168.50.120", Port: 554, Stream: "stream2"},
	}
	if len(cams) != len(want) || cams[0] != want[0] || cams[1] != want[1] {
		t.Fatalf("loadCameras = %+v; want %+v", cams, want)
	}
	// The keyring entry is keyed by connection, so it survives migration.
	if got := cams[0].keyringUser(); got != "justin@192.168.50.120/stream1" {
		t.Errorf("keyringUser = %q", got)
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
	m := press(newPicker(base, nil), "enter") // empty name is rejected
	if m.err == "" || m.step != stepName {
		t.Fatalf("empty name accepted: step %d, err %q", m.step, m.err)
	}
	m = press(m, "door", "enter", "enter") // empty host is rejected
	if m.err == "" || m.step != stepHost {
		t.Fatalf("empty host accepted: step %d, err %q", m.step, m.err)
	}
	m = press(m, "cam:8554", "enter", "live/ch0", "enter")
	m = press(m, "admin", "enter", "s3cret", "enter")
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
	want := config{camera: "door", host: "cam", port: 8554, stream: "live/ch0", user: "admin", pass: "s3cret", hwaccel: "auto", passChoice: passRemember}
	if *m.result != want {
		t.Fatalf("result = %+v; want %+v", *m.result, want)
	}

	// Without a username the password step is skipped.
	m = press(newPicker(base, nil), "door", "enter", "cam", "enter", "live", "enter", "enter")
	if m.result == nil || m.result.port != 554 || m.result.user != "" {
		t.Fatalf("result = %+v; want port 554 and no user", m.result)
	}
}

func TestPickerSaved(t *testing.T) {
	cams := []savedCamera{
		{Name: "open", Host: "open", Port: 554, Stream: "a"},
		{Name: "locked", Host: "locked", Port: 8554, User: "admin", Stream: "b"},
	}
	m := press(newPicker(config{}, cams), "enter")
	if m.result == nil || m.result.camera != "open" || m.result.host != "open" {
		t.Fatalf("result = %+v; want camera open", m.result)
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
	if len(m.forgotten) != 1 || len(m.cams) != 1 || m.cams[0].Name != "locked" {
		t.Fatalf("forget: cams = %+v", m.cams)
	}

	m = press(newPicker(config{}, cams), "down", "down", "enter")
	if m.screen != screenWizard || m.step != stepName {
		t.Fatalf("New camera did not open the wizard at the name step")
	}
	if m = press(m, "esc"); m.screen != screenList {
		t.Fatalf("esc from first wizard step did not return to the list")
	}

	// A new camera can't reuse a name.
	m = press(newPicker(config{}, cams), "down", "down", "enter", "OPEN", "enter")
	if m.err == "" || m.step != stepName {
		t.Fatalf("duplicate name accepted: step %d, err %q", m.step, m.err)
	}

	// Editing and renaming records the old name so it is replaced.
	m = press(newPicker(config{}, cams), "e")
	if m.screen != screenWizard || m.editing != "open" || m.inputs[stepHost].Value() != "open" {
		t.Fatalf("edit did not prefill the wizard: %+v", m.editing)
	}
	m.inputs[stepName].SetValue("porch")
	m = press(m, "enter", "enter", "enter", "enter")
	if m.result == nil || m.result.camera != "porch" || m.result.replaces != "open" {
		t.Fatalf("rename: result = %+v; want porch replacing open", m.result)
	}
}

func TestStartPicker(t *testing.T) {
	keyring.MockInit()
	cams := []savedCamera{
		{Name: "open", Host: "open", Port: 554, Stream: "a"},
		{Name: "locked", Host: "locked", Port: 554, User: "admin", Stream: "b"},
		{Name: "saved", Host: "saved", Port: 554, User: "admin", Stream: "c", RememberPassword: true},
	}
	if err := keyring.Set(keyringService, cams[2].keyringUser(), "pw"); err != nil {
		t.Fatal(err)
	}

	if m, ready := startPicker(config{}, cams); ready != nil || m.screen != screenList || m.direct {
		t.Fatalf("no name: ready %+v, screen %d; want the list", ready, m.screen)
	}
	if _, ready := startPicker(config{camera: "OPEN"}, cams); ready == nil || ready.host != "open" {
		t.Fatalf("no username: ready = %+v; want it to connect", ready)
	}
	if _, ready := startPicker(config{camera: "saved"}, cams); ready == nil || ready.pass != "pw" || ready.passChoice != passKeep {
		t.Fatalf("saved password: ready = %+v; want it to connect with pw", ready)
	}

	m, ready := startPicker(config{camera: "locked"}, cams)
	if ready != nil || m.screen != screenPassword || m.picked.Name != "locked" {
		t.Fatalf("no saved password: ready %+v, screen %d; want a password prompt", ready, m.screen)
	}
	if m = press(m, "esc"); m.result != nil || m.screen != screenPassword {
		t.Fatalf("esc should quit, not go back to a list")
	}

	m, ready = startPicker(config{camera: "garage"}, cams)
	if ready != nil || m.screen != screenWizard || m.step != stepHost || m.inputs[stepName].Value() != "garage" {
		t.Fatalf("unknown camera: screen %d step %d; want the wizard at the host step", m.screen, m.step)
	}
	m = press(m, "garage.local", "enter", "live", "enter", "enter", "enter")
	if m.result == nil || m.result.camera != "garage" || m.result.host != "garage.local" {
		t.Fatalf("unknown camera: result = %+v", m.result)
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
	cam := savedCamera{Name: "cam", Host: "cam", Port: 554, User: "admin", Stream: "live", RememberPassword: true}

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
	path, err := camerasPath()
	if err != nil {
		t.Fatal(err)
	}
	saved := func(name string) savedCamera {
		t.Helper()
		cams, err := loadCameras(path)
		if err != nil {
			t.Fatal(err)
		}
		cam, _ := findCamera(cams, name)
		return cam
	}
	cfg := config{camera: "door", host: "cam", port: 554, user: "admin", pass: "pw", stream: "live"}
	user := cameraFromConfig(cfg).keyringUser()

	cfg.passChoice = passRemember
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := keyring.Get(keyringService, user); err != nil || got != "pw" {
		t.Fatalf("keyring = %q, %v; want pw", got, err)
	}
	if !saved("door").RememberPassword {
		t.Fatal("camera not marked as having a saved password")
	}

	// Connecting with the saved password keeps it.
	cfg.passChoice = passKeep
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if !saved("door").RememberPassword {
		t.Fatal("password lost after passKeep")
	}

	// Renaming keeps the password, which is keyed by the connection.
	renamed := cfg
	renamed.camera, renamed.replaces = "porch", "door"
	if err := recordOpened(renamed); err != nil {
		t.Fatal(err)
	}
	if saved("door").Name != "" || !saved("porch").RememberPassword {
		t.Fatalf("rename: door %+v, porch %+v", saved("door"), saved("porch"))
	}

	// Changing the connection drops the old password.
	moved := renamed
	moved.host, moved.replaces = "cam2", ""
	if err := recordOpened(moved); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keyringService, user); err != keyring.ErrNotFound {
		t.Fatalf("old keyring entry after move: %v; want ErrNotFound", err)
	}
	if saved("porch").RememberPassword {
		t.Fatal("moved camera still marked as having a saved password")
	}

	cfg = moved
	cfg.passChoice = passRemember
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.passChoice = passForget
	if err := recordOpened(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keyringService, cameraFromConfig(cfg).keyringUser()); err != keyring.ErrNotFound {
		t.Fatalf("keyring after forget: %v; want ErrNotFound", err)
	}
	if saved("porch").RememberPassword {
		t.Fatal("still marked as saved after passForget")
	}
}

func TestLoadCamerasWithModel(t *testing.T) {
	// For a while detection was a choice of model, or none.
	path := filepath.Join(t.TempDir(), "cameras.json")
	old := `[
		{"name": "porch", "host": "cam", "port": 554, "stream": "a", "model": "yolox-s"},
		{"name": "garage", "host": "cam", "port": 554, "stream": "b"}
	]`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cams, err := loadCameras(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cams[0].OldDetect || cams[0].OldModel != "" || cams[1].OldDetect {
		t.Fatalf("loadCameras = %+v; want detection on for porch only", cams)
	}
}
