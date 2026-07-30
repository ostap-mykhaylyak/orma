//go:build !linux

package proc

import "errors"

// SpawnDetached esiste solo per far compilare il pacchetto altrove: orma gira
// esclusivamente su Linux.
func SpawnDetached(args ...string) (int, error) {
	return 0, errors.New("il riavvio staccato e' supportato solo su Linux")
}
