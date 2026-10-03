//go:build linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	notifyDest  = "org.freedesktop.Notifications"
	notifyPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	notifyIface = "org.freedesktop.Notifications"

	// notifyEnv tells a peep process to run as a screenshot notifier.
	notifyEnv       = "PEEP_NOTIFY_SCREENSHOT"
	notifyCopiedEnv = "PEEP_NOTIFY_COPIED"

	// notifyWait is how long a notifier waits for its notification to be
	// clicked before giving up.
	notifyWait = time.Hour
)

// notifyScreenshot announces a saved screenshot with a desktop notification
// that opens it when clicked. A detached copy of peep owns the notification,
// so clicking it still works after this process exits.
func notifyScreenshot(path string, copied bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), notifyEnv+"="+path)
	if copied {
		cmd.Env = append(cmd.Env, notifyCopiedEnv+"=1")
	}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal closing
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it exits while peep is still running
	return nil
}

// runNotifierIfRequested runs the notifier and exits when this process was
// started by notifyScreenshot.
func runNotifierIfRequested() {
	path := os.Getenv(notifyEnv)
	if path == "" {
		return
	}
	if err := runNotifier(path, os.Getenv(notifyCopiedEnv) != ""); err != nil {
		fmt.Fprintf(os.Stderr, "peep: screenshot notification: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runNotifier shows the notification and waits for it to be clicked,
// dismissed, or to time out.
func runNotifier(path string, copied bool) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	// Subscribe before sending so a quick click is not missed.
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(notifyPath), dbus.WithMatchInterface(notifyIface)); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)

	obj := conn.Object(notifyDest, notifyPath)
	var caps []string
	if err := obj.Call(notifyIface+".GetCapabilities", 0).Store(&caps); err != nil {
		return err
	}
	clickable := slices.Contains(caps, "actions")

	var actions []string
	if clickable {
		actions = []string{"default", "Open"}
	}
	body := filepath.Base(path)
	if copied {
		body += "\nCopied to the clipboard"
	}
	hints := map[string]dbus.Variant{
		"image-path": dbus.MakeVariant("file://" + path), // thumbnail, where supported
		"category":   dbus.MakeVariant("transfer.complete"),
	}
	var id uint32
	err = obj.Call(notifyIface+".Notify", 0,
		"peep", uint32(0), "camera-photo", "Screenshot saved", body, actions, hints, int32(-1),
	).Store(&id)
	if err != nil || !clickable {
		return err
	}

	timeout := time.After(notifyWait)
	var token string // lets the viewer take focus on Wayland
	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				return nil
			}
			if len(sig.Body) < 2 || sig.Body[0] != id {
				continue
			}
			switch sig.Name {
			case notifyIface + ".ActivationToken":
				token, _ = sig.Body[1].(string)
			case notifyIface + ".ActionInvoked":
				return openFile(path, token)
			case notifyIface + ".NotificationClosed":
				return nil
			}
		case <-timeout:
			return nil
		}
	}
}

// openFile opens path in the desktop's default application for it.
func openFile(path, activationToken string) error {
	cmd := exec.Command("xdg-open", path)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, notifyEnv+"=") || strings.HasPrefix(kv, notifyCopiedEnv+"=")
	})
	if activationToken != "" {
		env = append(env, "XDG_ACTIVATION_TOKEN="+activationToken, "DESKTOP_STARTUP_ID="+activationToken)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	return cmd.Process.Release()
}
