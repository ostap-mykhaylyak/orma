/* Stato della transazione.
 *
 * La denominazione e' il punto critico di un APM senza framework detection:
 * se si nomina per URI grezzo la cardinalita' esplode e le metriche diventano
 * rumore. Qui i segmenti ad alta cardinalita' vengono sostituiti da segnaposto.
 * Vedi DESIGN.md §2.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "SAPI.h"
#include "php_orma.h"
#include "orma_txn.h"

#include <time.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/random.h>
#include <unistd.h>

uint64_t orma_now_monotonic_nano(void)
{
	struct timespec ts;
	if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) {
		return 0;
	}
	return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

uint64_t orma_now_unix_nano(void)
{
	struct timespec ts;
	if (clock_gettime(CLOCK_REALTIME, &ts) != 0) {
		return 0;
	}
	return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

/* xoshiro256++. Gli identificativi di trace devono essere unici, non
 * imprevedibili: un PRNG veloce seminato una volta per processo evita una
 * syscall per richiesta. */
static uint64_t orma_splitmix64(uint64_t *state)
{
	uint64_t z = (*state += 0x9E3779B97F4A7C15ULL);
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
	z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
	return z ^ (z >> 31);
}

static inline uint64_t orma_rotl(const uint64_t x, int k)
{
	return (x << k) | (x >> (64 - k));
}

static uint64_t orma_rng_next(void)
{
	uint64_t *s = ORMA_G(rng);
	const uint64_t result = orma_rotl(s[0] + s[3], 23) + s[0];
	const uint64_t t = s[1] << 17;

	s[2] ^= s[0];
	s[3] ^= s[1];
	s[1] ^= s[2];
	s[0] ^= s[3];
	s[2] ^= t;
	s[3] = orma_rotl(s[3], 45);

	return result;
}

void orma_rng_seed(void)
{
	uint64_t seed = 0;

	if (getrandom(&seed, sizeof(seed), 0) != (ssize_t)sizeof(seed)) {
		/* Ripiego: unicita' sufficiente anche senza getrandom. */
		seed = (uint64_t)getpid() ^ orma_now_unix_nano();
	}

	uint64_t *s = ORMA_G(rng);
	for (int i = 0; i < 4; i++) {
		s[i] = orma_splitmix64(&seed);
	}
	ORMA_G(rng_seeded) = true;
}

void orma_rng_fill(uint8_t *out, size_t len)
{
	if (!ORMA_G(rng_seeded)) {
		orma_rng_seed();
	}

	size_t written = 0;
	while (written < len) {
		uint64_t v = orma_rng_next();
		size_t chunk = len - written;
		if (chunk > sizeof(v)) {
			chunk = sizeof(v);
		}
		memcpy(out + written, &v, chunk);
		written += chunk;
	}
}

static bool orma_seg_all_digits(const char *s, size_t n)
{
	if (n == 0) {
		return false;
	}
	for (size_t i = 0; i < n; i++) {
		if (s[i] < '0' || s[i] > '9') {
			return false;
		}
	}
	return true;
}

static inline bool orma_is_hex(char c)
{
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
}

static bool orma_seg_is_uuid(const char *s, size_t n)
{
	if (n != 36) {
		return false;
	}
	for (size_t i = 0; i < 36; i++) {
		if (i == 8 || i == 13 || i == 18 || i == 23) {
			if (s[i] != '-') {
				return false;
			}
		} else if (!orma_is_hex(s[i])) {
			return false;
		}
	}
	return true;
}

static bool orma_seg_is_hash(const char *s, size_t n)
{
	if (n < 16) {
		return false;
	}
	for (size_t i = 0; i < n; i++) {
		if (!orma_is_hex(s[i])) {
			return false;
		}
	}
	return true;
}

/* Numero massimo di segmenti conservati: oltre, la coda collassa in "/*".
 * Un URL profondissimo e' quasi sempre generato, non una vera route. */
#define ORMA_MAX_SEGMENTS 8

/* Lunghezza massima di un singolo segmento conservato. */
#define ORMA_MAX_SEGMENT_LEN 64

