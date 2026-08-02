// Package agg aggrega le transazioni in metriche a finestra di un minuto.
//
// Le metriche crescono con il numero di transazioni distinte, non con il
// traffico: e' questo che rende sostenibile lo storage. Vedi DESIGN.md §4.
package agg

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/protocol"
	"github.com/ostap-mykhaylyak/orma/internal/store"
)

// WindowSeconds e' l'ampiezza della finestra di aggregazione.
const WindowSeconds = 60

// flushInterval e' ogni quanto si controlla se ci sono finestre chiuse da
// scrivere. Piu' corto della finestra, cosi' i dati compaiono nella UI entro
// poco piu' di un minuto.
const flushInterval = 15 * time.Second

// OverflowName raccoglie le transazioni oltre il limite di cardinalita'.
const OverflowName = "OtherTransaction/*"

// Options regola cardinalita' e campionamento dei trace.
type Options struct {
	// MaxNames e' il tetto ai nomi di transazione distinti per finestra.
	MaxNames int
	// TraceThresholdNS e' la durata sopra la quale un trace viene conservato
	// per intero. Le transazioni in errore si conservano comunque.
	TraceThresholdNS uint64
	// TraceMaxPerWindow limita quanti trace si conservano al minuto.
	TraceMaxPerWindow int
	// SlowestNames e' quante transazioni distinte, fra quelle rimaste sotto
	// soglia, conservano comunque la loro esecuzione piu' lenta del minuto.
	// Serve a non restare completamente ciechi sulle transazioni veloci.
	SlowestNames int
}

func (o Options) withDefaults() Options {
	if o.MaxNames <= 0 {
		o.MaxNames = 5000
	}
	if o.TraceMaxPerWindow <= 0 {
		o.TraceMaxPerWindow = 20
	}
	if o.SlowestNames < 0 {
		o.SlowestNames = 0
	}
	return o
}

// Aggregator accumula in memoria e riversa su store a finestra chiusa.
type Aggregator struct {
	store *store.Store
	log   *slog.Logger
	opts  Options

	finestreScritte  atomic.Uint64
	finestrePerse    atomic.Uint64
	agentPerse       atomic.Uint64
	perseConnessione atomic.Uint64
	perseTimeout     atomic.Uint64
	perseScrittura   atomic.Uint64

	mu      sync.Mutex
	windows map[int64]*store.Window
	// La piu' lenta del minuto per nome, fra quelle che non hanno superato la
	// soglia. Tenuta a parte per non doverla distinguere dentro Window.
	slowest map[int64]map[string]*store.Trace
}

// New costruisce un aggregatore.
func New(st *store.Store, log *slog.Logger, opts Options) *Aggregator {
	return &Aggregator{
		store:   st,
		log:     log,
		opts:    opts.withDefaults(),
		windows: make(map[int64]*store.Window),
		slowest: make(map[int64]map[string]*store.Trace),
	}
}

