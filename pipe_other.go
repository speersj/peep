//go:build !linux

package main

import "os"

func setPipeSize(*os.File, int) {}