size_t orma_normalize_path(const char *uri, size_t uri_len, char *out, size_t out_cap)
{
	size_t written = 0;

	if (out_cap == 0) {
		return 0;
	}
	out[0] = '\0';

	if (uri == NULL || uri_len == 0) {
		return 0;
	}

	/* La query string non entra mai nel nome: e' cardinalita' pura e spesso
	 * contiene dati personali. */
	const char *q = memchr(uri, '?', uri_len);
	if (q != NULL) {
		uri_len = (size_t)(q - uri);
	}

	if (uri_len == 0) {
		if (out_cap > 1) {
			out[written++] = '/';
			out[written] = '\0';
		}
		return written;
	}

	size_t pos = 0;
	int segments = 0;

	while (pos < uri_len) {
		while (pos < uri_len && uri[pos] == '/') {
			pos++;
		}
		if (pos >= uri_len) {
			break;
		}

		size_t start = pos;
		while (pos < uri_len && uri[pos] != '/') {
			pos++;
		}
		size_t seg_len = pos - start;
		const char *seg = uri + start;

		if (segments >= ORMA_MAX_SEGMENTS) {
			const char *tail = "/*";
			if (written + 2 < out_cap) {
				memcpy(out + written, tail, 2);
				written += 2;
			}
			break;
		}

		const char *replacement = NULL;
		if (orma_seg_all_digits(seg, seg_len)) {
			replacement = "{id}";
		} else if (orma_seg_is_uuid(seg, seg_len)) {
			replacement = "{uuid}";
		} else if (orma_seg_is_hash(seg, seg_len)) {
			replacement = "{hash}";
		}

		size_t need = 1 + (replacement ? strlen(replacement)
		                               : (seg_len > ORMA_MAX_SEGMENT_LEN ? ORMA_MAX_SEGMENT_LEN : seg_len));
		if (written + need >= out_cap) {
			break;
		}

		out[written++] = '/';
		if (replacement != NULL) {
			size_t rlen = strlen(replacement);
			memcpy(out + written, replacement, rlen);
			written += rlen;
		} else {
			size_t copy = seg_len > ORMA_MAX_SEGMENT_LEN ? ORMA_MAX_SEGMENT_LEN : seg_len;
			memcpy(out + written, seg, copy);
			written += copy;
		}
		segments++;
	}

	if (written == 0 && out_cap > 1) {
		out[written++] = '/';
	}
	out[written] = '\0';
	return written;
}

/* Nome per le esecuzioni senza richiesta HTTP: cron, code, script CLI. */
static void orma_name_background(orma_txn *txn)
{
	const char *script = SG(request_info).path_translated;
	if (script == NULL || *script == '\0') {
		script = "sconosciuto";
	}

	const char *base = strrchr(script, '/');
	base = (base != NULL) ? base + 1 : script;

	int n = snprintf(txn->name, sizeof(txn->name), "cli/%s", base);
	txn->name_len = (n < 0) ? 0 : ((size_t)n >= sizeof(txn->name) ? sizeof(txn->name) - 1 : (size_t)n);
}

static void orma_txn_assign_name(orma_txn *txn)
{
	/* Nome deciso da userland: la denominazione automatica non lo tocca. */
	if (txn->name_locked) {
		return;
	}

	/* Su php-fpm SG(request_info).request_uri contiene lo SCRIPT_NAME, non
	 * l'URI richiesto: con un front controller tutte le pagine diventerebbero
	 * "/index.php" e le metriche non direbbero piu' nulla. L'URI vero sta
	 * nell'ambiente della richiesta, che si legge senza materializzare
	 * $_SERVER. Il ripiego resta per i SAPI che non lo espongono. */
	char *env_uri = sapi_getenv("REQUEST_URI", sizeof("REQUEST_URI") - 1);
	const char *uri = (env_uri != NULL && *env_uri != '\0')
	                ? env_uri
	                : SG(request_info).request_uri;

	if (uri == NULL || *uri == '\0') {
		txn->background = true;
		orma_name_background(txn);
	} else {
		txn->background = false;
		txn->name_len = orma_normalize_path(uri, strlen(uri), txn->name, sizeof(txn->name));
	}

	if (env_uri != NULL) {
		efree(env_uri);
	}
}

static void orma_cpu_times(uint64_t *user_nano, uint64_t *sys_nano)
{
	struct rusage ru;
	if (getrusage(RUSAGE_SELF, &ru) != 0) {
		*user_nano = 0;
		*sys_nano = 0;
		return;
	}
	*user_nano = (uint64_t)ru.ru_utime.tv_sec * 1000000000ULL
	           + (uint64_t)ru.ru_utime.tv_usec * 1000ULL;
	*sys_nano = (uint64_t)ru.ru_stime.tv_sec * 1000000000ULL
	          + (uint64_t)ru.ru_stime.tv_usec * 1000ULL;
}

static int orma_hex_val(char c)
{
	if (c >= '0' && c <= '9') return c - '0';
	if (c >= 'a' && c <= 'f') return c - 'a' + 10;
	if (c >= 'A' && c <= 'F') return c - 'A' + 10;
	return -1;
}

