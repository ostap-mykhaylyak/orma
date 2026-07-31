/* orma — APM per PHP.
 * Dichiarazioni condivise dell'estensione. Vedi DESIGN.md §2 e §3.
 */

#ifndef PHP_ORMA_H
#define PHP_ORMA_H

#include <stdint.h>
#include <sys/types.h>

extern zend_module_entry orma_module_entry;
#define phpext_orma_ptr &orma_module_entry

#define PHP_ORMA_VERSION "0.1.0"

/* Versione del protocollo agent to daemon: deve restare allineata a
 * protocol.Version nel daemon. Alzata a 2 dal M4, che aggiunge la sezione
 * degli errori in coda al frame: un daemon vecchio rifiuta il frame con un
 * messaggio chiaro invece di interpretarlo male. */
#define ORMA_PROTOCOL_VERSION 2

/* Errori conservati per transazione. Oltre, si contano soltanto: cento
 * warning identici non aggiungono informazione. */
#define ORMA_MAX_ERRORS 32

/* Lunghezze massime dei campi di un errore. */
#define ORMA_MAX_ERROR_MESSAGE 500

#define ORMA_TRACE_ID_LEN 16
#define ORMA_SPAN_ID_LEN  8

/* Limite al nome della transazione. Oltre si tronca: un nome lunghissimo e'
 * quasi sempre il sintomo di una normalizzazione che non ha fatto il suo lavoro. */
#define ORMA_MAX_NAME 512

/* Budget massimo, in millisecondi, che il flush puo' sottrarre alla richiesta.
 * Scaduto questo, il frame si perde e si incrementa un contatore: perdere
 * telemetria e' sempre preferibile a rallentare l'utente. */
#define ORMA_SEND_TIMEOUT_MS 5

/* Tetto agli span per transazione. Oltre, si tronca e si conta: un trace
 * troncato e dichiarato e' utile, uno troncato in silenzio e' una bugia. */
#define ORMA_MAX_SPANS 2000

#define ORMA_MAX_SPAN_ATTRS 3

/* Lunghezza massima dello statement SQL conservato. */
#define ORMA_MAX_STATEMENT 2000

/* Tipi di attributo e di span, allineati a OpenTelemetry. */
#define ORMA_ATTR_STRING 0
#define ORMA_ATTR_INT64  1
#define ORMA_ATTR_DOUBLE 2
#define ORMA_ATTR_BOOL   3

#define ORMA_SPAN_INTERNAL 0
#define ORMA_SPAN_SERVER   1
#define ORMA_SPAN_CLIENT   2

#define ORMA_STATUS_OK    0
#define ORMA_STATUS_ERROR 1

#if defined(ZTS) && defined(COMPILE_DL_ORMA)
ZEND_TSRMLS_CACHE_EXTERN()
#endif

/* Buffer di serializzazione riusato fra richieste: allocato una volta per
 * processo, azzerato a ogni richiesta, mai liberato finche' il worker vive. */
typedef struct _orma_buf {
	char   *data;
	size_t  len;
	size_t  cap;
} orma_buf;

/* Attributo di span. Le chiavi sono stringhe statiche (semantic conventions);
 * i valori stringa vivono nell'arena e sono referenziati per offset, perche'
 * l'arena puo' essere rilocata da una realloc. */
typedef struct _orma_attr {
	const char *key;
	uint8_t     type;
	uint32_t    str_off;
	uint32_t    str_len;
	int64_t     i64;
} orma_attr;

/* Profondita' massima della pila dell'observer. Oltre, i frame non vengono
 * tracciati ma restano contati, perche' begin ed end devono restare in pari. */
#define ORMA_MAX_STACK 512

/* Valori di orma.detail. */
#define ORMA_DETAIL_OFF    0  /* nessuna instrumentazione delle funzioni utente */
#define ORMA_DETAIL_SOGLIA 1  /* solo le funzioni che superano la soglia */
#define ORMA_DETAIL_TUTTO  2  /* ogni chiamata */

