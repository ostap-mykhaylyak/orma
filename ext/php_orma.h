/* orma — APM per PHP.
 * Dichiarazioni condivise dell'estensione. Vedi DESIGN.md §2 e §3.
 */

#ifndef PHP_ORMA_H
#define PHP_ORMA_H

#include <stdint.h>
#include <sys/types.h>

extern zend_module_entry orma_module_entry;
#define phpext_orma_ptr &orma_module_entry

#define PHP_ORMA_VERSION "0.1.3"

/* Versione del protocollo agent to daemon: deve restare allineata a
 * protocol.Version nel daemon. Alzata a 2 dal M4, che aggiunge la sezione
 * degli errori in coda al frame: un daemon vecchio rifiuta il frame con un
 * messaggio chiaro invece di interpretarlo male. */
#define ORMA_PROTOCOL_VERSION 6

/* Quanti livelli di pila si registrano per ogni span.
 *
 * Uno solo non basta: il chiamante immediato di una query e' quasi sempre
 * l'astrazione del framework — su WordPress wpdb::query — e non dice nulla su
 * chi l'ha voluta. Tre livelli arrivano quasi sempre al plugin. */
#define ORMA_RIF_MAX 3

/* Percorsi distinti internati per richiesta. Oltre, si copia senza internare:
 * si spreca arena, non si perde informazione. */
#define ORMA_MAX_FILE 256

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

/* Cause per cui un frame puo' non arrivare. Tenute distinte perche' hanno
 * rimedi diversi: la connessione fallita vuol dire daemon fermo o permessi
 * sbagliati, il timeout vuol dire macchina carica o budget troppo stretto,
 * l'errore di scrittura vuol dire socket caduto sotto di noi. */
#define ORMA_DROP_CONNESSIONE 0
#define ORMA_DROP_TIMEOUT     1
#define ORMA_DROP_SCRITTURA   2
#define ORMA_DROP_CAUSE       3

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

/* Funzioni interne di cui si tiene il profilo: quante volte sono state
 * chiamate e quanto sono costate in tutto.
 *
 * Sono la risposta alla domanda che il waterfall da solo non regge: un metodo
 * che dura cinque secondi senza figli sopra soglia non dice niente, mentre
 * "dodicimila preg_replace_callback per tre secondi" dice tutto.
 *
 * L'elenco e' curato a mano: si escludono le funzioni banali chiamate a
 * milioni, dove il costo di cronometrarle supererebbe quello di eseguirle. */
#define ORMA_FUNZIONI_PROFILATE(X) \
	X(preg_match) X(preg_match_all) X(preg_replace) X(preg_replace_callback) \
	X(preg_split) X(preg_quote) \
	X(json_decode) X(json_encode) \
	X(serialize) X(unserialize) \
	X(file_get_contents) X(file_put_contents) X(fopen) X(file_exists) \
	X(is_file) X(is_dir) X(filemtime) X(filesize) X(glob) X(scandir) \
	X(unlink) X(copy) X(rename) X(mkdir) X(realpath) \
	X(gzcompress) X(gzuncompress) X(gzdecode) X(gzencode) \
	X(hash) X(md5) X(sha1) X(crc32) X(password_hash) X(password_verify) \
	X(usort) X(uasort) X(uksort) \
	X(base64_decode) X(base64_encode) \
	X(mb_convert_encoding) X(iconv) \
	X(simplexml_load_string) X(dom_import_simplexml) \
	X(imagecreatefromjpeg) X(imagecreatefrompng) X(imagejpeg) X(imagepng) \
	X(imagecopyresampled) X(imagecopyresized) \
	X(sleep) X(usleep) \
	X(curl_multi_exec) X(curl_multi_select) \
	X(opcache_compile_file) X(get_headers) X(dns_get_record) X(gethostbyname)

#define ORMA_PROFILO_INDICE(nome) ORMA_PROF_##nome,
typedef enum {
	ORMA_FUNZIONI_PROFILATE(ORMA_PROFILO_INDICE)
	ORMA_PROF_TOTALE
} orma_funzione_profilata;

typedef struct _orma_profilo {
	uint32_t chiamate;
	uint64_t nanosecondi;
} orma_profilo;

/* Un punto nel codice: file e riga. Il file vive nell'arena, referenziato per
 * offset perche' una realloc puo' spostarla. */
