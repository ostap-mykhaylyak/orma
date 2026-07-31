/* Serializzazione del frame agent to daemon.
 *
 * Formato in DESIGN.md §3. Tutti gli interi sono little-endian ed emessi in
 * modo esplicito, senza affidarsi all'ordinamento nativo.
 *
 * Le stringhe sono internate in una tabella per frame: nomi di transazione,
 * hostname e chiavi degli attributi si ripetono molto, e referenziarli per
 * indice tiene basso il payload.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_orma.h"
#include "orma_proto.h"

#include <string.h>

#define ORMA_MAX_STRINGS 64

/* Tipi di attributo. */
#define ORMA_ATTR_STRING 0
#define ORMA_ATTR_INT64  1
#define ORMA_ATTR_DOUBLE 2
#define ORMA_ATTR_BOOL   3

/* Tipi di span, allineati a OpenTelemetry. */
#define ORMA_SPAN_INTERNAL 0
#define ORMA_SPAN_SERVER   1
#define ORMA_SPAN_CLIENT   2

#define ORMA_STATUS_OK    0
#define ORMA_STATUS_ERROR 1

typedef struct _orma_strtab {
	const char *ptr[ORMA_MAX_STRINGS];
	size_t      len[ORMA_MAX_STRINGS];
	uint32_t    count;
} orma_strtab;

void orma_buf_reset(orma_buf *b)
{
	b->len = 0;
}

void orma_buf_free(orma_buf *b)
{
	if (b->data != NULL) {
		pefree(b->data, 1);
		b->data = NULL;
	}
	b->len = 0;
	b->cap = 0;
}

static bool orma_buf_reserve(orma_buf *b, size_t extra)
{
	if (b->len + extra <= b->cap) {
		return true;
	}

	size_t want = b->cap ? b->cap : 1024;
	while (want < b->len + extra) {
		want *= 2;
	}

	char *grown = perealloc(b->data, want, 1);
	if (grown == NULL) {
		return false;
	}
	b->data = grown;
	b->cap = want;
	return true;
}

static bool orma_put(orma_buf *b, const void *src, size_t n)
{
	if (!orma_buf_reserve(b, n)) {
		return false;
	}
	memcpy(b->data + b->len, src, n);
	b->len += n;
	return true;
}

static bool orma_put_u8(orma_buf *b, uint8_t v)
{
	return orma_put(b, &v, 1);
}

static bool orma_put_u16(orma_buf *b, uint16_t v)
{
	uint8_t tmp[2] = { (uint8_t)(v), (uint8_t)(v >> 8) };
	return orma_put(b, tmp, 2);
}

static bool orma_put_u32(orma_buf *b, uint32_t v)
{
	uint8_t tmp[4];
	for (int i = 0; i < 4; i++) {
		tmp[i] = (uint8_t)(v >> (8 * i));
	}
	return orma_put(b, tmp, 4);
}

static bool orma_put_u64(orma_buf *b, uint64_t v)
{
	uint8_t tmp[8];
	for (int i = 0; i < 8; i++) {
		tmp[i] = (uint8_t)(v >> (8 * i));
	}
	return orma_put(b, tmp, 8);
}

/* Interna una stringa e restituisce il suo indice. A tabella piena si ricade
 * sull'indice 0, che e' sempre la stringa vuota: un attributo perso e' meglio
 * di un frame perso. */
static uint32_t orma_intern(orma_strtab *t, const char *s, size_t len)
{
	if (s == NULL) {
		return 0;
	}
	for (uint32_t i = 0; i < t->count; i++) {
		if (t->len[i] == len && memcmp(t->ptr[i], s, len) == 0) {
			return i;
		}
	}
	if (t->count >= ORMA_MAX_STRINGS) {
		return 0;
	}
	t->ptr[t->count] = s;
	t->len[t->count] = len;
	return t->count++;
}

static uint32_t orma_intern_cstr(orma_strtab *t, const char *s)
{
	return orma_intern(t, s, s ? strlen(s) : 0);
}

