// Package pidfile gestisce il file che traccia l'istanza in esecuzione.
package pidfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrNotRunning indica che nessuna istanza risulta attiva.
var ErrNotRunning = errors.New("nessuna istanza in esecuzione")

// File e' un pidfile acquisito da questo processo.
type File struct {
	path string
}

// Acquire scrive il pid corrente. Se il file esiste ma il processo indicato non
// e' piu' vivo, il pidfile viene considerato residuo e sovrascritto: una macchina
// che si e' spenta male non deve impedire il riavvio.
func Acquire(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creazione della directory del pidfile: %w", err)
	}

	if pid, err := Read(path); err == nil {
		if Running(pid) {
			return nil, fmt.Errorf("orma e' gia' in esecuzione con pid %d", pid)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("rimozione del pidfile residuo: %w", err)
		}
	}

	content := strconv.Itoa(os.Getpid()) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("scrittura del pidfile: %w", err)
	}
	return &File{path: path}, nil
}

// Release rimuove il pidfile.
func (f *File) Release() error {
	if f == nil {
		return nil
	}
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Read restituisce il pid registrato nel file.
func Read(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotRunning
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("%s non contiene un pid valido", path)
	}
	return pid, nil
}

// Running verifica che il processo esista, inviandogli il segnale 0.
func Running(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Signal invia un segnale all'istanza registrata nel pidfile.
func Signal(path string, sig os.Signal) (int, error) {
	pid, err := Read(path)
	if err != nil {
		return 0, err
	}
	if !Running(pid) {
		return pid, ErrNotRunning
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return pid, err
	}
	return pid, p.Signal(sig)
}
