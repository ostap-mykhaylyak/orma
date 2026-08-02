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
#include "orma_txn.h"

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
static zif_handler orig_mysqli_prepare;
static zif_handler orig_mysqli_method_prepare;
static zif_handler orig_mysqli_stmt_execute;
static zif_handler orig_mysqli_stmt_method_execute;

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

/* ------------------------------------------- statement preparati di mysqli */

/* Alla preparazione lo statement e' un argomento; all'esecuzione non e' piu'
 * raggiungibile da nessuna parte. Si tiene una mappa dall'handle dell'oggetto
 * mysqli_stmt all'SQL gia' offuscato, valida per la durata della richiesta.
 *
 * Offset e lunghezza nell'arena stanno in un solo intero: la mappa contiene
 * numeri, non puntatori, e l'arena puo' essere rilocata senza invalidarla. */
static void orma_stmt_ricorda(uint32_t handle, const char *sql, size_t len)
{
	if (ORMA_G(stmt_map) == NULL) {
		ALLOC_HASHTABLE(ORMA_G(stmt_map));
		zend_hash_init(ORMA_G(stmt_map), 16, NULL, NULL, 1);
	}

	char offuscato[ORMA_MAX_STATEMENT + 1];
	size_t offuscato_len = orma_sql_obfuscate(sql, len, offuscato, sizeof(offuscato));

	uint32_t off, salvato_len;
	if (!orma_arena_copy(offuscato, offuscato_len, &off, &salvato_len)) {
		return;
	}

	zval v;
	ZVAL_LONG(&v, ((zend_long)off << 16) | (zend_long)(salvato_len & 0xFFFF));
	zend_hash_index_update(ORMA_G(stmt_map), (zend_ulong)handle, &v);
}

static bool orma_stmt_ricorda_get(uint32_t handle, const char **sql, size_t *len)
{
	if (ORMA_G(stmt_map) == NULL) {
		return false;
	}
	zval *v = zend_hash_index_find(ORMA_G(stmt_map), (zend_ulong)handle);
	if (v == NULL || Z_TYPE_P(v) != IS_LONG) {
		return false;
	}

	uint32_t off = (uint32_t)(Z_LVAL_P(v) >> 16);
	uint32_t salvato_len = (uint32_t)(Z_LVAL_P(v) & 0xFFFF);
	if (salvato_len == 0) {
		return false;
	}

	*sql = orma_arena_str(off);
	*len = salvato_len;
	return true;
}

void orma_hooks_reset(void)
{
	if (ORMA_G(stmt_map) != NULL) {
		zend_hash_clean(ORMA_G(stmt_map));
	}
}

void orma_hooks_free(void)
{
	if (ORMA_G(stmt_map) != NULL) {
		zend_hash_destroy(ORMA_G(stmt_map));
		FREE_HASHTABLE(ORMA_G(stmt_map));
		ORMA_G(stmt_map) = NULL;
	}
}

static void orma_prepare_call(zif_handler original, zend_execute_data *execute_data,
                              zval *return_value, uint32_t indice_sql)
{
	const zval *sql = orma_arg(execute_data, indice_sql);

	original(execute_data, return_value);

	if (ORMA_G(txn).active && sql != NULL && Z_TYPE_P(sql) == IS_STRING
	    && return_value != NULL && Z_TYPE_P(return_value) == IS_OBJECT) {
		orma_stmt_ricorda(Z_OBJ_HANDLE_P(return_value), Z_STRVAL_P(sql), Z_STRLEN_P(sql));
	}
}

static void orma_stmt_execute_call(zif_handler original, zend_execute_data *execute_data,
                                   zval *return_value, const zval *stmt)
{
	const char *sql = NULL;
	size_t len = 0;

	if (!ORMA_G(txn).active || stmt == NULL || Z_TYPE_P(stmt) != IS_OBJECT
	    || !orma_stmt_ricorda_get(Z_OBJ_HANDLE_P(stmt), &sql, &len)) {
		/* Senza statement lo span si apre lo stesso: sapere che c'e' stata una
		 * esecuzione e quanto e' costata vale piu' di non sapere nulla. */
		int span = orma_span_open("EXECUTE", 7, ORMA_SPAN_CLIENT);
		orma_span_attr_str(span, "db.system", "mysqli", 6);
		original(execute_data, return_value);
		orma_span_close(span, orma_failed(return_value) ? ORMA_STATUS_ERROR : ORMA_STATUS_OK);
		return;
	}

	const char *op = orma_sql_operation(sql, len);
	int span = orma_span_open(op, strlen(op), ORMA_SPAN_CLIENT);
	orma_span_attr_str(span, "db.system", "mysqli", 6);
	/* Lo statement e' gia' offuscato: e' stato trattato alla preparazione. */
	orma_span_attr_str(span, "db.statement", sql, len);

	original(execute_data, return_value);

	orma_span_close(span, orma_failed(return_value) ? ORMA_STATUS_ERROR : ORMA_STATUS_OK);
}

