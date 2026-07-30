/* orma — APM per PHP.
 *
 * M0: scheletro caricabile. Registra le direttive INI e i confini di richiesta,
 * ma non raccoglie ancora nulla. La transazione arriva al M1.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_ini.h"
#include "ext/standard/info.h"

#include "php_orma.h"

ZEND_DECLARE_MODULE_GLOBALS(orma)

PHP_INI_BEGIN()
	STD_PHP_INI_BOOLEAN("orma.enabled", "1", PHP_INI_SYSTEM, OnUpdateBool,
		enabled, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.app_name", "default", PHP_INI_SYSTEM, OnUpdateString,
		app_name, zend_orma_globals, orma_globals)
	STD_PHP_INI_ENTRY("orma.socket", "/run/orma/orma.sock", PHP_INI_SYSTEM, OnUpdateString,
		socket_path, zend_orma_globals, orma_globals)
PHP_INI_END()

static PHP_GINIT_FUNCTION(orma)
{
#if defined(COMPILE_DL_ORMA) && defined(ZTS)
	ZEND_TSRMLS_CACHE_UPDATE();
#endif
	memset(orma_globals, 0, sizeof(*orma_globals));
}

PHP_MINIT_FUNCTION(orma)
{
	REGISTER_INI_ENTRIES();
	return SUCCESS;
}

PHP_MSHUTDOWN_FUNCTION(orma)
{
	UNREGISTER_INI_ENTRIES();
	return SUCCESS;
}

PHP_RINIT_FUNCTION(orma)
{
#if defined(ZTS) && defined(COMPILE_DL_ORMA)
	ZEND_TSRMLS_CACHE_UPDATE();
#endif
	/* M1: apertura della transazione. */
	return SUCCESS;
}

PHP_RSHUTDOWN_FUNCTION(orma)
{
	/* M1: chiusura della transazione e flush non bloccante. */
	return SUCCESS;
}

PHP_MINFO_FUNCTION(orma)
{
	php_info_print_table_start();
	php_info_print_table_header(2, "orma", "abilitata");
	php_info_print_table_row(2, "Versione", PHP_ORMA_VERSION);
	php_info_print_table_row(2, "Stato",
		ORMA_G(enabled) ? "attivo" : "disattivato");
	php_info_print_table_end();

	DISPLAY_INI_ENTRIES();
}

zend_module_entry orma_module_entry = {
	STANDARD_MODULE_HEADER,
	"orma",
	NULL,                      /* funzioni userland: arrivano al M6 */
	PHP_MINIT(orma),
	PHP_MSHUTDOWN(orma),
	PHP_RINIT(orma),
	PHP_RSHUTDOWN(orma),
	PHP_MINFO(orma),
	PHP_ORMA_VERSION,
	PHP_MODULE_GLOBALS(orma),
	PHP_GINIT(orma),
	NULL,                      /* globals dtor */
	NULL,                      /* post deactivate */
	STANDARD_MODULE_PROPERTIES_EX
};

#ifdef COMPILE_DL_ORMA
# ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
# endif
ZEND_GET_MODULE(orma)
#endif
