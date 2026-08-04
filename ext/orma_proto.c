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

/* Interna chiavi e valori degli attributi aggiunti da userland. */
static void orma_intern_custom(orma_strtab *tab, const orma_txn *txn)
{
	for (uint32_t i = 0; i < txn->custom_count; i++) {
		const orma_custom_attr *attr = &txn->custom[i];
		orma_intern(tab, orma_arena_str(attr->key_off), attr->key_len);
		if (attr->type == ORMA_ATTR_STRING) {
			orma_intern(tab, orma_arena_str(attr->str_off), attr->str_len);
		}
	}
}

static bool orma_emit_custom(orma_buf *out, orma_strtab *tab, const orma_txn *txn)
{
	for (uint32_t i = 0; i < txn->custom_count; i++) {
		const orma_custom_attr *attr = &txn->custom[i];

		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(attr->key_off), attr->key_len))) return false;
		if (!orma_put_u8(out, attr->type)) return false;

		switch (attr->type) {
		case ORMA_ATTR_STRING:
			if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(attr->str_off), attr->str_len))) return false;
			break;
		case ORMA_ATTR_DOUBLE: {
			uint64_t bits;
			memcpy(&bits, &attr->dbl, sizeof(bits));
			if (!orma_put_u64(out, bits)) return false;
			break;
		}
		case ORMA_ATTR_BOOL:
			if (!orma_put_u8(out, attr->i64 ? 1 : 0)) return false;
			break;
		default:
			if (!orma_put_u64(out, (uint64_t)attr->i64)) return false;
			break;
		}
	}
	return true;
}

/* Interna i nomi delle funzioni profilate che sono state davvero chiamate. */
static void orma_intern_profilo(orma_strtab *tab, const orma_txn *txn)
{
	for (int i = 0; i < ORMA_PROF_TOTALE; i++) {
		if (txn->profilo[i].chiamate > 0) {
			orma_intern_cstr(tab, orma_profilo_nome(i));
		}
	}
}

static bool orma_emit_profilo(orma_buf *out, orma_strtab *tab, const orma_txn *txn)
{
	uint32_t voci = 0;
	for (int i = 0; i < ORMA_PROF_TOTALE; i++) {
		if (txn->profilo[i].chiamate > 0) {
			voci++;
		}
	}

	if (!orma_put_u32(out, voci)) return false;

	for (int i = 0; i < ORMA_PROF_TOTALE; i++) {
		if (txn->profilo[i].chiamate == 0) {
			continue;
		}
		if (!orma_put_u32(out, orma_intern_cstr(tab, orma_profilo_nome(i)))) return false;
		if (!orma_put_u32(out, txn->profilo[i].chiamate)) return false;
		if (!orma_put_u64(out, txn->profilo[i].nanosecondi)) return false;
	}
	return true;
}

/* Interna le stringhe degli eventi di errore. */
static void orma_intern_events(orma_strtab *tab, const orma_txn *txn)
{
	for (uint32_t i = 0; i < txn->event_count; i++) {
		const orma_error *ev = &txn->events[i];
		orma_intern(tab, orma_arena_str(ev->class_off), ev->class_len);
		orma_intern(tab, orma_arena_str(ev->msg_off), ev->msg_len);
		orma_intern(tab, orma_arena_str(ev->file_off), ev->file_len);
	}
}

static bool orma_emit_events(orma_buf *out, orma_strtab *tab, const orma_txn *txn)
{
	if (!orma_put_u32(out, txn->event_count)) return false;

	for (uint32_t i = 0; i < txn->event_count; i++) {
		const orma_error *ev = &txn->events[i];

		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(ev->class_off), ev->class_len))) return false;
		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(ev->msg_off), ev->msg_len))) return false;
		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(ev->file_off), ev->file_len))) return false;
		if (!orma_put_u32(out, ev->line)) return false;
		if (!orma_put_u8(out, ev->severita)) return false;
		if (!orma_put_u64(out, ev->unix_nano)) return false;
	}
	return true;
}

static void orma_intern_posizione(orma_strtab *tab, const orma_posizione *pos)
{
	if (pos->file_len > 0) {
		orma_intern(tab, orma_arena_str(pos->file_off), pos->file_len);
	}
}

static bool orma_emit_posizione(orma_buf *out, orma_strtab *tab, const orma_posizione *pos)
{
	uint32_t idx = 0;
	if (pos->file_len > 0) {
		idx = orma_intern(tab, orma_arena_str(pos->file_off), pos->file_len);
	}
	if (!orma_put_u32(out, idx)) return false;
	return orma_put_u32(out, pos->linea);
}

