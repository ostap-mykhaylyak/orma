// Package agg aggrega le transazioni in metriche a finestra di un minuto.
//
// Le metriche crescono con il numero di transazioni distinte, non con il
// traffico: e' questo che rende sostenibile lo storage. Vedi DESIGN.md §4.
package agg

import (
	"context"
	"log/slog"
	"sync"
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

// Aggregator accumula in memoria e riversa su store a finestra chiusa.
type Aggregator struct {
	store *store.Store
	log   *slog.Logger

	mu      sync.Mutex
	windows map[int64]*store.Window

	// Valvola contro l'esplosione di cardinalita': oltre questo numero di nomi
	// distinti per finestra, i nuovi confluiscono in un nome di raccolta.
	maxNames int
}

// New costruisce un aggregatore.
func New(st *store.Store, log *slog.Logger, maxNames int) *Aggregator {
	if maxNames <= 0 {
		maxNames = 5000
	}
	return &Aggregator{
		store:    st,
		log:      log,
		windows:  make(map[int64]*store.Window),
		maxNames: maxNames,
	}
}

// Add contabilizza una transazione. Chiamata dalla goroutine della
// connessione: prende il lock e torna subito, senza toccare il disco.
func (a *Aggregator) Add(txn *protocol.Transaction) {
	if txn == nil {
		return
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
		span := &txn.Spans[i]
		if isRoot(span) {
			continue
		}
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
}

// applyCardinalityCap fa confluire i nomi in eccesso in un nome di raccolta.
// Senza questa valvola un'applicazione con URL generati riempie lo storage in
// pochi giorni.
func (a *Aggregator) applyCardinalityCap(w *store.Window, app, kind, name string) string {
	key := store.Key{App: app, Txn: name, Kind: kind, Category: store.CategoriaTotale}
	if _, seen := w.Metrics[key]; seen {
		return name
	}
	if len(w.Metrics) < a.maxNames {
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

func isRoot(span *protocol.Span) bool {
	return span.ParentSpanID == [8]byte{}
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

// isError considera errore un 5xx o una transazione che ha registrato errori
// PHP. Il conteggio degli errori applicativi diventa preciso al M4.
func isError(txn *protocol.Transaction) bool {
	return txn.Errors > 0 || txn.HTTPStatus >= 500
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
			ready[ts] = w
			delete(a.windows, ts)
		}
	}
	a.mu.Unlock()

	for ts, w := range ready {
		if w.Empty() {
			continue
		}
		if err := a.store.WriteWindow(ts, w); err != nil {
			// I dati sono gia' stati tolti dalla memoria: rimetterli
			// significherebbe rischiare di crescere senza limite se il disco
			// resta rotto. Si perde la finestra e lo si dice.
			a.log.Error("scrittura delle metriche fallita, finestra persa",
				"finestra", ts, "serie", len(w.Metrics), "errore", err)
			continue
		}
		a.log.Debug("finestra scritta", "finestra", ts,
			"serie", len(w.Metrics), "query", len(w.SQL), "host", len(w.Hosts))
	}
}
