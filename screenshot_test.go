package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateScreenshot(t *testing.T) {
	dir := t.TempDir()
	// The name uses the timestamp's own zone, not UTC.
	zone := time.FixedZone("test", -7*60*60)
	ts := time.Date(2026, 10, 3, 9, 5, 59, 0, time.UTC).In(zone)
	want := []string{"peep-2026-10-03-02-05.png", "peep-2026-10-03-02-05-2.png", "peep-2026-10-03-02-05-3.png"}
	for _, name := range want {
		f, err := createScreenshot(dir, "", ts)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if got := filepath.Base(f.Name()); got != name {
			t.Errorf("created %s; want %s", got, name)
		}
	}
	// With detection running, the model is named.
	f, err := createScreenshot(dir, "dfine-s", ts)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, want := filepath.Base(f.Name()), "peep-dfine-s-2026-10-03-02-05.png"; got != want {
		t.Errorf("created %s; want %s", got, want)
	}
}

func TestWriteScreenshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	img.Pix[0], img.Pix[3] = 200, 255
	path, err := writeScreenshot(img, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(home, "Pictures") {
		t.Fatalf("saved to %s; want it in ~/Pictures", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != img.Bounds() {
		t.Fatalf("bounds = %v; want %v", got.Bounds(), img.Bounds())
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r>>8 != 200 {
		t.Fatalf("pixel red = %d; want 200", r>>8)
	}
}

func TestClipboardCommand(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	tests := []struct {
		goos      string
		env       map[string]string
		wantCmd   string
		wantStdin bool
		wantErr   bool
	}{
		{goos: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0"}, wantCmd: "wl-copy", wantStdin: true},
		{goos: "linux", env: map[string]string{"DISPLAY": ":0"}, wantCmd: "xclip", wantStdin: true},
		{goos: "freebsd", env: map[string]string{"DISPLAY": ":0"}, wantCmd: "xclip", wantStdin: true},
		{goos: "linux", env: nil, wantErr: true},
		{goos: "darwin", env: nil, wantCmd: "osascript"},
		{goos: "windows", env: nil, wantErr: true},
	}
	for _, tt := range tests {
		args, stdin, err := clipboardCommand(tt.goos, env(tt.env), "/tmp/shot.png")
		if (err != nil) != tt.wantErr {
			t.Errorf("%s %v: error = %v, wantErr %v", tt.goos, tt.env, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (args[0] != tt.wantCmd || stdin != tt.wantStdin) {
			t.Errorf("%s %v: got %v stdin=%v; want %s stdin=%v", tt.goos, tt.env, args, stdin, tt.wantCmd, tt.wantStdin)
		}
	}
}
