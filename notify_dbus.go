//go:build linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
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

	// notifyEnv tells a peep process to run as a notifier for an image; the
	// others carry the notification's text.
	notifyEnv         = "PEEP_NOTIFY_IMAGE"
	notifySummaryEnv  = "PEEP_NOTIFY_SUMMARY"
	notifyBodyEnv     = "PEEP_NOTIFY_BODY"
	notifyCategoryEnv = "PEEP_NOTIFY_CATEGORY"

	// notifyWait is how long a notifier waits for its notification to be
	// clicked before giving up.
	notifyWait = time.Hour
)

// notifyImage shows a desktop notification about the image at n.image,
// with a thumbnail of it, that opens the image when clicked. A detached copy
// of peep owns the notification, so clicking it still works after this
// process exits.
func notifyImage(n notification) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		notifyEnv+"="+n.image,
		notifySummaryEnv+"="+n.summary,
		notifyBodyEnv+"="+n.body,
		notifyCategoryEnv+"="+n.category,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal closing
	// Relay its errors through a pipe rather than handing it peep's stderr,
	// which it would hold open after peep exits, stalling a pipeline such
	// as peep 2>&1 | tee log.
	errs, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		io.Copy(os.Stderr, errs)
		cmd.Wait() // reap it if it exits while peep is still running
	}()
	return nil
}

// runNotifierIfRequested runs the notifier and exits when this process was
// started by notifyImage.
func runNotifierIfRequested() {
	n := notification{
		image:    os.Getenv(notifyEnv),
		summary:  os.Getenv(notifySummaryEnv),
		body:     os.Getenv(notifyBodyEnv),
		category: os.Getenv(notifyCategoryEnv),
	}
	if n.image == "" {
		return
	}
	if err := runNotifier(n); err != nil {
		fmt.Fprintf(os.Stderr, "peep: notification: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runNotifier shows the notification and waits for it to be clicked,
// dismissed, or to time out.
func runNotifier(n notification) error {
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
	hints := map[string]dbus.Variant{
		"image-path": dbus.MakeVariant("file://" + n.image), // thumbnail, where supported
	}
	if n.category != "" {
		hints["category"] = dbus.MakeVariant(n.category)
	}
	var id uint32
	err = obj.Call(notifyIface+".Notify", 0,
		"peep", uint32(0), "camera-photo", n.summary, n.body, actions, hints, int32(-1),
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
				return openFile(n.image, token)
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
		return strings.HasPrefix(kv, "PEEP_NOTIFY_")
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
