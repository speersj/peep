package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// settings are options that apply to every camera, kept in settings.json
// so they can be edited by hand. The file is created when missing, so it is
// easy to find, and peep rewrites it only when t or n is pressed in the
// video window.
type settings struct {
	Detect bool `json:"detect"` // object detection on
	Quiet  bool `json:"quiet"`  // save detections without notifying
	// MaxDetectionStorage caps the size of the saved detection images, e.g.
	// "1 GB" or "500 MB".
	MaxDetectionStorage string `json:"max_detection_storage"`
}

var defaultSettings = settings{MaxDetectionStorage: "1 GB"}

// settingsPath is where the settings file lives.
func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "peep", "settings.json"), nil
}

// loadSettings reads the settings file. Settings left out keep their
// defaults, except that detect and quiet are taken from the saved cameras,
// where they used to be kept, and written to the file, which is created if
// it is missing.
func loadSettings(path string) (settings, error) {
	s := defaultSettings
	var keys map[string]json.RawMessage
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			return defaultSettings, fmt.Errorf("parsing %s: %w", path, err)
		}
		json.Unmarshal(b, &keys)
	}
	_, hasDetect := keys["detect"]
	_, hasQuiet := keys["quiet"]
	if hasDetect && hasQuiet {
		return s, nil
	}
	old := oldCameraSettings()
	if !hasDetect {
		s.Detect = old.Detect
	}
	if !hasQuiet {
		s.Quiet = old.Quiet
	}
	return s, saveSettings(path, s)
}

// oldCameraSettings finds the detection settings of the most recently used
// camera that had detection on, from when they were kept per camera.
func oldCameraSettings() settings {
	var s settings
	path, err := camerasPath()
	if err != nil {
		return s
	}
	cams, _ := loadCameras(path)
	for _, c := range cams {
		if c.OldDetect {
			s.Detect, s.Quiet = true, c.OldQuiet
			break
		}
	}
	return s
}

// saveSettings atomically replaces the settings file.
func saveSettings(path string, s settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// currentSettings loads the settings, reporting problems on stderr.
func currentSettings() settings {
	path, err := settingsPath()
	if err != nil {
		return defaultSettings
	}
	s, err := loadSettings(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: %v\n", err)
	}
	return s
}

// changeSettings applies change to the settings file. A file that can't be
// read is left alone rather than replaced.
func changeSettings(change func(*settings)) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	s, err := loadSettings(path)
	if err != nil {
		return err
	}
	change(&s)
	return saveSettings(path, s)
}

// maxDetectionBytes is the detection storage cap from the settings file, or
// the default if the file or the value can't be read, which is reported on
// stderr.
func maxDetectionBytes() int64 {
	def, _ := parseSize(defaultSettings.MaxDetectionStorage)
	n, err := parseSize(currentSettings().MaxDetectionStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peep: settings: max_detection_storage: %v; using %s\n", err, defaultSettings.MaxDetectionStorage)
		return def
	}
	return n
}

// sizeUnits are the multipliers for sizes. Like file managers on Linux,
// KB, MB and GB are taken as powers of 1024.
var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
}

// parseSize parses a size such as "1 GB", "500MB" or "1.5g" into bytes.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(t)
	}
	num, unit := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	v, err := strconv.ParseFloat(num, 64)
	mult, ok := sizeUnits[unit]
	if err != nil || !ok || v <= 0 {
		return 0, fmt.Errorf("invalid size %q; use e.g. \"1 GB\" or \"500 MB\"", s)
	}
	return int64(v * float64(mult)), nil
}
