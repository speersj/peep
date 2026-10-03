package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	defaultPort    = 554
	maxSavedCams   = 20
	keyringService = "peep"
)

// passChoice says what to do with a camera's password once it opens.
type passChoice int

const (
	passKeep     passChoice = iota // leave any remembered password alone
	passRemember                   // store it in the system keyring
	passForget                     // remove it from the system keyring
)

// parseHost splits "host", "host:port", "[v6addr]:port" or a bare IPv6
// address into its host and port, defaulting the port to 554.
func parseHost(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, errors.New("host is empty")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// No port: a plain name, a bare IPv6 address, or "[v6addr]".
		if strings.Count(s, ":") == 1 && !strings.HasPrefix(s, "[") {
			return "", 0, fmt.Errorf("invalid host %q: %v", s, err)
		}
		host, portStr = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), ""
	}
	if host == "" {
		return "", 0, fmt.Errorf("invalid host %q: missing hostname", s)
	}
	if portStr == "" {
		return host, defaultPort, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q in host %q", portStr, s)
	}
	return host, port, nil
}

// savedCamera is a camera that was opened successfully, known by its Name.
// Passwords never go in the cameras file: when RememberPassword is set, the
// password is in the system keyring under keyringService and keyringUser.
type savedCamera struct {
	Name             string    `json:"name"`
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	User             string    `json:"user,omitempty"`
	Stream           string    `json:"stream"`
	Detect           bool      `json:"detect,omitempty"` // object detection on
	RememberPassword bool      `json:"remember_password,omitempty"`
	LastUsed         time.Time `json:"last_used"`
}

// is reports whether the camera is called name, ignoring case.
func (c savedCamera) is(name string) bool { return strings.EqualFold(c.Name, name) }

// validCameraName checks a name for a new camera, or for renaming the
// camera currently called except.
func validCameraName(name string, cams []savedCamera, except string) error {
	switch {
	case name == "":
		return errors.New("a camera name is required, e.g. frontdoor")
	case strings.HasPrefix(name, "-"):
		return errors.New("camera names can't start with '-'")
	}
	for _, c := range cams {
		if c.is(name) && !c.is(except) {
			return fmt.Errorf("a camera named %q already exists", c.Name)
		}
	}
	return nil
}

// addr renders host[:port] in the form parseHost accepts, leaving out the
// default port.
func (c savedCamera) addr() string {
	if c.Port != defaultPort || strings.Contains(c.Host, ":") {
		return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	return c.Host
}

// addrOrEmpty is addr, or "" for a camera with no host yet.
func (c savedCamera) addrOrEmpty() string {
	if c.Host == "" {
		return ""
	}
	return c.addr()
}

// label renders the camera's connection as [user@]host[:port]/stream.
func (c savedCamera) label() string {
	addr := c.addr()
	if c.User != "" {
		addr = c.User + "@" + addr
	}
	return addr + "/" + strings.TrimPrefix(c.Stream, "/")
}

// keyringUser is the account name the camera's password is stored under. It
// is the connection rather than the name, so renaming keeps the password.
func (c savedCamera) keyringUser() string { return c.label() }

// storedPassword fetches the camera's password from the system keyring.
func (c savedCamera) storedPassword() (string, error) {
	return keyring.Get(keyringService, c.keyringUser())
}

// dropPassword removes the camera's password from the system keyring.
func (c savedCamera) dropPassword() error {
	err := keyring.Delete(keyringService, c.keyringUser())
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (c savedCamera) config(base config) config {
	base.camera, base.host, base.port, base.user, base.stream = c.Name, c.Host, c.Port, c.User, c.Stream
	base.detect = c.Detect
	return base
}

func cameraFromConfig(cfg config) savedCamera {
	return savedCamera{Name: cfg.camera, Host: cfg.host, Port: cfg.port, User: cfg.user, Stream: cfg.stream, Detect: cfg.detect}
}

// camerasPath is where previously opened cameras are remembered.
func camerasPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "peep", "cameras.json"), nil
}

// loadCameras reads the saved cameras, most recently used first. A missing
// file is not an error.
func loadCameras(path string) ([]savedCamera, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cams []savedCamera
	if err := json.Unmarshal(b, &cams); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	migrateCameras(cams)
	return cams, nil
}

// migrateCameras upgrades entries saved before cameras had names, when
// "name" held the stream path, naming each after its host.
func migrateCameras(cams []savedCamera) {
	for i := range cams {
		if cams[i].Stream != "" || cams[i].Name == "" {
			continue
		}
		cams[i].Stream, cams[i].Name = cams[i].Name, ""
		name := cams[i].Host
		for n := 2; validCameraName(name, cams, "") != nil; n++ {
			name = fmt.Sprintf("%s-%d", cams[i].Host, n)
		}
		cams[i].Name = name
	}
}

// saveCameras atomically replaces the saved camera list.
func saveCameras(path string, cams []savedCamera) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cams, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cameras-*.json")
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

// rememberCamera moves cam to the front of cams, replacing the camera of the
// same name, or the one called replaces when it was renamed, and trims the
// list to maxSavedCams.
func rememberCamera(cams []savedCamera, cam savedCamera, replaces string) []savedCamera {
	out := []savedCamera{cam}
	for _, c := range cams {
		if !c.is(cam.Name) && !(replaces != "" && c.is(replaces)) && len(out) < maxSavedCams {
			out = append(out, c)
		}
	}
	return out
}

// forgetCamera removes the camera called name from cams.
func forgetCamera(cams []savedCamera, name string) []savedCamera {
	var out []savedCamera
	for _, c := range cams {
		if !c.is(name) {
			out = append(out, c)
		}
	}
	return out
}

// findCamera returns the camera called name, if any.
func findCamera(cams []savedCamera, name string) (savedCamera, bool) {
	for _, c := range cams {
		if c.is(name) {
			return c, true
		}
	}
	return savedCamera{}, false
}

// recordOpened remembers cfg as the most recently opened camera, storing or
// dropping its password in the system keyring as cfg.passChoice asks.
func recordOpened(cfg config) error {
	path, err := camerasPath()
	if err != nil {
		return err
	}
	cams, err := loadCameras(path)
	if err != nil {
		return err
	}
	cam := cameraFromConfig(cfg)
	cam.LastUsed = time.Now()
	prevName := cfg.replaces
	if prevName == "" {
		prevName = cam.Name
	}
	var keyErr error
	if prev, ok := findCamera(cams, prevName); ok && prev.RememberPassword {
		if prev.keyringUser() == cam.keyringUser() {
			cam.RememberPassword = true
		} else if err := prev.dropPassword(); err != nil {
			// The connection changed, so the old password no longer applies.
			keyErr = fmt.Errorf("removing old password from keyring: %w", err)
		}
	}
	switch {
	case cfg.passChoice == passRemember && cfg.user != "":
		if err := keyring.Set(keyringService, cam.keyringUser(), cfg.pass); err == nil {
			cam.RememberPassword = true
		} else {
			keyErr = errors.Join(keyErr, fmt.Errorf("saving password to keyring: %w", err))
		}
	case cfg.passChoice == passForget && cam.RememberPassword:
		if err := cam.dropPassword(); err == nil {
			cam.RememberPassword = false
		} else {
			keyErr = errors.Join(keyErr, fmt.Errorf("removing password from keyring: %w", err))
		}
	}
	return errors.Join(keyErr, saveCameras(path, rememberCamera(cams, cam, cfg.replaces)))
}