bool orma_proto_encode(const orma_txn *txn, orma_buf *out)
{
	orma_strtab tab;
	tab.count = 0;

	/* L'indice 0 e' riservato alla stringa vuota. */
	orma_intern(&tab, "", 0);

	const char *app = ORMA_G(app_name);
	if (app == NULL || *app == '\0') {
		app = "default";
	}

	uint32_t app_idx    = orma_intern_cstr(&tab, app);
	uint32_t host_idx   = orma_intern_cstr(&tab, ORMA_G(hostname));
	uint32_t name_idx   = orma_intern(&tab, txn->name, txn->name_len);
	uint32_t method_key = orma_intern_cstr(&tab, "http.request.method");
	uint32_t method_idx = orma_intern_cstr(&tab, txn->method ? txn->method : "");
	uint32_t mem_key    = orma_intern_cstr(&tab, "php.memory.peak_bytes");

	orma_buf_reset(out);

	/* Spazio per la lunghezza, riempita alla fine. */
	if (!orma_put_u32(out, 0)) {
		return false;
	}

	if (!orma_put_u8(out, ORMA_PROTOCOL_VERSION)) return false;
	if (!orma_put_u8(out, 0)) return false; /* flags di frame */

	/* Tabella delle stringhe. */
	if (!orma_put_u32(out, tab.count)) return false;
	for (uint32_t i = 0; i < tab.count; i++) {
		size_t len = tab.len[i];
		if (len > 0xFFFF) {
			len = 0xFFFF;
		}
		if (!orma_put_u16(out, (uint16_t)len)) return false;
		if (len > 0 && !orma_put(out, tab.ptr[i], len)) return false;
	}

	/* Transazione. */
	if (!orma_put_u32(out, app_idx)) return false;
	if (!orma_put_u32(out, host_idx)) return false;
	if (!orma_put_u32(out, name_idx)) return false;
	if (!orma_put_u32(out, (uint32_t)getpid())) return false;
	if (!orma_put_u8(out, txn->background ? 1 : 0)) return false;
	if (!orma_put_u16(out, txn->http_status)) return false;
	if (!orma_put_u64(out, txn->start_unix_nano)) return false;
	if (!orma_put_u64(out, txn->duration_nano)) return false;
	if (!orma_put_u64(out, txn->peak_memory)) return false;
	if (!orma_put_u64(out, txn->cpu_user_nano)) return false;
	if (!orma_put_u64(out, txn->cpu_sys_nano)) return false;
	if (!orma_put_u32(out, txn->errors)) return false;
	if (!orma_put_u32(out, txn->spans_dropped)) return false;

	/* Span. Al M1 c'e' solo la radice; gli span figli arrivano al M2. */
	if (!orma_put_u32(out, 1)) return false;

	static const uint8_t no_parent[ORMA_SPAN_ID_LEN] = { 0 };

	if (!orma_put(out, txn->trace_id, ORMA_TRACE_ID_LEN)) return false;
	if (!orma_put(out, txn->span_id, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_put(out, no_parent, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_put_u32(out, name_idx)) return false;
	if (!orma_put_u8(out, txn->background ? ORMA_SPAN_INTERNAL : ORMA_SPAN_SERVER)) return false;
	if (!orma_put_u64(out, txn->start_unix_nano)) return false;
	if (!orma_put_u64(out, txn->duration_nano)) return false;
	if (!orma_put_u8(out, txn->errors > 0 ? ORMA_STATUS_ERROR : ORMA_STATUS_OK)) return false;

	/* Attributi: uno stringa e uno intero, cosi' che entrambi i tipi siano
	 * esercitati dal primo giorno invece di comparire per la prima volta al M2. */
	uint16_t attr_count = txn->background ? 1 : 2;
	if (!orma_put_u16(out, attr_count)) return false;

	if (!txn->background) {
		if (!orma_put_u32(out, method_key)) return false;
		if (!orma_put_u8(out, ORMA_ATTR_STRING)) return false;
		if (!orma_put_u32(out, method_idx)) return false;
	}

	if (!orma_put_u32(out, mem_key)) return false;
	if (!orma_put_u8(out, ORMA_ATTR_INT64)) return false;
	if (!orma_put_u64(out, txn->peak_memory)) return false;

	/* Patch della lunghezza: byte che seguono il campo stesso. */
	uint32_t frame_len = (uint32_t)(out->len - 4);
	for (int i = 0; i < 4; i++) {
		out->data[i] = (char)(uint8_t)(frame_len >> (8 * i));
	}

	return true;
}
