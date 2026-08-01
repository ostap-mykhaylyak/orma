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

/* Registra uno span gia' concluso, con identificativi e tempi decisi dal
 * chiamante. La usa l'observer, che sa solo alla fine se lo span va emesso. */
void orma_span_record(const char *name, size_t name_len, uint8_t kind,
                      const uint8_t span_id[ORMA_SPAN_ID_LEN],
                      const uint8_t parent_id[ORMA_SPAN_ID_LEN],
                      uint64_t start_unix_nano, uint64_t duration_nano,
                      uint8_t status, uint32_t chiamate);

void orma_span_attr_str(int idx, const char *key, const char *value, size_t len);
void orma_span_attr_int(int idx, const char *key, int64_t value);

/* Chiude gli span rimasti aperti, per esempio perche' la funzione avvolta ha
 * fatto bailout e non e' mai tornata. */
void orma_spans_close_open(void);

/* Puntatore a una stringa nell'arena. Valido solo fino alla prossima scrittura
 * nell'arena, che puo' rilocarla. */
const char *orma_arena_str(uint32_t off);

/* Copia una stringa nell'arena restituendone offset e lunghezza. */
bool orma_arena_copy(const char *s, size_t len, uint32_t *off, uint32_t *out_len);

#endif /* ORMA_SPAN_H */
