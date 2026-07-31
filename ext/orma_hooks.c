/* Instrumentazione delle funzioni interne.
 *
 * Il valore vero di un APM sta qui: quasi sempre il tempo se ne va in query
 * lente e chiamate HTTP uscenti, non nel PHP. Sostituire l'handler di una
 * funzione interna non richiede l'observer e non dipende dal framework: si
 * intercetta il motore, non l'applicazione.
 *
 * Vincoli che questo file rispetta:
 *   - l'handler originale viene chiamato sempre, anche se la nostra logica
 *     fallisce a monte;
 *   - non si introducono mai eccezioni che non c'erano (da cui i controlli di
 *     tipo prima di chiamare curl_getinfo);
 *   - se la funzione avvolta non torna, lo span resta aperto e viene chiuso in
 *     errore alla fine della transazione, invece di riportare una durata finta.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "zend_exceptions.h"

#include "php_orma.h"
#include "orma_hooks.h"
#include "orma_span.h"
#include "orma_sql.h"

#include <string.h>
#include <strings.h>

/* Gli handler originali sono di processo, non di richiesta: la tabella delle
 * funzioni si patcha una volta sola. */
static zif_handler orig_curl_exec;
static zif_handler orig_file_get_contents;
static zif_handler orig_pdo_query;
static zif_handler orig_pdo_exec;
static zif_handler orig_pdostatement_execute;
static zif_handler orig_mysqli_query;
static zif_handler orig_mysqli_real_query;
static zif_handler orig_mysqli_method_query;

/* ---------------------------------------------------------------- utilita' */

static bool orma_failed(const zval *return_value)
{
	return return_value != NULL && Z_TYPE_P(return_value) == IS_FALSE;
}

/* Estrae l'host da un URL. Restituisce la lunghezza, 0 se non lo trova. */
static size_t orma_url_host(const char *url, size_t len, const char **host)
{
	const char *sep = NULL;
	for (size_t i = 0; i + 2 < len; i++) {
		if (url[i] == ':' && url[i + 1] == '/' && url[i + 2] == '/') {
			sep = url + i + 3;
			break;
		}
	}
	if (sep == NULL) {
		return 0;
	}

	const char *end = sep;
	const char *limit = url + len;
	while (end < limit && *end != '/' && *end != '?' && *end != '#') {
		end++;
	}

	/* Le credenziali eventualmente presenti prima della chiocciola non devono
	 * finire nella telemetria. */
	for (const char *p = sep; p < end; p++) {
		if (*p == '@') {
			sep = p + 1;
			break;
		}
	}

	/* La porta non fa parte dell'host. */
	const char *colon = memchr(sep, ':', (size_t)(end - sep));
	if (colon != NULL) {
		end = colon;
	}

	*host = sep;
	return (size_t)(end - sep);
}

static bool orma_is_http_url(const char *s, size_t len)
{
	return (len > 7 && strncasecmp(s, "http://", 7) == 0)
	    || (len > 8 && strncasecmp(s, "https://", 8) == 0);
}

/* ------------------------------------------------------------------ database */

static void orma_db_call(zif_handler original, zend_execute_data *execute_data,
                         zval *return_value, const char *system, const zval *sql)
{
	if (!ORMA_G(txn).active || sql == NULL || Z_TYPE_P(sql) != IS_STRING) {
		original(execute_data, return_value);
		return;
	}

	char obfuscated[ORMA_MAX_STATEMENT + 1];
	size_t obf_len = orma_sql_obfuscate(Z_STRVAL_P(sql), Z_STRLEN_P(sql),
	                                    obfuscated, sizeof(obfuscated));
	const char *op = orma_sql_operation(obfuscated, obf_len);

	int span = orma_span_open(op, strlen(op), ORMA_SPAN_CLIENT);
	orma_span_attr_str(span, "db.system", system, strlen(system));
	orma_span_attr_str(span, "db.statement", obfuscated, obf_len);

	original(execute_data, return_value);

	orma_span_close(span, orma_failed(return_value) ? ORMA_STATUS_ERROR : ORMA_STATUS_OK);
}

