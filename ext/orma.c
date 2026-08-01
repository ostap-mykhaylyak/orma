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
	/* Millisecondi che la consegna puo' sottrarre alla richiesta. Scaduti, il
	 * frame si perde: perdere telemetria e' preferibile a rallentare l'utente.
	 * Su una macchina carica cinque possono essere pochi. */
	STD_PHP_INI_ENTRY("orma.send_timeout_ms", "5", PHP_INI_SYSTEM, OnUpdateLong,
		send_timeout_ms, zend_orma_globals, orma_globals)
	/* Profilo delle funzioni interne costose: risponde al perche' di una
	 * lentezza che il waterfall da solo non spiega. */
	STD_PHP_INI_BOOLEAN("orma.profile_internals", "1", PHP_INI_SYSTEM, OnUpdateBool,
		profile_internals, zend_orma_globals, orma_globals)
	/* Classi di eccezione da non registrare, separate da virgola. Per le
	 * applicazioni che usano le eccezioni come controllo di flusso. */
	STD_PHP_INI_ENTRY("orma.ignored_exceptions", "", PHP_INI_SYSTEM, OnUpdateString,
		ignored_exceptions, zend_orma_globals, orma_globals)
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
	orma_hooks_free();
}

PHP_MINIT_FUNCTION(orma)
{
	REGISTER_INI_ENTRIES();

	if (gethostname(ORMA_G(hostname), sizeof(ORMA_G(hostname))) != 0) {
		strcpy(ORMA_G(hostname), "sconosciuto");
	}
	ORMA_G(hostname)[sizeof(ORMA_G(hostname)) - 1] = '\0';

	/* La registrazione dell'observer e' di processo e va fatta prima che
	 * qualunque op_array venga compilato: qui e' l'unico posto giusto.
	 *
	 * Va anche evitata del tutto quando non serve: registrare un observer
	 * cambia il percorso di chiamata del motore per ogni funzione, anche se
	 * poi decidiamo di non osservarla. Misurato, sono circa otto punti
	 * percentuali su un carico fatto di sole chiamate. */
	if (ORMA_G(enabled) && ORMA_G(detail) != ORMA_DETAIL_OFF) {
		orma_observer_register();
	}

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
	orma_hooks_reset();
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
	/* Qui si misura soltanto. La consegna avviene in post_deactivate, quando
	 * la richiesta e' gia' stata chiusa: vedi orma_post_deactivate. */
	orma_spans_close_open();
	orma_txn_end();

	return SUCCESS;
}

/* Eseguita dopo lo spegnimento della richiesta, quando l'output e' gia' stato
 * consegnato e i moduli hanno gia' fatto il loro RSHUTDOWN.
 *
 * Onesta' su cosa questo risolve e cosa no: la risposta e' gia' partita verso
 * il client, quindi il tempo speso qui non allunga la latenza percepita. Non e'
 * pero' fuori dal ciclo del worker php-fpm, che resta occupato finche' non
 * abbiamo finito. E' per questo che il budget di ORMA_SEND_TIMEOUT_MS resta
 * indispensabile: sposta il costo, non lo elimina.
 */
static int orma_post_deactivate(void)
{
	orma_txn *txn = &ORMA_G(txn);

	if (!txn->active) {
		return SUCCESS;
	}
	txn->active = false;

	if (txn->ignored) {
		return SUCCESS;
	}

	if (orma_proto_encode(txn, &ORMA_G(buf))) {
		orma_sender_send(ORMA_G(buf).data, ORMA_G(buf).len);
	}
	return SUCCESS;
}

PHP_MINFO_FUNCTION(orma)
{
	char sent[32], dropped[32];

	snprintf(sent, sizeof(sent), "%" PRIu64, ORMA_G(sent_frames));
	snprintf(dropped, sizeof(dropped), "%" PRIu64, ORMA_G(dropped_total));

	php_info_print_table_start();
	php_info_print_table_header(2, "orma", "abilitata");
	php_info_print_table_row(2, "Versione", PHP_ORMA_VERSION);
	php_info_print_table_row(2, "Stato", ORMA_G(enabled) ? "attivo" : "disattivato");
	php_info_print_table_row(2, "Hostname", ORMA_G(hostname));
	php_info_print_table_row(2, "Frame consegnati", sent);
	php_info_print_table_row(2, "Frame persi", dropped);

	char cause[96];
	snprintf(cause, sizeof(cause), "connessione %u, timeout %u, scrittura %u",
		ORMA_G(dropped)[ORMA_DROP_CONNESSIONE],
		ORMA_G(dropped)[ORMA_DROP_TIMEOUT],
		ORMA_G(dropped)[ORMA_DROP_SCRITTURA]);
	php_info_print_table_row(2, "Persi dall'ultima consegna", cause);
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
	orma_post_deactivate,
	STANDARD_MODULE_PROPERTIES_EX
};

#ifdef COMPILE_DL_ORMA
# ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
# endif
ZEND_GET_MODULE(orma)
#endif
