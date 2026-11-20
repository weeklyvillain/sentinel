//go:build !linux

package main

import "os/exec"

// asAgent: running Claude as another user needs Linux; elsewhere (local
// development) it runs as the current user.
func asAgent(*exec.Cmd, string) error { return nil }