static const zval *orma_arg(zend_execute_data *execute_data, uint32_t n)
{
	if (ZEND_CALL_NUM_ARGS(execute_data) < n) {
		return NULL;
	}
	return ZEND_CALL_ARG(execute_data, n);
}

static void orma_pdo_query_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_db_call(orig_pdo_query, execute_data, return_value, "pdo",
	             orma_arg(execute_data, 1));
}

static void orma_pdo_exec_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_db_call(orig_pdo_exec, execute_data, return_value, "pdo",
	             orma_arg(execute_data, 1));
}

static void orma_pdostatement_execute_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	zval rv;
	const zval *sql = NULL;
	zval *self = getThis();

	if (self != NULL && Z_TYPE_P(self) == IS_OBJECT) {
		sql = zend_read_property(Z_OBJCE_P(self), Z_OBJ_P(self),
		                         "queryString", sizeof("queryString") - 1, 1, &rv);
	}

	orma_db_call(orig_pdostatement_execute, execute_data, return_value, "pdo", sql);
}

static void orma_mysqli_query_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_db_call(orig_mysqli_query, execute_data, return_value, "mysqli",
	             orma_arg(execute_data, 2));
}

static void orma_mysqli_real_query_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_db_call(orig_mysqli_real_query, execute_data, return_value, "mysqli",
	             orma_arg(execute_data, 2));
}

static void orma_mysqli_method_query_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_db_call(orig_mysqli_method_query, execute_data, return_value, "mysqli",
	             orma_arg(execute_data, 1));
}

/* ------------------------------------------------------------------ esterne */

/* Legge l'URL da un handle curl passando per curl_getinfo.
 *
 * Si chiama solo su un vero CurlHandle: su un argomento di altro tipo
 * curl_getinfo solleverebbe una TypeError, e introdurre un'eccezione che non
 * c'era e' esattamente cio' che questa estensione non deve mai fare. */
static zend_string *orma_curl_url(const zval *handle)
{
	if (handle == NULL || Z_TYPE_P(handle) != IS_OBJECT) {
		return NULL;
	}
	zend_class_entry *ce = Z_OBJCE_P(handle);
	if (ce == NULL || !zend_string_equals_literal_ci(ce->name, "CurlHandle")) {
		return NULL;
	}
	if (EG(exception) != NULL) {
		return NULL;
	}

	zval fname, arg, retval;
	ZVAL_STRING(&fname, "curl_getinfo");
	ZVAL_COPY_VALUE(&arg, handle);
	ZVAL_UNDEF(&retval);

	zend_string *url = NULL;
	if (call_user_function(NULL, NULL, &fname, &retval, 1, &arg) == SUCCESS
	    && Z_TYPE(retval) == IS_ARRAY) {
		zval *found = zend_hash_str_find(Z_ARRVAL(retval), "url", sizeof("url") - 1);
		if (found != NULL && Z_TYPE_P(found) == IS_STRING) {
			url = zend_string_copy(Z_STR_P(found));
		}
	}

	zval_ptr_dtor(&fname);
	zval_ptr_dtor(&retval);
	return url;
}

static void orma_external_span(const char *url, size_t url_len, int *span)
{
	const char *host = NULL;
	size_t host_len = orma_url_host(url, url_len, &host);

	if (host_len == 0) {
		*span = orma_span_open("esterna", 7, ORMA_SPAN_CLIENT);
		return;
	}

	*span = orma_span_open(host, host_len, ORMA_SPAN_CLIENT);
	orma_span_attr_str(*span, "server.address", host, host_len);
	orma_span_attr_str(*span, "url.scheme",
	                   (url_len > 5 && strncasecmp(url, "https", 5) == 0) ? "https" : "http",
	                   (url_len > 5 && strncasecmp(url, "https", 5) == 0) ? 5 : 4);
}

