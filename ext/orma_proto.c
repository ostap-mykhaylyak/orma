/* Serializzazione del frame agent to daemon.
 *
 * Formato in DESIGN.md §3. Tutti gli interi sono little-endian ed emessi in
 * modo esplicito, senza affidarsi all'ordinamento nativo.
 *
 * Le stringhe sono internate in una tabella per frame: nomi di transazione,
 * host, chiavi degli attributi e statement SQL si ripetono molto, e
 * referenziarli per indice tiene basso il payload.
 *
 * La tabella precede il corpo, quindi tutte le stringhe vanno internate prima
 * di emetterla: da qui la passata preliminare sugli span figli. Il corpo
 * richiama poi orma_intern sulle stesse stringhe, che le ritrova senza far
 * crescere la tabella.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_orma.h"
#include "orma_proto.h"
#include "orma_span.h"

#include <string.h>

/* Tetto alle stringhe internate per frame. Una volta saturata, le nuove
 * ricadono sulla stringa vuota: si perde un attributo, non il frame. */
#define ORMA_MAX_STRINGS 1024

typedef struct _orma_strtab {
	const char *ptr[ORMA_MAX_STRINGS];
	size_t      len[ORMA_MAX_STRINGS];
	uint32_t    hash[ORMA_MAX_STRINGS];
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

bool orma_buf_append(orma_buf *b, const void *src, size_t n)
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
	return orma_buf_append(b, &v, 1);
}

static bool orma_put_u16(orma_buf *b, uint16_t v)
{
	uint8_t tmp[2] = { (uint8_t)(v), (uint8_t)(v >> 8) };
	return orma_buf_append(b, tmp, 2);
}

static bool orma_put_u32(orma_buf *b, uint32_t v)
{
	uint8_t tmp[4];
	for (int i = 0; i < 4; i++) {
		tmp[i] = (uint8_t)(v >> (8 * i));
	}
	return orma_buf_append(b, tmp, 4);
}

static bool orma_put_u64(orma_buf *b, uint64_t v)
{
	uint8_t tmp[8];
	for (int i = 0; i < 8; i++) {
		tmp[i] = (uint8_t)(v >> (8 * i));
	}
	return orma_buf_append(b, tmp, 8);
}

/* FNV-1a: confrontare prima l'hash evita quasi tutte le memcmp quando la
 * tabella e' piena di statement lunghi e simili fra loro. */
static uint32_t orma_hash(const char *s, size_t len)
{
	uint32_t h = 2166136261u;
	for (size_t i = 0; i < len; i++) {
		h ^= (uint8_t)s[i];
		h *= 16777619u;
	}
	return h;
}

/* Interna una stringa e restituisce il suo indice. A tabella piena si ricade
 * sull'indice 0, che e' sempre la stringa vuota. */
static uint32_t orma_intern(orma_strtab *t, const char *s, size_t len)
{
	if (s == NULL || len == 0) {
		return 0;
	}

	uint32_t h = orma_hash(s, len);
	for (uint32_t i = 0; i < t->count; i++) {
		if (t->hash[i] == h && t->len[i] == len && memcmp(t->ptr[i], s, len) == 0) {
			return i;
		}
	}
	if (t->count >= ORMA_MAX_STRINGS) {
		return 0;
	}

	t->ptr[t->count] = s;
	t->len[t->count] = len;
	t->hash[t->count] = h;
	return t->count++;
}

static uint32_t orma_intern_cstr(orma_strtab *t, const char *s)
{
	return orma_intern(t, s, s ? strlen(s) : 0);
}

/* Interna nomi e valori degli span figli, senza emettere nulla. */
static void orma_intern_children(orma_strtab *tab)
{
	for (uint32_t i = 0; i < ORMA_G(span_count); i++) {
		const orma_span *span = &ORMA_G(spans)[i];

		orma_intern(tab, orma_arena_str(span->name_off), span->name_len);

		for (uint8_t a = 0; a < span->attr_count; a++) {
			const orma_attr *attr = &span->attrs[a];
			orma_intern_cstr(tab, attr->key);
			if (attr->type == ORMA_ATTR_STRING) {
				orma_intern(tab, orma_arena_str(attr->str_off), attr->str_len);
			}
		}
	}
}

static bool orma_emit_children(orma_buf *out, orma_strtab *tab, const orma_txn *txn)
{
	for (uint32_t i = 0; i < ORMA_G(span_count); i++) {
		const orma_span *span = &ORMA_G(spans)[i];

		if (!orma_buf_append(out, txn->trace_id, ORMA_TRACE_ID_LEN)) return false;
		if (!orma_buf_append(out, span->span_id, ORMA_SPAN_ID_LEN)) return false;
		/* Tutti appesi alla radice: le funzioni interne instrumentate non si
		 * chiamano fra loro, quindi al M2 non c'e' annidamento da ricostruire. */
		if (!orma_buf_append(out, txn->span_id, ORMA_SPAN_ID_LEN)) return false;

		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(span->name_off), span->name_len))) return false;
		if (!orma_put_u8(out, span->kind)) return false;
		if (!orma_put_u64(out, span->start_unix_nano)) return false;
		if (!orma_put_u64(out, span->duration_nano)) return false;
		if (!orma_put_u8(out, span->status)) return false;
		if (!orma_put_u16(out, span->attr_count)) return false;

		for (uint8_t a = 0; a < span->attr_count; a++) {
			const orma_attr *attr = &span->attrs[a];

			if (!orma_put_u32(out, orma_intern_cstr(tab, attr->key))) return false;
			if (!orma_put_u8(out, attr->type)) return false;

			if (attr->type == ORMA_ATTR_STRING) {
				if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(attr->str_off), attr->str_len))) return false;
			} else {
				if (!orma_put_u64(out, (uint64_t)attr->i64)) return false;
			}
		}
	}
	return true;
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

	orma_intern_children(&tab);

	orma_buf_reset(out);

	/* Spazio per la lunghezza, riempita alla fine. */
	if (!orma_put_u32(out, 0)) return false;

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
		if (len > 0 && !orma_buf_append(out, tab.ptr[i], len)) return false;
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

	/* Span: la radice piu' i figli raccolti dagli hook. */
	if (!orma_put_u32(out, 1 + ORMA_G(span_count))) return false;

	static const uint8_t no_parent[ORMA_SPAN_ID_LEN] = { 0 };

	if (!orma_buf_append(out, txn->trace_id, ORMA_TRACE_ID_LEN)) return false;
	if (!orma_buf_append(out, txn->span_id, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_buf_append(out, no_parent, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_put_u32(out, name_idx)) return false;
	if (!orma_put_u8(out, txn->background ? ORMA_SPAN_INTERNAL : ORMA_SPAN_SERVER)) return false;
	if (!orma_put_u64(out, txn->start_unix_nano)) return false;
	if (!orma_put_u64(out, txn->duration_nano)) return false;
	if (!orma_put_u8(out, txn->errors > 0 ? ORMA_STATUS_ERROR : ORMA_STATUS_OK)) return false;

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

	if (!orma_emit_children(out, &tab, txn)) return false;

	/* Patch della lunghezza: byte che seguono il campo stesso. */
	uint32_t frame_len = (uint32_t)(out->len - 4);
	for (int i = 0; i < 4; i++) {
		out->data[i] = (char)(uint8_t)(frame_len >> (8 * i));
	}

	return true;
}
