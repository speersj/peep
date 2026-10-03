package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const clipboardTimeout = 5 * time.Second

// clipboardCommand returns the command that puts the PNG at path on the
// system clipboard, and whether the PNG is fed to it on stdin.
func clipboardCommand(goos string, getenv func(string) string, path string) (args []string, stdin bool, err error) {
	switch goos {
	case "darwin":
		script := fmt.Sprintf("set the clipboard to (read (POSIX file %q) as «class PNGf»)", path)
		return []string{"osascript", "-e", script}, false, nil
	case "windows", "plan9", "js", "wasip1", "ios", "android":
		return nil, false, fmt.Errorf("copying images to the clipboard is not supported on %s", goos)
	}
	if getenv("WAYLAND_DISPLAY") != "" {
		return []string{"wl-copy", "--type", "image/png"}, true, nil
	}
	if getenv("DISPLAY") != "" {
		return []string{"xclip", "-selection", "clipboard", "-target", "image/png", "-in"}, true, nil
	}
	return nil, false, errors.New("no Wayland or X11 display to copy to")
}

// copyImageFile puts the PNG at path on the system clipboard.
func copyImageFile(path string) error {
	args, stdin, err := clipboardCommand(runtime.GOOS, os.Getenv, path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if stdin {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd.Stdin = f
	}
	// stdout and stderr stay unset: wl-copy and xclip fork a process that
	// serves the clipboard after they exit, and a pipe it inherited would
	// keep Run waiting until something else is copied.
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s is not installed", args[0])
		}
		return fmt.Errorf("%s: %w", args[0], err)
	}
	return nil
}
