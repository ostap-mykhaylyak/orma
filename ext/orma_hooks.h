/* Instrumentazione delle funzioni interne di PHP. */

#ifndef ORMA_HOOKS_H
#define ORMA_HOOKS_H

/* Sostituisce gli handler delle funzioni note. Va chiamata quando tutte le
 * estensioni sono registrate, cioe' alla prima richiesta e non in MINIT:
 * l'ordine di caricamento delle estensioni non e' garantito, e PDO o curl
 * potrebbero non esistere ancora. Idempotente. */
void orma_hooks_install(void);

#endif /* ORMA_HOOKS_H */
