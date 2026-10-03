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

// savedCamera is a camera that was opened successfully. Passwords never go
// in the cameras file: when RememberPassword is set, the password is in the
// system keyring under keyringService and keyringUser.
type savedCamera struct {
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	User             string    `json:"user,omitempty"`
	Name             string    `json:"name"`
	RememberPassword bool      `json:"remember_password,omitempty"`
	LastUsed         time.Time `json:"last_used"`
}

func (c savedCamera) sameAs(o savedCamera) bool {
	return c.Host == o.Host && c.Port == o.Port && c.User == o.User && c.Name == o.Name
}

// addr renders host[:port] in the form parseHost accepts, leaving out the
// default port.
func (c savedCamera) addr() string {
	if c.Port != defaultPort || strings.Contains(c.Host, ":") {
		return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	return c.Host
}

// label renders the camera as [user@]host[:port]/name.
func (c savedCamera) label() string {
	addr := c.addr()
	if c.User != "" {
		addr = c.User + "@" + addr
	}
	return addr + "/" + strings.TrimPrefix(c.Name, "/")
}

// keyringUser is the account name the camera's password is stored under.
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
	base.host, base.port, base.user, base.name = c.Host, c.Port, c.User, c.Name
	return base
}

func cameraFromConfig(cfg config) savedCamera {
	return savedCamera{Host: cfg.host, Port: cfg.port, User: cfg.user, Name: cfg.name}
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
	return cams, nil
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

// rememberCamera moves cam to the front of cams, dropping any older copy and
// trimming the list to maxSavedCams.
func rememberCamera(cams []savedCamera, cam savedCamera) []savedCamera {
	out := []savedCamera{cam}
	for _, c := range cams {
		if !c.sameAs(cam) && len(out) < maxSavedCams {
			out = append(out, c)
		}
	}
	return out
}

// forgetCamera removes cam from cams.
func forgetCamera(cams []savedCamera, cam savedCamera) []savedCamera {
	var out []savedCamera
	for _, c := range cams {
		if !c.sameAs(cam) {
			out = append(out, c)
		}
	}
	return out
}

// findCamera returns the saved entry matching cam, if any.
func findCamera(cams []savedCamera, cam savedCamera) (savedCamera, bool) {
	for _, c := range cams {
		if c.sameAs(cam) {
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
	if prev, ok := findCamera(cams, cam); ok {
		cam.RememberPassword = prev.RememberPassword
	}
	var keyErr error
	switch {
	case cfg.passChoice == passRemember && cfg.user != "":
		if keyErr = keyring.Set(keyringService, cam.keyringUser(), cfg.pass); keyErr == nil {
			cam.RememberPassword = true
		} else {
			keyErr = fmt.Errorf("saving password to keyring: %w", keyErr)
		}
	case cfg.passChoice == passForget && cam.RememberPassword:
		if keyErr = cam.dropPassword(); keyErr == nil {
			cam.RememberPassword = false
		} else {
			keyErr = fmt.Errorf("removing password from keyring: %w", keyErr)
		}
	}
	return errors.Join(keyErr, saveCameras(path, rememberCamera(cams, cam)))
}

// rememberedPassword looks up the keyring password for cfg's camera, if it
// was saved with one.
func rememberedPassword(cfg config) (string, bool) {
	path, err := camerasPath()
	if err != nil {
		return "", false
	}
	cams, err := loadCameras(path)
	if err != nil {
		return "", false
	}
	cam, ok := findCamera(cams, cameraFromConfig(cfg))
	if !ok || !cam.RememberPassword {
		return "", false
	}
	pass, err := cam.storedPassword()
	return pass, err == nil
}
