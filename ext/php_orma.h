/* orma — APM per PHP.
 * Dichiarazioni dell'estensione. Vedi DESIGN.md §2.
 */

#ifndef PHP_ORMA_H
#define PHP_ORMA_H

extern zend_module_entry orma_module_entry;
#define phpext_orma_ptr &orma_module_entry

#define PHP_ORMA_VERSION "0.1.0"

#if defined(ZTS) && defined(COMPILE_DL_ORMA)
ZEND_TSRMLS_CACHE_EXTERN()
#endif

ZEND_BEGIN_MODULE_GLOBALS(orma)
	bool   enabled;
	char  *app_name;
	char  *socket_path;
ZEND_END_MODULE_GLOBALS(orma)

#define ORMA_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(orma, v)

#endif /* PHP_ORMA_H */
