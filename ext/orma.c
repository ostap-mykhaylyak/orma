/* orma — APM per PHP.
 *
 * M1: la transazione. RINIT apre, RSHUTDOWN chiude, serializza e consegna al
 * daemon. Gli span figli (query, HTTP uscenti, funzioni utente) arrivano ai
 * milestone successivi.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_ini.h"
#include "ext/standard/info.h"

#include "php_orma.h"
#include "orma_txn.h"
#include "orma_span.h"
#include "orma_api.h"
#include "orma_observer.h"
#include "orma_error.h"
#include "orma_hooks.h"
#include "orma_proto.h"
#include "orma_sender.h"

#include <unistd.h>

ZEND_DECLARE_MODULE_GLOBALS(orma)

PHP_INI_BEGIN()
	STD_PHP_INI_BOOLEAN("orma.enabled", "1", PHP_INI_SYSTEM, OnUpdateBool,
		enabled, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.app_name", "default", PHP_INI_SYSTEM, OnUpdateString,
		app_name, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.socket", "/run/orma/orma.sock", PHP_INI_SYSTEM, OnUpdateString,
		socket_path, zend_orma_globals, orma_globals)
	/* 0 nessuna instrumentazione delle funzioni utente, 1 solo sopra soglia,
	 * 2 tutto. Vedi ext/orma_observer.c. */
	STD_PHP_INI_ENTRY("orma.detail", "1", PHP_INI_SYSTEM, OnUpdateLong,
		detail, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.function_ms", "5", PHP_INI_SYSTEM, OnUpdateLong,
		function_ms, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.max_depth", "5", PHP_INI_SYSTEM, OnUpdateLong,
		max_depth, zend_orma_globals, orma_globals)
PHP_INI_END()

static PHP_GINIT_FUNCTION(orma)
{
#if defined(COMPILE_DL_ORMA) && defined(ZTS)
	ZEND_TSRMLS_CACHE_UPDATE();
#endif
	memset(orma_globals, 0, sizeof(*orma_globals));
	orma_globals->sock_fd = -1;
}

static PHP_GSHUTDOWN_FUNCTION(orma)
{
	if (orma_globals->sock_fd >= 0) {
		close(orma_globals->sock_fd);
		orma_globals->sock_fd = -1;
	}
	orma_buf_free(&orma_globals->buf);
	orma_spans_free();
	orma_observer_free();
}

PHP_MINIT_FUNCTION(orma)
{
	REGISTER_INI_ENTRIES();

	if (gethostname(ORMA_G(hostname), sizeof(ORMA_G(hostname))) != 0) {
		strcpy(ORMA_G(hostname), "sconosciuto");
	}
	ORMA_G(hostname)[sizeof(ORMA_G(hostname)) - 1] = '\0';

	/* La registrazione dell'observer e' di processo e va fatta prima che
	 * qualunque op_array venga compilato: qui e' l'unico posto giusto. */
	orma_observer_register();
	orma_error_install();

	return SUCCESS;
}

PHP_MSHUTDOWN_FUNCTION(orma)
{
	orma_error_uninstall();
	UNREGISTER_INI_ENTRIES();
	return SUCCESS;
}

PHP_RINIT_FUNCTION(orma)
{
#if defined(ZTS) && defined(COMPILE_DL_ORMA)
	ZEND_TSRMLS_CACHE_UPDATE();
#endif

	if (!ORMA_G(enabled)) {
		ORMA_G(txn).active = false;
		return SUCCESS;
	}

	/* Alla prima richiesta, non in MINIT: l'ordine di caricamento delle
	 * estensioni non e' garantito, e PDO o curl potrebbero non essere ancora
	 * registrate quando tocca a noi. */
	orma_hooks_install();

	orma_spans_reset();
	orma_observer_reset();
	orma_txn_begin();
	return SUCCESS;
}

PHP_RSHUTDOWN_FUNCTION(orma)
{
	orma_txn *txn = &ORMA_G(txn);

	if (!ORMA_G(enabled) || !txn->active) {
		return SUCCESS;
	}

	/* Uno span ancora aperto significa che la funzione avvolta non e' tornata:
	 * si chiude in errore prima di misurare la transazione. */
	orma_spans_close_open();
	orma_txn_end();

	if (txn->ignored) {
		txn->active = false;
		return SUCCESS;
	}

	if (orma_proto_encode(txn, &ORMA_G(buf))) {
		orma_sender_send(ORMA_G(buf).data, ORMA_G(buf).len);
	}

	txn->active = false;
	return SUCCESS;
}

PHP_MINFO_FUNCTION(orma)
{
	char sent[32], dropped[32];

	snprintf(sent, sizeof(sent), "%" PRIu64, ORMA_G(sent_frames));
	snprintf(dropped, sizeof(dropped), "%" PRIu64, ORMA_G(dropped_frames));

	php_info_print_table_start();
	php_info_print_table_header(2, "orma", "abilitata");
	php_info_print_table_row(2, "Versione", PHP_ORMA_VERSION);
	php_info_print_table_row(2, "Stato", ORMA_G(enabled) ? "attivo" : "disattivato");
	php_info_print_table_row(2, "Hostname", ORMA_G(hostname));
	php_info_print_table_row(2, "Frame consegnati", sent);
	php_info_print_table_row(2, "Frame persi", dropped);
	php_info_print_table_end();

	DISPLAY_INI_ENTRIES();
}

zend_module_entry orma_module_entry = {
	STANDARD_MODULE_HEADER,
	"orma",
	orma_functions,
	PHP_MINIT(orma),
	PHP_MSHUTDOWN(orma),
	PHP_RINIT(orma),
	PHP_RSHUTDOWN(orma),
	PHP_MINFO(orma),
	PHP_ORMA_VERSION,
	PHP_MODULE_GLOBALS(orma),
	PHP_GINIT(orma),
	PHP_GSHUTDOWN(orma),
	NULL,                      /* post deactivate */
	STANDARD_MODULE_PROPERTIES_EX
};

#ifdef COMPILE_DL_ORMA
# ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
# endif
ZEND_GET_MODULE(orma)
#endif
