package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// asAgent runs cmd as FIXER_AGENT_UID (the container's "agent" user), so
// Claude Code cannot read the fixer's or git's process environment (which
// hold FIXER_TOKEN and the repository token) through /proc.
func asAgent(cmd *exec.Cmd, dir string) error {
	uid, err := strconv.Atoi(os.Getenv("FIXER_AGENT_UID"))
	if err != nil || uid <= 0 {
		return nil
	}
	gid := uid
	if g, err := strconv.Atoi(os.Getenv("FIXER_AGENT_GID")); err == nil && g > 0 {
		gid = g
	}
	if err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	}); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	return nil
}