static void orma_mysqli_prepare_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_prepare_call(orig_mysqli_prepare, execute_data, return_value, 2);
}

static void orma_mysqli_method_prepare_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_prepare_call(orig_mysqli_method_prepare, execute_data, return_value, 1);
}

static void orma_mysqli_stmt_execute_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_stmt_execute_call(orig_mysqli_stmt_execute, execute_data, return_value,
	                       orma_arg(execute_data, 1));
}

static void orma_mysqli_stmt_method_execute_handler(INTERNAL_FUNCTION_PARAMETERS)
{
	orma_stmt_execute_call(orig_mysqli_stmt_method_execute, execute_data, return_value,
	                       getThis());
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

/* ------------------------------------------- profilo delle funzioni interne */

/*
 * Il waterfall dice dove il tempo si accumula, non perche'. Un metodo che dura
 * cinque secondi senza figli sopra soglia lascia esattamente dove si era: e'
 * lento, e non si sa di cosa.
 *
 * Il profilo risponde a quella domanda contando chiamate e tempo delle funzioni
 * interne che possono davvero costare. Non produce span — sarebbero migliaia —
 * ma un totale per funzione, che e' la forma in cui l'informazione si legge:
 * "dodicimila preg_replace_callback per tre secondi" chiude l'indagine.
 *
 * Costa due letture di orologio per chiamata delle sole funzioni in elenco, che
 * e' curato apposta per escludere quelle banali chiamate a milioni.
 */

#define ORMA_PROFILO_ORIGINALE(nome) static zif_handler orma_orig_prof_##nome;
ORMA_FUNZIONI_PROFILATE(ORMA_PROFILO_ORIGINALE)

#define ORMA_PROFILO_NOME(nome) #nome,
static const char *const orma_nomi_profilati[] = {
	ORMA_FUNZIONI_PROFILATE(ORMA_PROFILO_NOME)
};

const char *orma_profilo_nome(int indice)
{
	if (indice < 0 || indice >= ORMA_PROF_TOTALE) {
		return "";
	}
	return orma_nomi_profilati[indice];
}

static void orma_profilo_chiama(int indice, zif_handler originale,
                                zend_execute_data *execute_data, zval *return_value)
{
	if (!ORMA_G(txn).active || !ORMA_G(profile_internals)) {
		originale(execute_data, return_value);
		return;
	}

	uint32_t profondita = ORMA_G(txn).profilo_profondita++;
	uint64_t inizio = orma_now_monotonic_nano();
	originale(execute_data, return_value);
	uint64_t fine = orma_now_monotonic_nano();
	if (ORMA_G(txn).profilo_profondita > 0) {
		ORMA_G(txn).profilo_profondita--;
	}

	orma_profilo *voce = &ORMA_G(txn).profilo[indice];
	voce->chiamate++;
	if (fine > inizio) {
		/* Per funzione il tempo e' inclusivo: e' quello che serve a sapere
		 * quanto costa. Nel totale entra solo la chiamata piu' esterna, per
		 * non contare due volte cio' che e' annidato. */
		voce->nanosecondi += fine - inizio;
		if (profondita == 0) {
			ORMA_G(txn).profilo_nano += fine - inizio;
		}
	}
}

#define ORMA_PROFILO_HANDLER(nome)                                            \
	static void orma_prof_##nome(INTERNAL_FUNCTION_PARAMETERS)                \
	{                                                                          \
		orma_profilo_chiama(ORMA_PROF_##nome, orma_orig_prof_##nome,          \
		                    execute_data, return_value);                      \
	}
ORMA_FUNZIONI_PROFILATE(ORMA_PROFILO_HANDLER)

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

	ORMA_HOOK_FN("mysqli_prepare", orma_mysqli_prepare_handler, orig_mysqli_prepare);
	ORMA_HOOK_FN("mysqli_stmt_execute",
	             orma_mysqli_stmt_execute_handler, orig_mysqli_stmt_execute);
	ORMA_HOOK_METHOD("mysqli", "prepare",
	                 orma_mysqli_method_prepare_handler, orig_mysqli_method_prepare);
	ORMA_HOOK_METHOD("mysqli_stmt", "execute",
	                 orma_mysqli_stmt_method_execute_handler, orig_mysqli_stmt_method_execute);

	/* Il profilo si installa per ultimo: dove una funzione e' gia' avvolta,
	 * per esempio file_get_contents, l'involucro del profilo finisce fuori e
	 * misura anche il tempo dell'altro. E' l'ordine giusto: il profilo deve
	 * riportare il costo reale della chiamata come la vede l'applicazione. */
#define ORMA_PROFILO_INSTALLA(nome)                                            \
	orma_hook_function(#nome, sizeof(#nome) - 1, orma_prof_##nome,             \
	                   &orma_orig_prof_##nome);
	ORMA_FUNZIONI_PROFILATE(ORMA_PROFILO_INSTALLA)
}
