/* Instrumentazione delle funzioni utente.
 *
 * zend_observer registra gli handler una volta per op_array, non a ogni
 * chiamata: e' per questo che si puo' osservare tutto il codice utente senza
 * sostituire zend_execute_ex.
 *
 * Il costo resta pero' reale: due letture di orologio per chiamata di
 * funzione. Da cui le politiche di orma.detail:
 *
 *   0  nessun observer registrato, costo zero;
 *   1  (default) si cronometra solo entro orma.max_depth e si emette uno span
 *      solo sopra orma.function_ms;
 *   2  si emette tutto, fino al tetto degli span.
 *
 * Perche' la soglia non produce span orfani: la durata di un genitore e'
 * sempre maggiore o uguale a quella di ogni suo discendente, quindi se un
 * genitore sta sotto soglia ci stanno anche tutti i figli, e nessuno di loro
 * e' stato emesso.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "zend_observer.h"

#include "php_orma.h"
#include "orma_observer.h"
#include "orma_span.h"
#include "orma_txn.h"

#include <string.h>

typedef struct _orma_frame {
	zend_execute_data *frame;
	uint8_t  span_id[ORMA_SPAN_ID_LEN];
	uint8_t  parent_id[ORMA_SPAN_ID_LEN];
	/* Identificativo che i figli devono usare come genitore: il proprio se
	 * questo frame e' tracciato, altrimenti quello ereditato. */
	uint8_t  inherited[ORMA_SPAN_ID_LEN];
	uint64_t start_monotonic_nano;
	/* Valore del contatore globale all'ingresso: la differenza a fine frame
	 * da' quante chiamate sono avvenute dentro, comprese quelle rimaste sotto
	 * soglia e le annidate, in tempo costante. */
	uint64_t chiamate_inizio;
	bool     tracked;
} orma_frame;

void orma_observer_reset(void)
{
	ORMA_G(depth) = 0;
	ORMA_G(skipped) = 0;
}

void orma_observer_free(void)
{
	if (ORMA_G(stack) != NULL) {
		pefree(ORMA_G(stack), 1);
		ORMA_G(stack) = NULL;
	}
	ORMA_G(stack_cap) = 0;
	ORMA_G(depth) = 0;
	ORMA_G(skipped) = 0;
}

void orma_observer_current_parent(uint8_t out[ORMA_SPAN_ID_LEN])
{
	if (ORMA_G(depth) > 0 && ORMA_G(stack) != NULL) {
		memcpy(out, ORMA_G(stack)[ORMA_G(depth) - 1].inherited, ORMA_SPAN_ID_LEN);
		return;
	}
	memcpy(out, ORMA_G(txn).span_id, ORMA_SPAN_ID_LEN);
}

static bool orma_stack_room(void)
{
	if (ORMA_G(depth) < ORMA_G(stack_cap)) {
		return true;
	}
	if (ORMA_G(stack_cap) >= ORMA_MAX_STACK) {
		return false;
	}

	uint32_t want = ORMA_G(stack_cap) ? ORMA_G(stack_cap) * 2 : 64;
	if (want > ORMA_MAX_STACK) {
		want = ORMA_MAX_STACK;
	}

	orma_frame *grown = perealloc(ORMA_G(stack), sizeof(orma_frame) * want, 1);
	if (grown == NULL) {
		return false;
	}
	ORMA_G(stack) = grown;
	ORMA_G(stack_cap) = want;
	return true;
}

/* Compone "Classe::metodo" oppure "funzione". Restituisce la lunghezza. */
static size_t orma_function_name(const zend_function *fbc, char *out, size_t cap)
{
	if (fbc == NULL || fbc->common.function_name == NULL) {
		return 0;
	}

	const zend_string *fn = fbc->common.function_name;
	const zend_string *cls = (fbc->common.scope != NULL) ? fbc->common.scope->name : NULL;

	size_t w = 0;
	if (cls != NULL) {
		size_t n = ZSTR_LEN(cls);
		if (n > cap - 3) {
			n = cap - 3;
		}
		memcpy(out, ZSTR_VAL(cls), n);
		w = n;
		out[w++] = ':';
		out[w++] = ':';
	}

	size_t n = ZSTR_LEN(fn);
	if (n > cap - w - 1) {
		n = cap - w - 1;
	}
	memcpy(out + w, ZSTR_VAL(fn), n);
	w += n;
	out[w] = '\0';
	return w;
}

