/* Funzioni esposte a userland.
 *
 * Servono dove l'instrumentazione automatica non puo' arrivare: dare un nome
 * sensato a un front controller che risponde a tutti gli URL, marcare un
 * consumer di coda come lavoro di sfondo, segnalare un errore applicativo che
 * per PHP non e' un errore.
 *
 * Nessuna di queste funzioni fallisce in modo visibile: se la telemetria e'
 * disattivata o non c'e' una transazione aperta, restituiscono false e non
 * succede nulla. Un'applicazione che le chiama non deve doverle proteggere.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "zend_exceptions.h"

#include "php_orma.h"
#include "orma_api.h"
#include "orma_span.h"
#include "orma_txn.h"

#include <string.h>

/* ---------------------------------------------------------------- arginfo */

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_name_transaction, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, nome, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_background_transaction, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, nome, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_ignore, 0, 0, _IS_BOOL, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_add_attribute, 0, 2, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, chiave, IS_STRING, 0)
	ZEND_ARG_TYPE_MASK(0, valore, MAY_BE_STRING|MAY_BE_LONG|MAY_BE_DOUBLE|MAY_BE_BOOL, NULL)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_start_span, 0, 1, IS_LONG, 0)
	ZEND_ARG_TYPE_INFO(0, nome, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_end_span, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, riferimento, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_notice_error, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, messaggio, IS_STRING, 0)
	ZEND_ARG_OBJ_INFO(0, eccezione, Throwable, 1)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_orma_get_trace_id, 0, 0, IS_STRING, 1)
ZEND_END_ARG_INFO()

/* --------------------------------------------------------------- utilita' */

static bool orma_attiva(void)
{
	return ORMA_G(enabled) && ORMA_G(txn).active;
}

static void orma_imposta_nome(const zend_string *nome)
{
	orma_txn *txn = &ORMA_G(txn);

	size_t len = ZSTR_LEN(nome);
	if (len >= sizeof(txn->name)) {
		len = sizeof(txn->name) - 1;
	}
	memcpy(txn->name, ZSTR_VAL(nome), len);
	txn->name[len] = '\0';
	txn->name_len = len;
	txn->name_locked = true;
}

/* ---------------------------------------------------------------- funzioni */

PHP_FUNCTION(orma_name_transaction)
{
	zend_string *nome;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(nome)
	ZEND_PARSE_PARAMETERS_END();

	if (!orma_attiva() || ZSTR_LEN(nome) == 0) {
		RETURN_FALSE;
	}

	orma_imposta_nome(nome);
	RETURN_TRUE;
}

PHP_FUNCTION(orma_background_transaction)
{
	zend_string *nome;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(nome)
	ZEND_PARSE_PARAMETERS_END();

	if (!orma_attiva() || ZSTR_LEN(nome) == 0) {
		RETURN_FALSE;
	}

	orma_imposta_nome(nome);
	ORMA_G(txn).background = true;
	RETURN_TRUE;
}

PHP_FUNCTION(orma_ignore)
{
	ZEND_PARSE_PARAMETERS_NONE();

	if (!orma_attiva()) {
		RETURN_FALSE;
	}

	ORMA_G(txn).ignored = true;
	RETURN_TRUE;
}

PHP_FUNCTION(orma_add_attribute)
{
	zend_string *chiave;
	zval *valore;

	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_STR(chiave)
		Z_PARAM_ZVAL(valore)
	ZEND_PARSE_PARAMETERS_END();

	orma_txn *txn = &ORMA_G(txn);

	if (!orma_attiva() || ZSTR_LEN(chiave) == 0
	    || txn->custom_count >= ORMA_MAX_CUSTOM_ATTRS) {
		RETURN_FALSE;
	}

	orma_custom_attr *attr = &txn->custom[txn->custom_count];
	memset(attr, 0, sizeof(*attr));

	if (!orma_arena_copy(ZSTR_VAL(chiave), ZSTR_LEN(chiave), &attr->key_off, &attr->key_len)) {
		RETURN_FALSE;
	}

	switch (Z_TYPE_P(valore)) {
	case IS_STRING:
		attr->type = ORMA_ATTR_STRING;
		if (!orma_arena_copy(Z_STRVAL_P(valore), Z_STRLEN_P(valore),
		                     &attr->str_off, &attr->str_len)) {
			RETURN_FALSE;
		}
		break;
	case IS_LONG:
		attr->type = ORMA_ATTR_INT64;
		attr->i64 = (int64_t)Z_LVAL_P(valore);
		break;
	case IS_DOUBLE:
		attr->type = ORMA_ATTR_DOUBLE;
		attr->dbl = Z_DVAL_P(valore);
		break;
	case IS_TRUE:
	case IS_FALSE:
		attr->type = ORMA_ATTR_BOOL;
		attr->i64 = (Z_TYPE_P(valore) == IS_TRUE) ? 1 : 0;
		break;
	default:
		RETURN_FALSE;
	}

	txn->custom_count++;
	RETURN_TRUE;
}

