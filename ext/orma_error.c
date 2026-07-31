/* Cattura degli errori PHP e delle eccezioni.
 *
 * Due agganci distinti perche' raccontano cose diverse:
 *
 *   zend_error_cb            errori del motore, dal deprecation al fatale.
 *                            Le eccezioni non catturate arrivano qui come
 *                            E_ERROR, quindi da sole basterebbero a marcare
 *                            la transazione fallita.
 *   zend_throw_exception_hook eccezioni al momento del lancio. Una eccezione
 *                            lanciata puo' essere catturata dieci righe dopo:
 *                            si registra, ma non marca nulla come fallito.
 *
 * Da cui la regola: solo gli errori di classe fatale incrementano txn.errors,
 * che e' il campo su cui il daemon decide se la transazione e' andata male.
 * Un sito pieno di deprecation non e' un sito rotto.
 *
 * Gli handler precedenti vengono sempre richiamati: orma osserva, non
 * sostituisce il comportamento di PHP.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "zend_exceptions.h"

#include "php_orma.h"
#include "orma_error.h"
#include "orma_span.h"
#include "orma_txn.h"

#include <string.h>
#include <strings.h>

static void (*orma_prev_error_cb)(int type, zend_string *file, uint32_t line, zend_string *message);
static void (*orma_prev_throw_hook)(zend_object *exception);

/* Il motore puo' passare il tipo con dei bit di servizio sopra a quelli
 * standard (per esempio E_DONT_BAIL su un'eccezione non catturata). Vanno
 * mascherati prima di confrontare, altrimenti un E_ERROR non si riconosce. */
static inline int orma_error_type(int type)
{
	return type & E_ALL;
}

static const char *orma_error_class(int type)
{
	switch (orma_error_type(type)) {
	case E_ERROR:             return "E_ERROR";
	case E_WARNING:           return "E_WARNING";
	case E_PARSE:             return "E_PARSE";
	case E_NOTICE:            return "E_NOTICE";
	case E_CORE_ERROR:        return "E_CORE_ERROR";
	case E_CORE_WARNING:      return "E_CORE_WARNING";
	case E_COMPILE_ERROR:     return "E_COMPILE_ERROR";
	case E_COMPILE_WARNING:   return "E_COMPILE_WARNING";
	case E_USER_ERROR:        return "E_USER_ERROR";
	case E_USER_WARNING:      return "E_USER_WARNING";
	case E_USER_NOTICE:       return "E_USER_NOTICE";
	case E_RECOVERABLE_ERROR: return "E_RECOVERABLE_ERROR";
	case E_DEPRECATED:        return "E_DEPRECATED";
	case E_USER_DEPRECATED:   return "E_USER_DEPRECATED";
	default:                  return "E_UNKNOWN";
	}
}

static bool orma_is_fatal(int type)
{
	return (orma_error_type(type) & (E_ERROR | E_PARSE | E_CORE_ERROR | E_COMPILE_ERROR
	                                 | E_USER_ERROR | E_RECOVERABLE_ERROR)) != 0;
}

/* Registra un evento. I conteggi crescono sempre; il dettaglio si conserva
 * solo per i primi ORMA_MAX_ERRORS. */
void orma_txn_record_event(const char *class, size_t class_len,
                           const char *message, size_t message_len,
                           const char *file, size_t file_len,
                           uint32_t line, uint8_t severita)
{
	orma_txn *txn = &ORMA_G(txn);

	if (!txn->active) {
		return;
	}

	if (severita == ORMA_SEVERITA_ERRORE) {
		txn->errors++;
	} else {
		txn->warnings++;
	}

	if (txn->event_count >= ORMA_MAX_ERRORS) {
		return;
	}

	if (message_len > ORMA_MAX_ERROR_MESSAGE) {
		message_len = ORMA_MAX_ERROR_MESSAGE;
	}

	orma_error *ev = &txn->events[txn->event_count];
	memset(ev, 0, sizeof(*ev));

	orma_arena_copy(class, class_len, &ev->class_off, &ev->class_len);
	orma_arena_copy(message, message_len, &ev->msg_off, &ev->msg_len);
	orma_arena_copy(file, file_len, &ev->file_off, &ev->file_len);

	ev->line = line;
	ev->severita = severita;
	ev->unix_nano = orma_now_unix_nano();

	txn->event_count++;
}