static void orma_observer_begin(zend_execute_data *execute_data)
{
	if (!ORMA_G(txn).active) {
		return;
	}

	/* Si contano tutte, anche quelle che non si cronometrano: e' il numero che
	 * distingue "lento perche' fa un milione di cose" da "lento perche'
	 * aspetta", e senza di esso un metodo da cinque secondi senza figli sopra
	 * soglia resta inspiegabile. */
	ORMA_G(txn).chiamate++;

	/* Oltre la profondita' massima non si cronometra: invece di scrivere un
	 * frame che non servira' a nulla, si contano le chiamate saltate. Su una
	 * ricorsione profonda e' la differenza fra due memcpy per chiamata e un
	 * incremento. begin ed end restano in pari perche' i frame saltati sono
	 * sempre piu' profondi di quelli impilati. */
	if ((ORMA_G(detail) != ORMA_DETAIL_TUTTO
	     && ORMA_G(depth) >= (uint32_t)ORMA_G(max_depth))
	    || !orma_stack_room()) {
		ORMA_G(skipped)++;
		return;
	}

	orma_frame *f = &ORMA_G(stack)[ORMA_G(depth)];
	const uint8_t *inherited = (ORMA_G(depth) > 0)
	                         ? ORMA_G(stack)[ORMA_G(depth) - 1].inherited
	                         : ORMA_G(txn).span_id;

	f->frame = execute_data;
	f->tracked = true;

	memcpy(f->parent_id, inherited, ORMA_SPAN_ID_LEN);
	orma_rng_fill(f->span_id, ORMA_SPAN_ID_LEN);
	memcpy(f->inherited, f->span_id, ORMA_SPAN_ID_LEN);

	/* Un solo orologio per chiamata. L'istante assoluto di inizio si ricava
	 * alla fine da quello della transazione piu' lo scarto monotonico: e'
	 * esattamente lo stesso valore, e costa una syscall in meno su ogni
	 * chiamata di funzione osservata. */
	f->start_monotonic_nano = orma_now_monotonic_nano();
	f->chiamate_inizio = ORMA_G(txn).chiamate;

	ORMA_G(depth)++;
}

static void orma_observer_end(zend_execute_data *execute_data, zval *return_value)
{
	(void)return_value;

	if (ORMA_G(skipped) > 0) {
		ORMA_G(skipped)--;
		return;
	}
	if (ORMA_G(depth) == 0 || ORMA_G(stack) == NULL) {
		return;
	}

	/* In caso di disallineamento, per esempio se un frame e' uscito senza
	 * passare da qui, si srotola fino a ritrovare il proprio. */
	uint32_t idx = ORMA_G(depth);
	while (idx > 0 && ORMA_G(stack)[idx - 1].frame != execute_data) {
		idx--;
	}
	if (idx == 0) {
		return;
	}
	ORMA_G(depth) = idx - 1;

	orma_frame *f = &ORMA_G(stack)[idx - 1];
	if (!f->tracked || !ORMA_G(txn).active) {
		return;
	}

	uint64_t now = orma_now_monotonic_nano();
	uint64_t duration = (now > f->start_monotonic_nano) ? now - f->start_monotonic_nano : 0;

	if (ORMA_G(detail) != ORMA_DETAIL_TUTTO) {
		uint64_t soglia = (uint64_t)ORMA_G(function_ms) * 1000000ULL;
		if (duration < soglia) {
			return;
		}
	}

	char name[256];
	size_t name_len = orma_function_name(execute_data->func, name, sizeof(name));
	if (name_len == 0) {
		return;
	}

	const orma_txn *txn = &ORMA_G(txn);
	uint64_t start_unix = txn->start_unix_nano
	                    + (f->start_monotonic_nano - txn->start_monotonic_nano);

	uint64_t chiamate = ORMA_G(txn).chiamate - f->chiamate_inizio;
	if (chiamate > UINT32_MAX) {
		chiamate = UINT32_MAX;
	}

	orma_span_record(name, name_len, ORMA_SPAN_INTERNAL,
	                 f->span_id, f->parent_id,
	                 start_unix, duration,
	                 EG(exception) != NULL ? ORMA_STATUS_ERROR : ORMA_STATUS_OK,
	                 (uint32_t)chiamate);
}

static zend_observer_fcall_handlers orma_observer_init(zend_execute_data *execute_data)
{
	zend_observer_fcall_handlers handlers = { NULL, NULL };

	/* Con la raccolta disattivata non si registra nulla: altrimenti gli
	 * handler verrebbero comunque chiamati a ogni funzione solo per uscire
	 * subito, e orma.enabled=0 costerebbe piu' di orma.detail=0. */
	if (!ORMA_G(enabled) || ORMA_G(detail) == ORMA_DETAIL_OFF) {
		return handlers;
	}

	const zend_function *fbc = execute_data->func;
	if (fbc == NULL || fbc->type != ZEND_USER_FUNCTION) {
		return handlers;
	}
	/* Il corpo del file non e' una funzione: osservarlo duplicherebbe la
	 * transazione. */
	if (fbc->common.function_name == NULL) {
		return handlers;
	}

	handlers.begin = orma_observer_begin;
	handlers.end = orma_observer_end;
	return handlers;
}

void orma_observer_register(void)
{
	zend_observer_fcall_register(orma_observer_init);
}