typedef struct _orma_posizione {
	uint32_t file_off;
	uint32_t file_len;
	uint32_t linea;
} orma_posizione;

typedef struct _orma_file_internato {
	uint32_t off;
	uint32_t len;
	uint32_t hash;
} orma_file_internato;

typedef struct _orma_span {
	uint8_t   span_id[ORMA_SPAN_ID_LEN];
	uint8_t   parent_span_id[ORMA_SPAN_ID_LEN];
	/* Chiamate di funzione utente avvenute dentro questo span, comprese
	 * quelle rimaste sotto soglia e quindi mai emesse. Distingue "lento
	 * perche' fa un milione di cose" da "lento perche' aspetta". */
	uint32_t  chiamate;
	/* Tempo passato dentro funzioni interne profilate mentre questo span era
	 * aperto. Senza, sapere che una richiesta spende il 45% in
	 * preg_replace_callback non dice ancora dove, e resta da dedurlo. */
	uint64_t  interne_nano;
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

	/* Dove la funzione e' scritta. Vuota per le funzioni interne di PHP, che
	 * non stanno in nessun file. */
	orma_posizione definizione;
	/* Da dove e' stata chiamata, risalendo la pila del codice utente. */
	orma_posizione pila[ORMA_RIF_MAX];
	uint8_t        pila_n;
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

	/* Chiamate di funzione utente in tutta la richiesta. */
	uint64_t chiamate;
	/* Tempo accumulato nelle funzioni interne profilate. Un contatore solo:
	 * la differenza fra ingresso e uscita di un frame da' il tempo interno di
	 * quello span, in tempo costante e senza orologi aggiuntivi. */
	uint64_t profilo_nano;
	/* Profondita' di annidamento fra funzioni profilate. Una md5 chiamata
	 * dentro una preg_replace_callback verrebbe contata due volte nel totale,
	 * e la somma supererebbe la durata della richiesta: nel totale entra solo
	 * la chiamata piu' esterna. Il conteggio per funzione resta inclusivo,
	 * perche' e' quello che serve a sapere quanto costa quella funzione. */
	uint32_t profilo_profondita;

	orma_profilo profilo[ORMA_PROF_TOTALE];
} orma_txn;

/* Nome della funzione profilata, per indice. */
const char *orma_profilo_nome(int indice);

ZEND_BEGIN_MODULE_GLOBALS(orma)
	/* Direttive INI. */
	bool      enabled;
	char     *app_name;
	char     *socket_path;
	zend_long detail;
	zend_long function_ms;
	zend_long max_depth;
	zend_long send_timeout_ms;
	bool      profile_internals;
	char     *ignored_exceptions;

	/* Stato di processo: il socket e' per worker, mai ereditato. */
	int    sock_fd;
	pid_t  sock_pid;

	uint64_t sent_frames;
	/* Frame persi dall'ultima consegna riuscita, per causa. Viaggiano dentro
	 * il frame successivo: e' l'unico modo perche' il daemon sappia quanto non
	 * gli e' arrivato, e perche'. Azzerati a ogni consegna riuscita. */
	uint32_t dropped[ORMA_DROP_CAUSE];
	/* Totale di processo, solo per phpinfo. */
	uint64_t dropped_total;

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

	/* I percorsi si ripetono moltissimo: internarli evita di copiare
	 * centinaia di volte la stessa stringa lunga nell'arena. */
	orma_file_internato file_tab[ORMA_MAX_FILE];
	uint32_t            file_count;

	/* Pila dell'observer sulle funzioni utente. */
	struct _orma_frame *stack;
	uint32_t            depth;
	uint32_t            stack_cap;
	uint32_t            skipped;

	/* Statement preparati: dall'handle dell'oggetto mysqli_stmt all'SQL gia'
	 * offuscato nell'arena. All'esecuzione lo statement non e' piu'
	 * accessibile, va catturato alla preparazione. */
	HashTable *stmt_map;

	bool hooks_installed;
ZEND_END_MODULE_GLOBALS(orma)

/* I globals sono definiti in orma.c; gli altri file di traduzione li vedono
 * attraverso questa dichiarazione. */
ZEND_EXTERN_MODULE_GLOBALS(orma)

#define ORMA_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(orma, v)

#endif /* PHP_ORMA_H */