static void orma_error_cb(int type, zend_string *file, uint32_t line, zend_string *message)
{
	const char *class = orma_error_class(type);

	orma_txn_record_event(class, strlen(class),
	                  message != NULL ? ZSTR_VAL(message) : "",
	                  message != NULL ? ZSTR_LEN(message) : 0,
	                  file != NULL ? ZSTR_VAL(file) : "",
	                  file != NULL ? ZSTR_LEN(file) : 0,
	                  line,
	                  orma_is_fatal(type) ? ORMA_SEVERITA_ERRORE : ORMA_SEVERITA_AVVISO);

	if (orma_prev_error_cb != NULL) {
		orma_prev_error_cb(type, file, line, message);
	}
}

/* Vero se la classe compare in orma.ignored_exceptions.
 *
 * Serve alle applicazioni che usano le eccezioni per controllo di flusso: senza
 * un filtro, la pagina Errori si riempie di eccezioni del tutto normali e
 * smette di essere utile. */
static bool orma_exception_ignored(const zend_string *nome)
{
	const char *lista = ORMA_G(ignored_exceptions);
	if (lista == NULL || *lista == '\0' || nome == NULL) {
		return false;
	}

	size_t nome_len = ZSTR_LEN(nome);
	const char *p = lista;

	while (*p != '\0') {
		while (*p == ',' || *p == ' ') {
			p++;
		}
		const char *inizio = p;
		while (*p != '\0' && *p != ',') {
			p++;
		}
		size_t len = (size_t)(p - inizio);
		while (len > 0 && inizio[len - 1] == ' ') {
			len--;
		}
		if (len == nome_len && strncasecmp(inizio, ZSTR_VAL(nome), len) == 0) {
			return true;
		}
	}
	return false;
}

static void orma_throw_hook(zend_object *exception)
{
	if (exception != NULL && ORMA_G(txn).active
	    && !orma_exception_ignored(exception->ce != NULL ? exception->ce->name : NULL)) {
		zend_class_entry *ce = exception->ce;

		zval rv;
		zval *message = zend_read_property(ce, exception, "message", sizeof("message") - 1, 1, &rv);
		zval *file = zend_read_property(ce, exception, "file", sizeof("file") - 1, 1, &rv);
		zval *line = zend_read_property(ce, exception, "line", sizeof("line") - 1, 1, &rv);

		/* Severita' avviso: l'eccezione potrebbe essere catturata subito dopo.
		 * Se non lo sara', arrivera' comunque come E_ERROR da zend_error_cb. */
		orma_txn_record_event(
			ce != NULL ? ZSTR_VAL(ce->name) : "Exception",
			ce != NULL ? ZSTR_LEN(ce->name) : 9,
			(message != NULL && Z_TYPE_P(message) == IS_STRING) ? Z_STRVAL_P(message) : "",
			(message != NULL && Z_TYPE_P(message) == IS_STRING) ? Z_STRLEN_P(message) : 0,
			(file != NULL && Z_TYPE_P(file) == IS_STRING) ? Z_STRVAL_P(file) : "",
			(file != NULL && Z_TYPE_P(file) == IS_STRING) ? Z_STRLEN_P(file) : 0,
			(line != NULL && Z_TYPE_P(line) == IS_LONG) ? (uint32_t)Z_LVAL_P(line) : 0,
			ORMA_SEVERITA_AVVISO);
	}

	if (orma_prev_throw_hook != NULL) {
		orma_prev_throw_hook(exception);
	}
}

void orma_error_install(void)
{
	orma_prev_error_cb = zend_error_cb;
	zend_error_cb = orma_error_cb;

	orma_prev_throw_hook = zend_throw_exception_hook;
	zend_throw_exception_hook = orma_throw_hook;
}

void orma_error_uninstall(void)
{
	if (zend_error_cb == orma_error_cb) {
		zend_error_cb = orma_prev_error_cb;
	}
	if (zend_throw_exception_hook == orma_throw_hook) {
		zend_throw_exception_hook = orma_prev_throw_hook;
	}
}