// Add contabilizza una transazione. Chiamata dalla goroutine della
// connessione: prende il lock e torna subito, senza toccare il disco.
func (a *Aggregator) Add(txn *protocol.Transaction) {
	if txn == nil {
		return
	}

	if n := txn.Perse.Totale(); n > 0 {
		a.agentPerse.Add(uint64(n))
		a.perseConnessione.Add(uint64(txn.Perse.Connessione))
		a.perseTimeout.Add(uint64(txn.Perse.Timeout))
		a.perseScrittura.Add(uint64(txn.Perse.Scrittura))
	}

	ts := int64(txn.StartUnixNano / 1e9)
	if ts == 0 {
		ts = time.Now().Unix()
	}
	windowTS := ts - ts%WindowSeconds

	app := txn.App
	if app == "" {
		app = "default"
	}
	name := txn.Name
	if name == "" {
		name = "sconosciuta"
	}
	kind := "web"
	if txn.Background {
		kind = "background"
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.windows[windowTS]
	if !ok {
		w = store.NewWindow()
		a.windows[windowTS] = w
	}

	name = a.applyCardinalityCap(w, app, kind, name)

	// Totale della transazione.
	total := bucketFor(w, store.Key{App: app, Txn: name, Kind: kind, Category: store.CategoriaTotale})
	total.Count++
	if isError(txn) {
		total.Errors++
	}
	total.SumNS += txn.DurationNano
	ms := float64(txn.DurationNano) / 1e6
	total.SumSqMS += ms * ms
	if total.Count == 1 || txn.DurationNano < total.MinNS {
		total.MinNS = txn.DurationNano
	}
	if txn.DurationNano > total.MaxNS {
		total.MaxNS = txn.DurationNano
	}
	total.Hist.Add(txn.DurationNano)

	// Scomposizione per categoria e dettaglio di query e host.
	var dbNS, extNS uint64
	for i := range txn.Spans {
		// La radice e' sempre il primo span emesso. Non la si riconosce piu'
		// dal genitore nullo: con il distributed tracing la radice puo' avere
		// un genitore remoto.
		if i == 0 {
			continue
		}
		span := &txn.Spans[i]
		failed := span.Status != 0

		switch category, value := classify(span); category {
		case store.CategoriaDatabase:
			dbNS += span.DurationNano
			sql := w.SQL[store.SQLKey{App: app, Statement: value}]
			if sql == nil {
				sql = &store.Simple{}
				w.SQL[store.SQLKey{App: app, Statement: value}] = sql
			}
			sql.Add(span.DurationNano, failed)

		case store.CategoriaEsterne:
			extNS += span.DurationNano
			host := w.Hosts[store.HostKey{App: app, Host: value}]
			if host == nil {
				host = &store.Simple{}
				w.Hosts[store.HostKey{App: app, Host: value}] = host
			}
			host.Add(span.DurationNano, failed)
		}
	}

	if dbNS > 0 {
		addCategory(w, store.Key{App: app, Txn: name, Kind: kind, Category: store.CategoriaDatabase}, dbNS)
	}
	if extNS > 0 {
		addCategory(w, store.Key{App: app, Txn: name, Kind: kind, Category: store.CategoriaEsterne}, extNS)
	}

	for i := range txn.Events {
		ev := &txn.Events[i]
		key := store.ErrKey{
			App:         app,
			Fingerprint: store.Fingerprint(ev.Class, ev.Message, ev.File, ev.Line),
		}
		b := w.Errors[key]
		if b == nil {
			b = &store.ErrBucket{
				Class:    ev.Class,
				Message:  ev.Message,
				File:     ev.File,
				Line:     ev.Line,
				Txn:      name,
				Severity: uint8(ev.Severity),
			}
			w.Errors[key] = b
		}
		b.Count++
	}

	for _, voce := range txn.Profilo {
		if voce.Funzione == "" {
			continue
		}
		key := store.ProfKey{App: app, Txn: name, Funzione: voce.Funzione}
		p := w.Profilo[key]
		if p == nil {
			p = &store.ProfBucket{}
			w.Profilo[key] = p
		}
		p.Chiamate += uint64(voce.Chiamate)
		p.SumNS += voce.Nano
	}

	// Il trace completo si conserva solo se e' lento o se e' andato male: e'
	// questa regola che tiene lo storage proporzionale al numero di
	// transazioni distinte invece che al traffico.
	if a.keepTrace(txn, w) {
		w.Traces = append(w.Traces, buildTrace(txn, app, name, kind, ts))
		return
	}

	// Sotto soglia: si tiene solo la piu' lenta del minuto per quel nome, e
	// solo per un numero limitato di nomi. Senza questo, una transazione che
	// non supera mai la soglia non avrebbe mai un trace da guardare.
	a.trackSlowest(windowTS, txn, app, name, kind, ts)
}

func (a *Aggregator) trackSlowest(windowTS int64, txn *protocol.Transaction,
	app, name, kind string, ts int64) {

	if a.opts.SlowestNames == 0 {
		return
	}

	per := a.slowest[windowTS]
	if per == nil {
		per = make(map[string]*store.Trace)
		a.slowest[windowTS] = per
	}

	if corrente, ok := per[name]; ok {
		if corrente.DurationNS >= txn.DurationNano {
			return
		}
	} else if len(per) >= a.opts.SlowestNames {
		return
	}

	per[name] = buildTrace(txn, app, name, kind, ts)
}

func (a *Aggregator) keepTrace(txn *protocol.Transaction, w *store.Window) bool {
	if len(w.Traces) >= a.opts.TraceMaxPerWindow {
		return false
	}
	return isError(txn) || txn.DurationNano >= a.opts.TraceThresholdNS
}

func buildTrace(txn *protocol.Transaction, app, name, kind string, ts int64) *store.Trace {
	t := &store.Trace{
		App:          app,
		Name:         name,
		Kind:         kind,
		TS:           ts,
		DurationNS:   txn.DurationNano,
		HTTPStatus:   txn.HTTPStatus,
		HasError:     isError(txn),
		Chiamate:     txn.Chiamate,
		SpansDropped: txn.SpansDropped,
		Spans:        make([]store.TraceSpan, 0, len(txn.Spans)),
		Profilo:      make([]store.TraceProfilo, 0, len(txn.Profilo)),
	}

	for _, voce := range txn.Profilo {
		t.Profilo = append(t.Profilo, store.TraceProfilo{
			Funzione: voce.Funzione,
			Chiamate: voce.Chiamate,
			Nano:     voce.Nano,
		})
	}

	for i := range txn.Spans {
		span := &txn.Spans[i]

		// L'inizio e' relativo a quello della transazione. Un orologio
		// leggermente indietro produrrebbe un offset negativo: si azzera.
		var offset uint64
		if span.StartUnixNano > txn.StartUnixNano {
			offset = span.StartUnixNano - txn.StartUnixNano
		}

		ts := store.TraceSpan{
			ID:        hex.EncodeToString(span.SpanID[:]),
			Name:      span.Name,
			Kind:      uint8(span.Kind),
			OffsetNS:  offset,
			DurNS:     span.DurationNano,
			Status:    span.Status,
			Chiamate:  span.Chiamate,
			InterneNS: span.InterneNano,
		}
		// La radice resta senza genitore nel trace anche quando ne ha uno
		// remoto: quel genitore vive in un altro servizio e nel waterfall
		// locale non c'e' nulla a cui appenderla.
		if i > 0 {
			ts.Parent = hex.EncodeToString(span.ParentSpanID[:])
		}
		if len(span.Attrs) > 0 {
			ts.Attrs = make(map[string]string, len(span.Attrs))
			for _, attr := range span.Attrs {
				ts.Attrs[attr.Key] = fmt.Sprint(attr.Value())
			}
		}
		t.Spans = append(t.Spans, ts)
	}
	return t
}

// applyCardinalityCap fa confluire i nomi in eccesso in un nome di raccolta.
// Senza questa valvola un'applicazione con URL generati riempie lo storage in
// pochi giorni.
func (a *Aggregator) applyCardinalityCap(w *store.Window, app, kind, name string) string {
	key := store.Key{App: app, Txn: name, Kind: kind, Category: store.CategoriaTotale}
	if _, seen := w.Metrics[key]; seen {
		return name
	}
	if len(w.Metrics) < a.opts.MaxNames {
		return name
	}
	return OverflowName
}

func bucketFor(w *store.Window, key store.Key) *store.Bucket {
	b := w.Metrics[key]
	if b == nil {
		b = &store.Bucket{}
		w.Metrics[key] = b
	}
	return b
}

func addCategory(w *store.Window, key store.Key, durationNS uint64) {
	b := bucketFor(w, key)
	b.Count++
	b.SumNS += durationNS
	ms := float64(durationNS) / 1e6
	b.SumSqMS += ms * ms
	if b.Count == 1 || durationNS < b.MinNS {
		b.MinNS = durationNS
	}
	if durationNS > b.MaxNS {
		b.MaxNS = durationNS
	}
	b.Hist.Add(durationNS)
}

// classify riconosce la natura di uno span dai suoi attributi, non dal nome:
// e' l'attributo a seguire le semantic conventions, il nome no.
func classify(span *protocol.Span) (category, value string) {
	for _, attr := range span.Attrs {
		switch attr.Key {
		case "db.statement":
			return store.CategoriaDatabase, attr.Str
		case "server.address":
			return store.CategoriaEsterne, attr.Str
		}
	}
	return "", ""
}

// isError considera fallita una transazione con un errore PHP di classe fatale
// oppure con uno stato 5xx. I warning non contano: un sito pieno di
// deprecation non e' un sito rotto.
func isError(txn *protocol.Transaction) bool {
	return txn.Errors > 0 || txn.HTTPStatus >= 500
}

// Stats sono i contatori interni, per la pagina di stato e per gli allarmi.
type Stats struct {
	FinestreScritte uint64
	FinestrePerse   uint64
	// AgentPerse e' quante transazioni gli agent dichiarano di non essere
	// riusciti a consegnare. Se cresce, il daemon e' cieco su una parte del
	// traffico e nessun'altra metrica lo direbbe.
	AgentPerse uint64
	// Le stesse perdite, per causa: connessione fallita vuol dire daemon fermo
	// o permessi sbagliati, timeout vuol dire macchina carica o budget troppo
	// stretto, scrittura vuol dire socket caduto sotto l'agent.
	PerseConnessione uint64
	PerseTimeout     uint64
	PerseScrittura   uint64
	FinestreAperte   int
}

// Stats restituisce una fotografia dei contatori.
func (a *Aggregator) Stats() Stats {
	a.mu.Lock()
	aperte := len(a.windows)
	a.mu.Unlock()

	return Stats{
		FinestreScritte:  a.finestreScritte.Load(),
		FinestrePerse:    a.finestrePerse.Load(),
		AgentPerse:       a.agentPerse.Load(),
		PerseConnessione: a.perseConnessione.Load(),
		PerseTimeout:     a.perseTimeout.Load(),
		PerseScrittura:   a.perseScrittura.Load(),
		FinestreAperte:   aperte,
	}
}

// Run riversa periodicamente le finestre chiuse finche' il contesto vive, poi
// scrive anche quella corrente per non perdere l'ultimo minuto.
func (a *Aggregator) Run(ctx context.Context) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.flush(true)
			return
		case <-ticker.C:
			a.flush(false)
		}
	}
}