/* Interna nomi e valori degli span figli, senza emettere nulla. */
static void orma_intern_children(orma_strtab *tab)
{
	for (uint32_t i = 0; i < ORMA_G(span_count); i++) {
		const orma_span *span = &ORMA_G(spans)[i];

		orma_intern(tab, orma_arena_str(span->name_off), span->name_len);
		orma_intern_posizione(tab, &span->definizione);
		for (uint8_t p = 0; p < span->pila_n; p++) {
			orma_intern_posizione(tab, &span->pila[p]);
		}

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
		if (!orma_buf_append(out, span->parent_span_id, ORMA_SPAN_ID_LEN)) return false;

		if (!orma_put_u32(out, orma_intern(tab, orma_arena_str(span->name_off), span->name_len))) return false;
		if (!orma_put_u8(out, span->kind)) return false;
		if (!orma_put_u64(out, span->start_unix_nano)) return false;
		if (!orma_put_u64(out, span->duration_nano)) return false;
		if (!orma_put_u8(out, span->status)) return false;
		if (!orma_put_u32(out, span->chiamate)) return false;
		if (!orma_put_u64(out, span->interne_nano)) return false;

		if (!orma_emit_posizione(out, tab, &span->definizione)) return false;
		if (!orma_put_u8(out, span->pila_n)) return false;
		for (uint8_t p = 0; p < span->pila_n; p++) {
			if (!orma_emit_posizione(out, tab, &span->pila[p])) return false;
		}

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

	/* L'indice 0 e' riservato alla stringa vuota: e' cosi' che un campo
	 * facoltativo dice "niente", ed e' anche il ripiego quando la tabella e'
	 * piena. Va occupato a mano: orma_intern su una stringa vuota restituisce 0
	 * senza inserire nulla, e senza questa riga l'indice 0 finirebbe alla prima
	 * stringa vera, facendo comparire il nome dell'applicazione al posto dei
	 * campi assenti. */
	tab.ptr[0] = "";
	tab.len[0] = 0;
	tab.hash[0] = orma_hash("", 0);
	tab.count = 1;

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
	orma_intern_events(&tab, txn);
	orma_intern_custom(&tab, txn);
	orma_intern_profilo(&tab, txn);

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
	if (!orma_put_u32(out, txn->warnings)) return false;
	if (!orma_put_u32(out, txn->spans_dropped)) return false;
	/* Quante transazioni non sono arrivate al daemon dall'ultima consegna
	 * riuscita, e perche': senza questi campi il daemon non puo' sapere di
	 * essere cieco, e sapendolo non saprebbe cosa farci. */
	for (int i = 0; i < ORMA_DROP_CAUSE; i++) {
		if (!orma_put_u32(out, ORMA_G(dropped)[i])) return false;
	}
	if (!orma_put_u64(out, txn->chiamate)) return false;

	/* Span: la radice piu' i figli raccolti dagli hook. */
	if (!orma_put_u32(out, 1 + ORMA_G(span_count))) return false;

	/* Il genitore della radice e' quello remoto se la richiesta arriva da un
	 * servizio instrumentato, altrimenti tutto zero. Il daemon riconosce la
	 * radice dalla posizione, non da questo campo. */
	if (!orma_buf_append(out, txn->trace_id, ORMA_TRACE_ID_LEN)) return false;
	if (!orma_buf_append(out, txn->span_id, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_buf_append(out, txn->parent_span_id, ORMA_SPAN_ID_LEN)) return false;
	if (!orma_put_u32(out, name_idx)) return false;
	if (!orma_put_u8(out, txn->background ? ORMA_SPAN_INTERNAL : ORMA_SPAN_SERVER)) return false;
	if (!orma_put_u64(out, txn->start_unix_nano)) return false;
	if (!orma_put_u64(out, txn->duration_nano)) return false;
	if (!orma_put_u8(out, txn->errors > 0 ? ORMA_STATUS_ERROR : ORMA_STATUS_OK)) return false;
	if (!orma_put_u32(out, txn->chiamate > UINT32_MAX ? UINT32_MAX : (uint32_t)txn->chiamate)) return false;
	if (!orma_put_u64(out, txn->profilo_nano)) return false;

	/* La radice non ha ne' definizione ne' chiamante: e' la richiesta. */
	if (!orma_put_u32(out, 0)) return false;
	if (!orma_put_u32(out, 0)) return false;
	if (!orma_put_u8(out, 0)) return false;

	uint16_t attr_count = (txn->background ? 1 : 2) + (uint16_t)txn->custom_count;
	if (!orma_put_u16(out, attr_count)) return false;

	if (!txn->background) {
		if (!orma_put_u32(out, method_key)) return false;
		if (!orma_put_u8(out, ORMA_ATTR_STRING)) return false;
		if (!orma_put_u32(out, method_idx)) return false;
	}

	if (!orma_put_u32(out, mem_key)) return false;
	if (!orma_put_u8(out, ORMA_ATTR_INT64)) return false;
	if (!orma_put_u64(out, txn->peak_memory)) return false;

	if (!orma_emit_custom(out, &tab, txn)) return false;
	if (!orma_emit_children(out, &tab, txn)) return false;
	if (!orma_emit_events(out, &tab, txn)) return false;
	if (!orma_emit_profilo(out, &tab, txn)) return false;

	/* Patch della lunghezza: byte che seguono il campo stesso. */
	uint32_t frame_len = (uint32_t)(out->len - 4);
	for (int i = 0; i < 4; i++) {
		out->data[i] = (char)(uint8_t)(frame_len >> (8 * i));
	}

	return true;
}