static bool orma_hex_decode(const char *s, size_t bytes, uint8_t *out)
{
	bool tutti_zero = true;

	for (size_t i = 0; i < bytes; i++) {
		int alto = orma_hex_val(s[i * 2]);
		int basso = orma_hex_val(s[i * 2 + 1]);
		if (alto < 0 || basso < 0) {
			return false;
		}
		out[i] = (uint8_t)((alto << 4) | basso);
		if (out[i] != 0) {
			tutti_zero = false;
		}
	}
	/* Un identificativo tutto a zero e' invalido secondo la specifica. */
	return !tutti_zero;
}

/* Adotta il contesto di traccia ricevuto, se c'e'.
 *
 * Formato W3C: 00-<32 esadecimali>-<16 esadecimali>-<2 esadecimali>.
 * Si legge dall'ambiente SAPI invece che da $_SERVER, per non forzare la
 * costruzione dell'autoglobale a ogni richiesta.
 */
static void orma_txn_adopt_traceparent(orma_txn *txn)
{
	char *header = sapi_getenv("HTTP_TRACEPARENT", sizeof("HTTP_TRACEPARENT") - 1);
	if (header == NULL) {
		return;
	}

	size_t len = strlen(header);
	uint8_t trace_id[ORMA_TRACE_ID_LEN];
	uint8_t parent_id[ORMA_SPAN_ID_LEN];

	if (len >= 55
	    && header[0] == '0' && header[1] == '0'
	    && header[2] == '-' && header[35] == '-' && header[52] == '-'
	    && orma_hex_decode(header + 3, ORMA_TRACE_ID_LEN, trace_id)
	    && orma_hex_decode(header + 36, ORMA_SPAN_ID_LEN, parent_id)) {

		memcpy(txn->trace_id, trace_id, ORMA_TRACE_ID_LEN);
		memcpy(txn->parent_span_id, parent_id, ORMA_SPAN_ID_LEN);
		txn->remote_parent = true;
	}

	efree(header);
}

size_t orma_txn_traceparent(char *out, size_t out_cap)
{
	static const char cifre[] = "0123456789abcdef";
	const orma_txn *txn = &ORMA_G(txn);

	if (!txn->active || out_cap < 56) {
		return 0;
	}

	size_t w = 0;
	out[w++] = '0';
	out[w++] = '0';
	out[w++] = '-';
	for (int i = 0; i < ORMA_TRACE_ID_LEN; i++) {
		out[w++] = cifre[txn->trace_id[i] >> 4];
		out[w++] = cifre[txn->trace_id[i] & 0x0F];
	}
	out[w++] = '-';
	for (int i = 0; i < ORMA_SPAN_ID_LEN; i++) {
		out[w++] = cifre[txn->span_id[i] >> 4];
		out[w++] = cifre[txn->span_id[i] & 0x0F];
	}
	/* Campionato: orma decide a valle cosa conservare, quindi il chiamante a
	 * valle non deve scartare in anticipo. */
	out[w++] = '-';
	out[w++] = '0';
	out[w++] = '1';
	out[w] = '\0';
	return w;
}

void orma_txn_begin(void)
{
	orma_txn *txn = &ORMA_G(txn);

	memset(txn, 0, sizeof(*txn));

	orma_rng_fill(txn->trace_id, ORMA_TRACE_ID_LEN);
	orma_rng_fill(txn->span_id, ORMA_SPAN_ID_LEN);
	orma_txn_adopt_traceparent(txn);

	txn->start_unix_nano = orma_now_unix_nano();
	txn->start_monotonic_nano = orma_now_monotonic_nano();
	orma_cpu_times(&txn->cpu_user_start_nano, &txn->cpu_sys_start_nano);

	txn->active = true;
}

void orma_txn_end(void)
{
	orma_txn *txn = &ORMA_G(txn);

	if (!txn->active) {
		return;
	}

	uint64_t now = orma_now_monotonic_nano();
	txn->duration_nano = (now > txn->start_monotonic_nano)
	                   ? now - txn->start_monotonic_nano
	                   : 0;

	uint64_t user_now, sys_now;
	orma_cpu_times(&user_now, &sys_now);
	txn->cpu_user_nano = (user_now > txn->cpu_user_start_nano)
	                   ? user_now - txn->cpu_user_start_nano : 0;
	txn->cpu_sys_nano = (sys_now > txn->cpu_sys_start_nano)
	                  ? sys_now - txn->cpu_sys_start_nano : 0;

	txn->peak_memory = (uint64_t)zend_memory_peak_usage(1);

	int status = SG(sapi_headers).http_response_code;
	txn->http_status = (status > 0 && status < 65536) ? (uint16_t)status : 0;

	txn->method = SG(request_info).request_method;

	orma_txn_assign_name(txn);
}