static void orma_curl_exec_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	if (!ORMA_G(txn).active) {
		orig_curl_exec(execute_data, return_value);
		return;
	}

	zend_string *url = orma_curl_url(orma_arg(execute_data, 1));
	int span;

	if (url != NULL) {
		orma_external_span(ZSTR_VAL(url), ZSTR_LEN(url), &span);
		zend_string_release(url);
	} else {
		span = orma_span_open("curl", 4, ORMA_SPAN_CLIENT);
	}

	orig_curl_exec(execute_data, return_value);

	orma_span_close(span, orma_failed(return_value) ? ORMA_STATUS_ERROR : ORMA_STATUS_OK);
}

static void orma_file_get_contents_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	const zval *path = orma_arg(execute_data, 1);

	/* Solo gli accessi di rete interessano: la lettura di un file locale non
	 * e' una chiamata esterna. */
	if (!ORMA_G(txn).active || path == NULL || Z_TYPE_P(path) != IS_STRING
	    || !orma_is_http_url(Z_STRVAL_P(path), Z_STRLEN_P(path))) {
		orig_file_get_contents(execute_data, return_value);
		return;
	}

	int span;
	orma_external_span(Z_STRVAL_P(path), Z_STRLEN_P(path), &span);

	orig_file_get_contents(execute_data, return_value);

	orma_span_close(span, orma_failed(return_value) ? ORMA_STATUS_ERROR : ORMA_STATUS_OK);
}

/* ------------------------------------------------------------ installazione */

static void orma_hook_function(const char *name, size_t len,
                               zif_handler replacement, zif_handler *original)
{
	if (*original != NULL) {
		return;
	}
	zend_function *fn = zend_hash_str_find_ptr(CG(function_table), name, len);
	if (fn == NULL || fn->type != ZEND_INTERNAL_FUNCTION) {
		return;
	}
	*original = fn->internal_function.handler;
	fn->internal_function.handler = replacement;
}

static void orma_hook_method(const char *cls, size_t cls_len,
                             const char *method, size_t method_len,
                             zif_handler replacement, zif_handler *original)
{
	if (*original != NULL) {
		return;
	}
	zend_class_entry *ce = zend_hash_str_find_ptr(CG(class_table), cls, cls_len);
	if (ce == NULL) {
		return;
	}
	zend_function *fn = zend_hash_str_find_ptr(&ce->function_table, method, method_len);
	if (fn == NULL || fn->type != ZEND_INTERNAL_FUNCTION) {
		return;
	}
	*original = fn->internal_function.handler;
	fn->internal_function.handler = replacement;
}

/* Le chiavi delle tabelle di funzioni e classi sono in minuscolo. */
#define ORMA_HOOK_FN(name, handler, slot) \
	orma_hook_function(name, sizeof(name) - 1, handler, &slot)
#define ORMA_HOOK_METHOD(cls, method, handler, slot) \
	orma_hook_method(cls, sizeof(cls) - 1, method, sizeof(method) - 1, handler, &slot)

void orma_hooks_install(void)
{
	if (ORMA_G(hooks_installed)) {
		return;
	}
	ORMA_G(hooks_installed) = true;

	ORMA_HOOK_FN("curl_exec", orma_curl_exec_handler, orig_curl_exec);
	ORMA_HOOK_FN("file_get_contents", orma_file_get_contents_handler, orig_file_get_contents);
	ORMA_HOOK_FN("mysqli_query", orma_mysqli_query_handler, orig_mysqli_query);
	ORMA_HOOK_FN("mysqli_real_query", orma_mysqli_real_query_handler, orig_mysqli_real_query);

	ORMA_HOOK_METHOD("pdo", "query", orma_pdo_query_handler, orig_pdo_query);
	ORMA_HOOK_METHOD("pdo", "exec", orma_pdo_exec_handler, orig_pdo_exec);
	ORMA_HOOK_METHOD("pdostatement", "execute",
	                 orma_pdostatement_execute_handler, orig_pdostatement_execute);
	ORMA_HOOK_METHOD("mysqli", "query",
	                 orma_mysqli_method_query_handler, orig_mysqli_method_query);
}