typedef struct _orma_span {
	uint8_t   span_id[ORMA_SPAN_ID_LEN];
	uint8_t   parent_span_id[ORMA_SPAN_ID_LEN];
	uint32_t  name_off;
	uint32_t  name_len;
	uint8_t   kind;
	uint8_t   status;
	bool      open;
	uint64_t  start_unix_nano;
	uint64_t  start_monotonic_nano;
	uint64_t  duration_nano;
	uint8_t   attr_count;
	orma_attr attrs[ORMA_MAX_SPAN_ATTRS];
} orma_span;

/* Severita' di un evento registrato. Solo ORMA_SEVERITA_ERRORE marca la
 * transazione come fallita: un deprecation warning non e' un errore. */
#define ORMA_SEVERITA_AVVISO 0
#define ORMA_SEVERITA_ERRORE 1

/* Attributi aggiunti da userland con orma_add_attribute. A differenza di
 * quelli degli span, la chiave e' una stringa dell'utente e non una costante,
 * quindi vive nell'arena come il valore. */
#define ORMA_MAX_CUSTOM_ATTRS 8

typedef struct _orma_custom_attr {
	uint32_t key_off, key_len;
	uint8_t  type;
	uint32_t str_off, str_len;
	int64_t  i64;
	double   dbl;
} orma_custom_attr;

typedef struct _orma_error {
	uint32_t class_off, class_len;
	uint32_t msg_off, msg_len;
	uint32_t file_off, file_len;
	uint32_t line;
	uint8_t  severita;
	uint64_t unix_nano;
} orma_error;

typedef struct _orma_txn {
	bool     active;
	bool     background;
	/* Chiesta esplicitamente da orma_ignore: la transazione non viene inviata. */
	bool     ignored;
	/* Nome deciso da userland: la denominazione automatica non lo tocca. */
	bool     name_locked;

	uint8_t  trace_id[ORMA_TRACE_ID_LEN];
	uint8_t  span_id[ORMA_SPAN_ID_LEN];
	/* Genitore remoto ricevuto in un header traceparent. Tutto zero se la
	 * richiesta non arriva da un servizio instrumentato. */
	uint8_t  parent_span_id[ORMA_SPAN_ID_LEN];
	bool     remote_parent;

	uint64_t start_unix_nano;
	uint64_t start_monotonic_nano;
	uint64_t duration_nano;

	uint64_t cpu_user_start_nano;
	uint64_t cpu_sys_start_nano;
	uint64_t cpu_user_nano;
	uint64_t cpu_sys_nano;

	char     name[ORMA_MAX_NAME];
	size_t   name_len;

	const char *method;
	uint16_t http_status;
	uint64_t peak_memory;
	uint32_t errors;         /* solo eventi fatali: decide se la transazione e' fallita */
	uint32_t warnings;
	uint32_t spans_dropped;

	orma_error events[ORMA_MAX_ERRORS];
	uint32_t   event_count;

	orma_custom_attr custom[ORMA_MAX_CUSTOM_ATTRS];
	uint32_t         custom_count;
} orma_txn;

ZEND_BEGIN_MODULE_GLOBALS(orma)
	/* Direttive INI. */
	bool      enabled;
	char     *app_name;
	char     *socket_path;
	zend_long detail;
	zend_long function_ms;
	zend_long max_depth;
	char     *ignored_exceptions;

	/* Stato di processo: il socket e' per worker, mai ereditato. */
	int    sock_fd;
	pid_t  sock_pid;

	uint64_t sent_frames;
	uint64_t dropped_frames;

	uint64_t rng[4];
	bool     rng_seeded;

	char   hostname[256];

	orma_buf buf;
	orma_txn txn;

	/* Span figli e arena delle stringhe: allocati una volta per processo,
	 * azzerati a ogni richiesta. */
	orma_span *spans;
	uint32_t   span_count;
	uint32_t   span_cap;
	orma_buf   arena;

	/* Pila dell'observer sulle funzioni utente. */
	struct _orma_frame *stack;
	uint32_t            depth;
	uint32_t            stack_cap;
	uint32_t            skipped;

	bool hooks_installed;
ZEND_END_MODULE_GLOBALS(orma)

/* I globals sono definiti in orma.c; gli altri file di traduzione li vedono
 * attraverso questa dichiarazione. */
ZEND_EXTERN_MODULE_GLOBALS(orma)

#define ORMA_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(orma, v)

#endif /* PHP_ORMA_H */