PHP_FUNCTION(orma_start_span)
{
	zend_string *nome;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(nome)
	ZEND_PARSE_PARAMETERS_END();

	if (!orma_attiva() || ZSTR_LEN(nome) == 0) {
		RETURN_LONG(-1);
	}

	RETURN_LONG(orma_span_open(ZSTR_VAL(nome), ZSTR_LEN(nome), ORMA_SPAN_INTERNAL));
}

PHP_FUNCTION(orma_end_span)
{
	zend_long riferimento;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(riferimento)
	ZEND_PARSE_PARAMETERS_END();

	if (!orma_attiva() || riferimento < 0 || riferimento > INT_MAX) {
		RETURN_FALSE;
	}

	orma_span_close((int)riferimento, ORMA_STATUS_OK);
	RETURN_TRUE;
}

PHP_FUNCTION(orma_notice_error)
{
	zend_string *messaggio;
	zval *eccezione = NULL;

	ZEND_PARSE_PARAMETERS_START(1, 2)
		Z_PARAM_STR(messaggio)
		Z_PARAM_OPTIONAL
		Z_PARAM_OBJECT_OF_CLASS_OR_NULL(eccezione, zend_ce_throwable)
	ZEND_PARSE_PARAMETERS_END();

	if (!orma_attiva()) {
		RETURN_FALSE;
	}

	const char *classe = "orma_notice_error";
	size_t classe_len = sizeof("orma_notice_error") - 1;
	const char *file = "";
	size_t file_len = 0;
	uint32_t riga = 0;

	zval rv;
	if (eccezione != NULL) {
		zend_class_entry *ce = Z_OBJCE_P(eccezione);
		classe = ZSTR_VAL(ce->name);
		classe_len = ZSTR_LEN(ce->name);

		zval *z = zend_read_property(ce, Z_OBJ_P(eccezione), "file", sizeof("file") - 1, 1, &rv);
		if (z != NULL && Z_TYPE_P(z) == IS_STRING) {
			file = Z_STRVAL_P(z);
			file_len = Z_STRLEN_P(z);
		}
		z = zend_read_property(ce, Z_OBJ_P(eccezione), "line", sizeof("line") - 1, 1, &rv);
		if (z != NULL && Z_TYPE_P(z) == IS_LONG) {
			riga = (uint32_t)Z_LVAL_P(z);
		}
	}

	/* Severita' errore: e' l'applicazione a dichiarare che qualcosa non ha
	 * funzionato, e la transazione va contata come fallita. */
	orma_txn_record_event(classe, classe_len,
	                      ZSTR_VAL(messaggio), ZSTR_LEN(messaggio),
	                      file, file_len, riga, ORMA_SEVERITA_ERRORE);
	RETURN_TRUE;
}

PHP_FUNCTION(orma_get_trace_id)
{
	ZEND_PARSE_PARAMETERS_NONE();

	if (!orma_attiva()) {
		RETURN_NULL();
	}

	static const char cifre[] = "0123456789abcdef";
	char esadecimale[ORMA_TRACE_ID_LEN * 2];

	for (int i = 0; i < ORMA_TRACE_ID_LEN; i++) {
		esadecimale[i * 2] = cifre[ORMA_G(txn).trace_id[i] >> 4];
		esadecimale[i * 2 + 1] = cifre[ORMA_G(txn).trace_id[i] & 0x0F];
	}

	RETURN_STRINGL(esadecimale, sizeof(esadecimale));
}

const zend_function_entry orma_functions[] = {
	PHP_FE(orma_name_transaction,       arginfo_orma_name_transaction)
	PHP_FE(orma_background_transaction, arginfo_orma_background_transaction)
	PHP_FE(orma_ignore,                 arginfo_orma_ignore)
	PHP_FE(orma_add_attribute,          arginfo_orma_add_attribute)
	PHP_FE(orma_start_span,             arginfo_orma_start_span)
	PHP_FE(orma_end_span,               arginfo_orma_end_span)
	PHP_FE(orma_notice_error,           arginfo_orma_notice_error)
	PHP_FE(orma_get_trace_id,           arginfo_orma_get_trace_id)
	PHP_FE_END
};