// flush scrive le finestre chiuse. Con all a true scrive anche quella in corso.
func (a *Aggregator) flush(all bool) {
	now := time.Now().Unix()
	current := now - now%WindowSeconds

	a.mu.Lock()
	ready := make(map[int64]*store.Window)
	for ts, w := range a.windows {
		if all || ts < current {
			for _, t := range a.slowest[ts] {
				w.Traces = append(w.Traces, t)
			}
			delete(a.slowest, ts)
			ready[ts] = w
			delete(a.windows, ts)
		}
	}
	// Finestre di sole lente senza metriche non esistono, ma se restasse una
	// mappa orfana crescerebbe per sempre.
	for ts := range a.slowest {
		if all || ts < current {
			delete(a.slowest, ts)
		}
	}
	a.mu.Unlock()

	for ts, w := range ready {
		if w.Empty() {
			continue
		}
		if err := a.store.WriteWindow(ts, w); err != nil {
			a.finestrePerse.Add(1)
			// I dati sono gia' stati tolti dalla memoria: rimetterli
			// significherebbe rischiare di crescere senza limite se il disco
			// resta rotto. Si perde la finestra e lo si dice.
			a.log.Error("scrittura delle metriche fallita, finestra persa",
				"finestra", ts, "serie", len(w.Metrics), "errore", err)
			continue
		}
		a.finestreScritte.Add(1)
		a.log.Debug("finestra scritta", "finestra", ts,
			"serie", len(w.Metrics), "query", len(w.SQL),
			"host", len(w.Hosts), "trace", len(w.Traces), "errori", len(w.Errors))
	}
}
