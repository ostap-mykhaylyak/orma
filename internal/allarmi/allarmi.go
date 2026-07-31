// Package allarmi valuta regole sulle metriche recenti e le riporta nel log.
//
// Non manda niente a nessuno: scrive nel log del daemon con livello WARN, e
// chi ha gia' un sistema di raccolta log lo intercetta di li'. Un APM che si
// costruisse la propria catena di notifiche duplicherebbe male qualcosa che
// esiste gia' meglio altrove.
package allarmi

import (
	"log/slog"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/agg"
	"github.com/ostap-mykhaylyak/orma/internal/store"
)

// Regole sono le soglie oltre le quali si segnala.
type Regole struct {
	// ErrorRatePct oltre il quale si segnala. Zero disattiva la regola.
	ErrorRatePct float64
	// ApdexMin sotto il quale si segnala.
	ApdexMin float64
	// P95MS oltre il quale si segnala.
	P95MS float64
	// FinestraMinuti e' l'intervallo su cui si valuta.
	FinestraMinuti int
}

// Predefinite restituisce soglie ragionevoli per un sito qualunque.
func Predefinite() Regole {
	return Regole{
		ErrorRatePct:   5,
		ApdexMin:       0.8,
		P95MS:          2000,
		FinestraMinuti: 5,
	}
}

// Valutatore applica le regole e riporta i cambi di stato.
//
// Segnala le transizioni, non lo stato: un allarme ripetuto ogni minuto
// diventa rumore che si impara a ignorare, ed e' cosi' che si perdono quelli
// veri. Si scrive quando qualcosa peggiora e quando rientra.
type Valutatore struct {
	store  *store.Store
	log    *slog.Logger
	regole Regole
	apdexT float64

	attivi          map[string]bool
	agentPerseViste uint64
}

// Nuovo costruisce un valutatore.
func Nuovo(st *store.Store, log *slog.Logger, regole Regole, apdexTMS float64) *Valutatore {
	if regole.FinestraMinuti <= 0 {
		regole.FinestraMinuti = 5
	}
	return &Valutatore{
		store:  st,
		log:    log,
		regole: regole,
		apdexT: apdexTMS,
		attivi: make(map[string]bool),
	}
}

// Valuta esamina la finestra recente e riporta i cambiamenti.
func (v *Valutatore) Valuta(stats agg.Stats) {
	since := time.Now().Add(-time.Duration(v.regole.FinestraMinuti) * time.Minute).Unix()

	riepilogo, err := v.store.Summary(since, v.apdexT)
	if err != nil {
		v.log.Error("valutazione degli allarmi fallita", "errore", err)
		return
	}

	// Su pochissime richieste ogni percentuale e' rumore: due errori su tre
	// richieste non sono un'emergenza, sono un sito senza traffico.
	if riepilogo.Requests >= 20 {
		v.regola("tasso di errore", v.regole.ErrorRatePct > 0 && riepilogo.ErrorRate() > v.regole.ErrorRatePct,
			"tasso", riepilogo.ErrorRate(), "soglia", v.regole.ErrorRatePct,
			"richieste", riepilogo.Requests, "fallite", riepilogo.Errors)

		v.regola("apdex", v.regole.ApdexMin > 0 && riepilogo.Apdex < v.regole.ApdexMin,
			"apdex", riepilogo.Apdex, "soglia", v.regole.ApdexMin,
			"apdex_t_ms", v.apdexT)

		v.regola("tempo di risposta", v.regole.P95MS > 0 && riepilogo.P95MS > v.regole.P95MS,
			"p95_ms", riepilogo.P95MS, "soglia_ms", v.regole.P95MS)
	}

	// Le perdite dell'agent non hanno soglia: qualunque valore diverso da zero
	// significa che una parte del traffico non e' stata vista.
	if stats.AgentPerse > v.agentPerseViste {
		v.log.Warn("telemetria incompleta: l'agent non e' riuscito a consegnare alcune transazioni",
			"perse_totali", stats.AgentPerse,
			"nuove", stats.AgentPerse-v.agentPerseViste)
		v.agentPerseViste = stats.AgentPerse
	}
	if stats.FinestrePerse > 0 {
		v.regola("scrittura delle metriche", true, "finestre_perse", stats.FinestrePerse)
	}
}

// regola registra la transizione di una singola condizione.
func (v *Valutatore) regola(nome string, superata bool, campi ...any) {
	giaAttivo := v.attivi[nome]

	switch {
	case superata && !giaAttivo:
		v.attivi[nome] = true
		v.log.Warn("allarme: "+nome+" oltre soglia", campi...)
	case !superata && giaAttivo:
		delete(v.attivi, nome)
		v.log.Info("rientrato: " + nome + " di nuovo entro soglia")
	}
}

// Attivi elenca gli allarmi in corso, per la pagina di stato.
func (v *Valutatore) Attivi() []string {
	var out []string
	for nome := range v.attivi {
		out = append(out, nome)
	}
	return out
}
