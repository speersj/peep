package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1 GB":   1 << 30,
		"500MB":  500 << 20,
		"1.5g":   3 << 29,
		" 2 TiB": 2 << 40,
		"4096":   4096,
		"10 kb":  10 << 10,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GB", "0 GB", "-1 GB", "1 PB", "1 G B", "lots"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d; want an error", in, got)
		}
	}
}

func TestLoadSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadSettings(path)
	if err != nil || s != defaultSettings {
		t.Fatalf("missing file: %+v, %v; want defaults", s, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("defaults not written: %v", err)
	}
	want := "{\n  \"detect\": false,\n  \"quiet\": false,\n  \"max_detection_storage\": \"1 GB\"\n}\n"
	if string(b) != want {
		t.Errorf("written file = %q; want %q", b, want)
	}

	os.WriteFile(path, []byte(`{"detect": true, "quiet": false, "max_detection_storage": "250 MB"}`), 0o600)
	if s, err := loadSettings(path); err != nil || !s.Detect || s.MaxDetectionStorage != "250 MB" {
		t.Errorf("edited file: %+v, %v", s, err)
	}
	os.WriteFile(path, []byte(`{"detect": false, "quiet": true}`), 0o600)
	if s, err := loadSettings(path); err != nil || s != (settings{Quiet: true, MaxDetectionStorage: "1 GB"}) {
		t.Errorf("storage left out: %+v, %v; want the default storage", s, err)
	}
	os.WriteFile(path, []byte(`{oops`), 0o600)
	if s, err := loadSettings(path); err == nil || s != defaultSettings {
		t.Errorf("broken file: %+v, %v; want defaults and an error", s, err)
	}
	if err := changeSettings(func(s *settings) { s.Detect = true }); err == nil {
		t.Error("changeSettings replaced a broken file")
	}

	os.Remove(path)
	if err := changeSettings(func(s *settings) { s.Quiet = true }); err != nil {
		t.Fatal(err)
	}
	if s, err := loadSettings(path); err != nil || !s.Quiet || s.Detect {
		t.Errorf("after changeSettings: %+v, %v; want quiet only", s, err)
	}
}

func TestSettingsFromCameras(t *testing.T) {
	// Detection used to be set per camera. The most recently used camera
	// with it on decides, and the old fields are dropped when the cameras
	// are next saved.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cams, _ := camerasPath()
	path, _ := settingsPath()
	os.MkdirAll(filepath.Dir(cams), 0o700)
	old := `[
		{"name": "garage", "host": "cam", "port": 554, "stream": "b"},
		{"name": "porch", "host": "cam", "port": 554, "stream": "a", "detect": true, "quiet": true},
		{"name": "yard", "host": "cam", "port": 554, "stream": "c", "detect": true}
	]`
	if err := os.WriteFile(cams, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSettings(path)
	if err != nil || !s.Detect || !s.Quiet {
		t.Fatalf("loadSettings = %+v, %v; want detection on and quiet", s, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"quiet": true`) {
		t.Fatalf("settings not written: %s", b)
	}

	saved, _ := loadCameras(cams)
	if err := saveCameras(cams, saved); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cams); strings.Contains(string(b), "detect") || strings.Contains(string(b), "quiet") {
		t.Fatalf("old settings saved again: %s", b)
	}
	if s, err := loadSettings(path); err != nil || !s.Detect || !s.Quiet {
		t.Fatalf("after saving cameras: %+v, %v; want the settings kept", s, err)
	}
}
