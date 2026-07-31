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
 * ingest.ProtocolVersion nel daemon. */
#define ORMA_PROTOCOL_VERSION 1

#define ORMA_TRACE_ID_LEN 16
#define ORMA_SPAN_ID_LEN  8

/* Limite al nome della transazione. Oltre si tronca: un nome lunghissimo e'
 * quasi sempre il sintomo di una normalizzazione che non ha fatto il suo lavoro. */
#define ORMA_MAX_NAME 512

/* Budget massimo, in millisecondi, che il flush puo' sottrarre alla richiesta.
 * Scaduto questo, il frame si perde e si incrementa un contatore: perdere
 * telemetria e' sempre preferibile a rallentare l'utente. */
#define ORMA_SEND_TIMEOUT_MS 5

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

typedef struct _orma_txn {
	bool     active;
	bool     background;

	uint8_t  trace_id[ORMA_TRACE_ID_LEN];
	uint8_t  span_id[ORMA_SPAN_ID_LEN];

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
	uint32_t errors;
	uint32_t spans_dropped;
} orma_txn;

ZEND_BEGIN_MODULE_GLOBALS(orma)
	/* Direttive INI. */
	bool   enabled;
	char  *app_name;
	char  *socket_path;

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
ZEND_END_MODULE_GLOBALS(orma)

/* I globals sono definiti in orma.c; gli altri file di traduzione li vedono
 * attraverso questa dichiarazione. */
ZEND_EXTERN_MODULE_GLOBALS(orma)

#define ORMA_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(orma, v)

#endif /* PHP_ORMA_H */
