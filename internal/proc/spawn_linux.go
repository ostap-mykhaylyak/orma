//go:build linux

// Package proc rilancia il binario staccato dal terminale.
package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// SpawnDetached riavvia il binario corrente in una nuova sessione, in modo che
// "orma restart" possa terminare senza portarsi dietro il daemon appena avviato.
func SpawnDetached(args ...string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}

	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Il figlio vive per conto suo: non lo aspettiamo.
	_ = cmd.Process.Release()
	return cmd.Process.Pid, nil
}
