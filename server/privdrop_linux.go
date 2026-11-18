//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// The container starts as root so that it can take ownership of its data
// folder (Docker creates missing bind-mount folders as root), then drops to
// an unprivileged user before serving anything. Set RUN_AS_UID / RUN_AS_GID
// to choose the user; started as non-root, it runs as whoever it is.
func dropPrivileges(dataDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid := envInt("RUN_AS_UID", 65532), envInt("RUN_AS_GID", 65532)
	if uid == 0 {
		log.Printf("RUN_AS_UID is 0: running as root")
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	err := filepath.WalkDir(dataDir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
	if err != nil {
		return fmt.Errorf("could not give %s to uid %d: %w", dataDir, uid, err)
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	log.Printf("dropped root: running as uid %d, gid %d", uid, gid)
	return nil
}
