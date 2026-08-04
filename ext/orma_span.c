/* Span figli.
 *
 * L'array degli span e l'arena delle stringhe vivono quanto il processo e
 * vengono azzerati a ogni richiesta: nessuna allocazione nel percorso caldo,
 * salvo la prima volta e quando l'array cresce.
 *
 * I nomi e i valori stringa stanno nell'arena e sono referenziati per offset,
 * non per puntatore: una realloc dell'arena sposta la memoria, e un puntatore
 * salvato prima diventerebbe penzolante.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_orma.h"
#include "orma_span.h"
#include "orma_proto.h"
#include "orma_txn.h"
#include "orma_observer.h"

#include <string.h>

#define ORMA_SPANS_INITIAL 64

static bool orma_spans_room(void)
{
	uint32_t count = ORMA_G(span_count);

	if (count < ORMA_G(span_cap)) {
		return true;
	}
	if (count >= ORMA_MAX_SPANS) {
		return false;
	}

	uint32_t want = ORMA_G(span_cap) ? ORMA_G(span_cap) * 2 : ORMA_SPANS_INITIAL;
	if (want > ORMA_MAX_SPANS) {
		want = ORMA_MAX_SPANS;
	}

	orma_span *grown = perealloc(ORMA_G(spans), sizeof(orma_span) * want, 1);
	if (grown == NULL) {
		return false;
	}
	ORMA_G(spans) = grown;
	ORMA_G(span_cap) = want;
	return true;
}

void orma_spans_reset(void)
{
	ORMA_G(span_count) = 0;
	ORMA_G(file_count) = 0;
	orma_buf_reset(&ORMA_G(arena));
}

void orma_spans_free(void)
{
	if (ORMA_G(spans) != NULL) {
		pefree(ORMA_G(spans), 1);
		ORMA_G(spans) = NULL;
	}
	ORMA_G(span_cap) = 0;
	ORMA_G(span_count) = 0;
	orma_buf_free(&ORMA_G(arena));
}

const char *orma_arena_str(uint32_t off)
{
	return ORMA_G(arena).data + off;
}

/* Copia una stringa nell'arena. Restituisce false se l'allocazione fallisce:
 * in quel caso il chiamante lascia il campo vuoto invece di perdere lo span. */
bool orma_arena_copy(const char *s, size_t len, uint32_t *off, uint32_t *out_len)
{
	if (s == NULL || len == 0) {
		*off = 0;
		*out_len = 0;
		return true;
	}
	if (len > 0xFFFF) {
		len = 0xFFFF;
	}

	*off = (uint32_t)ORMA_G(arena).len;
	if (!orma_buf_append(&ORMA_G(arena), s, len)) {
		*off = 0;
		*out_len = 0;
		return false;
	}
	*out_len = (uint32_t)len;
	return true;
}

int orma_span_open(const char *name, size_t name_len, uint8_t kind)
{
	if (!ORMA_G(txn).active) {
		return -1;
	}
	if (!orma_spans_room()) {
		ORMA_G(txn).spans_dropped++;
		return -1;
	}

	uint32_t idx = ORMA_G(span_count)++;
	orma_span *span = &ORMA_G(spans)[idx];

	memset(span, 0, sizeof(*span));
	orma_rng_fill(span->span_id, ORMA_SPAN_ID_LEN);
	/* Sotto la funzione utente che sta eseguendo, non appesa alla radice:
	 * cosi' nel waterfall la query sta dentro il metodo che l'ha lanciata. */
	orma_observer_current_parent(span->parent_span_id);
	orma_arena_copy(name, name_len, &span->name_off, &span->name_len);

	span->kind = kind;
	span->status = ORMA_STATUS_OK;
	span->open = true;
	span->start_unix_nano = orma_now_unix_nano();
	span->start_monotonic_nano = orma_now_monotonic_nano();

	/* Le query e le chiamate di rete partono da un punto preciso del codice, e
	 * senza quel punto un elenco di SELECT non dice da dove vengono. */
	orma_span_posizioni((int)idx, NULL, EG(current_execute_data));

	return (int)idx;
}

static orma_span *orma_span_at(int idx)
{
	if (idx < 0 || (uint32_t)idx >= ORMA_G(span_count)) {
		return NULL;
	}
	return &ORMA_G(spans)[idx];
}

void orma_span_close(int idx, uint8_t status)
{
	orma_span *span = orma_span_at(idx);
	if (span == NULL || !span->open) {
		return;
	}

	uint64_t now = orma_now_monotonic_nano();
	span->duration_nano = (now > span->start_monotonic_nano)
	                    ? now - span->start_monotonic_nano : 0;
	span->status = status;
	span->open = false;
}

void orma_spans_close_open(void)
{
	for (uint32_t i = 0; i < ORMA_G(span_count); i++) {
		orma_span *span = &ORMA_G(spans)[i];
		if (span->open) {
			/* Non e' mai tornata: la si segna in errore invece di riportare
			 * una durata inventata. */
			orma_span_close((int)i, ORMA_STATUS_ERROR);
		}
	}
}

