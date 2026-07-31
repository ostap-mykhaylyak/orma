/* Offuscamento e classificazione delle query SQL. */

#ifndef ORMA_SQL_H
#define ORMA_SQL_H

/* Sostituisce letterali e numeri con "?", rimuove i commenti e comprime gli
 * spazi. Restituisce la lunghezza scritta in out.
 *
 * L'offuscamento avviene qui, nel processo PHP, e non nel daemon: cosi' i
 * valori dei parametri non lasciano mai il processo che li ha in mano. Il
 * daemon puo' solo normalizzare ulteriormente cio' che gli arriva. */
size_t orma_sql_obfuscate(const char *sql, size_t len, char *out, size_t out_cap);

/* Prima parola chiave della query: SELECT, INSERT, UPDATE, DELETE, ...
 * Restituisce una stringa statica in maiuscolo, "SQL" se non riconosciuta. */
const char *orma_sql_operation(const char *sql, size_t len);

#endif /* ORMA_SQL_H */
