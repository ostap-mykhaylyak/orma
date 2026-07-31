/* Stato della transazione: apertura, chiusura, denominazione. */

#ifndef ORMA_TXN_H
#define ORMA_TXN_H

/* Orologi. Il monotonico misura le durate, il realtime data i timestamp:
 * usare il secondo per le durate significa produrre durate negative quando
 * ntp corregge l'ora. */
uint64_t orma_now_monotonic_nano(void);
uint64_t orma_now_unix_nano(void);

void orma_rng_seed(void);
void orma_rng_fill(uint8_t *out, size_t len);

void orma_txn_begin(void);
void orma_txn_end(void);

/* Normalizza un percorso a template, sostituendo i segmenti a cardinalita'
 * alta. Esposta per i test. */
size_t orma_normalize_path(const char *uri, size_t uri_len, char *out, size_t out_cap);

#endif /* ORMA_TXN_H */
