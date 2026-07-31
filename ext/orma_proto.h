/* Serializzazione del frame agent to daemon. Vedi DESIGN.md §3. */

#ifndef ORMA_PROTO_H
#define ORMA_PROTO_H

/* Il buffer vive quanto il processo: allocato alla prima richiesta, riusato
 * dalle successive, liberato allo spegnimento del worker. */
void orma_buf_reset(orma_buf *b);
void orma_buf_free(orma_buf *b);

/* Codifica la transazione in un frame completo, lunghezza inclusa.
 * Restituisce false solo se l'allocazione fallisce. */
bool orma_proto_encode(const orma_txn *txn, orma_buf *out);

#endif /* ORMA_PROTO_H */
