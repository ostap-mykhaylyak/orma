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

// Aggregator accumula in memoria e riversa su store a finestra chiusa.
type Aggregator struct {
	store *store.Store
	log   *slog.Logger

	mu      sync.Mutex
	windows map[int64]map[store.Key]*store.Bucket

	// Valvola contro l'esplosione di cardinalita': oltre questo numero di nomi
	// distinti per finestra, i nuovi confluiscono in un nome di raccolta.
	maxNames int
}

// OverflowName raccoglie le transazioni oltre il limite di cardinalita'.
const OverflowName = "OtherTransaction/*"

// New costruisce un aggregatore.
func New(st *store.Store, log *slog.Logger, maxNames int) *Aggregator {
	if maxNames <= 0 {
		maxNames = 5000
	}
	return &Aggregator{
		store:    st,
		log:      log,
		windows:  make(map[int64]map[store.Key]*store.Bucket),
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
	window := ts - ts%WindowSeconds

	app := txn.App
	if app == "" {
		app = "default"
	}
	name := txn.Name
	if name == "" {
		name = "sconosciuta"
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	buckets, ok := a.windows[window]
	if !ok {
		buckets = make(map[store.Key]*store.Bucket)
		a.windows[window] = buckets
	}

	key := store.Key{App: app, Txn: name, Category: categoryOf(txn)}
	b, ok := buckets[key]
	if !ok {
		if len(buckets) >= a.maxNames {
			key = store.Key{App: app, Txn: OverflowName, Category: key.Category}
			b, ok = buckets[key]
		}
		if !ok {
			b = &store.Bucket{MinNS: txn.DurationNano}
			buckets[key] = b
		}
	}

	b.Count++
	if isError(txn) {
		b.Errors++
	}
	b.SumNS += txn.DurationNano

	ms := float64(txn.DurationNano) / 1e6
	b.SumSqMS += ms * ms

	if txn.DurationNano < b.MinNS || b.Count == 1 {
		b.MinNS = txn.DurationNano
	}
	if txn.DurationNano > b.MaxNS {
		b.MaxNS = txn.DurationNano
	}
	b.Hist.Add(txn.DurationNano)
}

func categoryOf(txn *protocol.Transaction) string {
	if txn.Background {
		return "background"
	}
	return "web"
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
	ready := make(map[int64]map[store.Key]*store.Bucket)
	for window, buckets := range a.windows {
		if all || window < current {
			ready[window] = buckets
			delete(a.windows, window)
		}
	}
	a.mu.Unlock()

	for window, buckets := range ready {
		if len(buckets) == 0 {
			continue
		}
		if err := a.store.WriteMetrics(window, buckets); err != nil {
			// I dati sono gia' stati rimossi dalla memoria: rimetterli
			// significherebbe rischiare di crescere senza limite se il disco
			// resta rotto. Si perde la finestra e lo si dice.
			a.log.Error("scrittura delle metriche fallita, finestra persa",
				"finestra", window, "chiavi", len(buckets), "errore", err)
			continue
		}
		a.log.Debug("finestra scritta", "finestra", window, "chiavi", len(buckets))
	}
}