int orma_span_record(const char *name, size_t name_len, uint8_t kind,
                     const uint8_t span_id[ORMA_SPAN_ID_LEN],
                     const uint8_t parent_id[ORMA_SPAN_ID_LEN],
                     uint64_t start_unix_nano, uint64_t duration_nano,
                     uint8_t status, uint32_t chiamate, uint64_t interne_nano)
{
	if (!ORMA_G(txn).active) {
		return -1;
	}
	if (!orma_spans_room()) {
		ORMA_G(txn).spans_dropped++;
		return -1;
	}

	uint32_t idx = ORMA_G(span_count);
	orma_span *span = &ORMA_G(spans)[ORMA_G(span_count)++];

	memset(span, 0, sizeof(*span));
	memcpy(span->span_id, span_id, ORMA_SPAN_ID_LEN);
	memcpy(span->parent_span_id, parent_id, ORMA_SPAN_ID_LEN);
	orma_arena_copy(name, name_len, &span->name_off, &span->name_len);

	span->kind = kind;
	span->status = status;
	span->open = false;
	span->start_unix_nano = start_unix_nano;
	span->duration_nano = duration_nano;
	span->chiamate = chiamate;
	span->interne_nano = interne_nano;
	return (int)idx;
}

/* FNV-1a sui percorsi, per confrontare senza memcmp nella maggior parte dei casi. */
static uint32_t orma_hash_file(const char *s, size_t len)
{
	uint32_t h = 2166136261u;
	for (size_t i = 0; i < len; i++) {
		h ^= (uint8_t)s[i];
		h *= 16777619u;
	}
	return h;
}

bool orma_arena_file(const char *s, size_t len, uint32_t *off, uint32_t *out_len)
{
	if (s == NULL || len == 0) {
		*off = 0;
		*out_len = 0;
		return true;
	}

	uint32_t h = orma_hash_file(s, len);
	for (uint32_t i = 0; i < ORMA_G(file_count); i++) {
		orma_file_internato *voce = &ORMA_G(file_tab)[i];
		if (voce->hash == h && voce->len == len
		    && memcmp(orma_arena_str(voce->off), s, len) == 0) {
			*off = voce->off;
			*out_len = voce->len;
			return true;
		}
	}

	if (!orma_arena_copy(s, len, off, out_len)) {
		return false;
	}
	if (ORMA_G(file_count) < ORMA_MAX_FILE) {
		orma_file_internato *voce = &ORMA_G(file_tab)[ORMA_G(file_count)++];
		voce->off = *off;
		voce->len = *out_len;
		voce->hash = h;
	}
	return true;
}

/* Riempie una posizione da un file e una riga. */
static void orma_posizione_da(orma_posizione *pos, const zend_string *file, uint32_t linea)
{
	if (file == NULL) {
		return;
	}
	orma_arena_file(ZSTR_VAL(file), ZSTR_LEN(file), &pos->file_off, &pos->file_len);
	pos->linea = linea;
}

void orma_span_posizioni(int idx, const zend_function *fbc, zend_execute_data *ex)
{
	orma_span *span = orma_span_at(idx);
	if (span == NULL) {
		return;
	}

	/* Dove la funzione e' scritta. Le funzioni interne di PHP non stanno in
	 * nessun file, e restano senza definizione. */
	if (fbc != NULL && fbc->type == ZEND_USER_FUNCTION) {
		orma_posizione_da(&span->definizione, fbc->op_array.filename,
		                  fbc->op_array.line_start);
	}

	/* Da dove e' partita la chiamata: si risale saltando i frame che non sono
	 * codice utente, perche' quelli non hanno un file da mostrare. */
	span->pila_n = 0;
	for (zend_execute_data *frame = ex != NULL ? ex->prev_execute_data : NULL;
	     frame != NULL && span->pila_n < ORMA_RIF_MAX;
	     frame = frame->prev_execute_data) {

		if (frame->func == NULL || !ZEND_USER_CODE(frame->func->type)
		    || frame->func->op_array.filename == NULL || frame->opline == NULL) {
			continue;
		}
		orma_posizione_da(&span->pila[span->pila_n], frame->func->op_array.filename,
		                  frame->opline->lineno);
		span->pila_n++;
	}
}

void orma_span_attr_str(int idx, const char *key, const char *value, size_t len)
{
	orma_span *span = orma_span_at(idx);
	if (span == NULL || span->attr_count >= ORMA_MAX_SPAN_ATTRS || value == NULL) {
		return;
	}

	orma_attr *attr = &span->attrs[span->attr_count];
	attr->key = key;
	attr->type = ORMA_ATTR_STRING;
	attr->i64 = 0;
	if (!orma_arena_copy(value, len, &attr->str_off, &attr->str_len)) {
		return;
	}
	span->attr_count++;
}

void orma_span_attr_int(int idx, const char *key, int64_t value)
{
	orma_span *span = orma_span_at(idx);
	if (span == NULL || span->attr_count >= ORMA_MAX_SPAN_ATTRS) {
		return;
	}

	orma_attr *attr = &span->attrs[span->attr_count];
	attr->key = key;
	attr->type = ORMA_ATTR_INT64;
	attr->str_off = 0;
	attr->str_len = 0;
	attr->i64 = value;
	span->attr_count++;
}
