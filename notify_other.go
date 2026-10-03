//go:build !(linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "errors"

// notifyImage is only implemented for freedesktop.org (D-Bus) desktops.
func notifyImage(notification) error {
	return errors.New("desktop notifications are not supported on this platform")
}

func runNotifierIfRequested() {}
