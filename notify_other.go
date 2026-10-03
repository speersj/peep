//go:build !(linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "errors"

// notifyScreenshot is only implemented for freedesktop.org (D-Bus) desktops.
func notifyScreenshot(string, bool) error {
	return errors.New("desktop notifications are not supported on this platform")
}

func runNotifierIfRequested() {}
