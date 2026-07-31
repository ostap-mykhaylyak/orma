/* Span figli: arena, apertura e chiusura. */

#ifndef ORMA_SPAN_H
#define ORMA_SPAN_H

void orma_spans_reset(void);
void orma_spans_free(void);

/* Apre uno span e ne restituisce l'indice, oppure -1 se non c'e' piu' posto.
 * Tutte le funzioni sotto accettano -1 senza fare nulla, cosi' il chiamante
 * non deve controllare. */
int  orma_span_open(const char *name, size_t name_len, uint8_t kind);
void orma_span_close(int idx, uint8_t status);

void orma_span_attr_str(int idx, const char *key, const char *value, size_t len);
void orma_span_attr_int(int idx, const char *key, int64_t value);

/* Chiude gli span rimasti aperti, per esempio perche' la funzione avvolta ha
 * fatto bailout e non e' mai tornata. */
void orma_spans_close_open(void);

/* Puntatore a una stringa nell'arena. Valido solo fino alla prossima scrittura
 * nell'arena, che puo' rilocarla. */
const char *orma_arena_str(uint32_t off);

#endif /* ORMA_SPAN_H */
