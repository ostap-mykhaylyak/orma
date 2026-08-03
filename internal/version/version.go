// Package version espone la versione del daemon.
package version

// Version viene sovrascritta in fase di build con -ldflags.
// Deve restare allineata a PHP_ORMA_VERSION in ext/php_orma.h: l'estensione e il
// daemon si rifiutano di parlarsi se le versioni maggiori divergono.
var Version = "0.1.2"

// Commit viene sovrascritto in fase di build con -ldflags.
var Commit = "sviluppo"
