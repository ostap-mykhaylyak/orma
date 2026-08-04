package store

import (
	"fmt"
	"sort"
	"strings"
)

// Rilievo e' un'osservazione concreta su un trace, con il tempo che vale.
//
// Esiste perche' leggere un waterfall di mille righe per capire dove sta il
// problema e' un lavoro meccanico: guardare quale funzione interna domina,
// contare le query ripetute, cercare gli span con tanto tempo proprio. Sono
// tutte cose che si fanno con una regola, e le regole le esegue il programma.
type Rilievo struct {
	Tipo      string
	Titolo    string
	Dettaglio string
	MS        float64
	QuotaPct  float64
	Azione    string
}

// Tipi di rilievo, che la UI usa per colorarli.
const (
	RilievoProfilo     = "profilo"
	RilievoRipetizione = "ripetizione"
	RilievoQuery       = "query"
	RilievoSpan        = "span"
	RilievoAttesa      = "attesa"
)

// GruppoQuery raccoglie le esecuzioni di una stessa query dentro un trace.
type GruppoQuery struct {
	Statement string
	Chiamate  int
	TotaleMS  float64
	MaxMS     float64
}

// MediaMS e' il costo di una singola esecuzione.
func (g GruppoQuery) MediaMS() float64 {
	if g.Chiamate == 0 {
		return 0
	}
	return g.TotaleMS / float64(g.Chiamate)
}

// Ripetuta indica un sospetto di N+1: la stessa query rieseguita molte volte,
// tipicamente dentro un ciclo che carica un oggetto per volta.
func (g GruppoQuery) Ripetuta() bool {
	return g.Chiamate >= 8
}

// statement restituisce lo statement SQL di uno span, vuoto se non e' una query.
func statementDi(s TraceSpan) string {
	return s.Attrs["db.statement"]
}

func hostDi(s TraceSpan) string {
	return s.Attrs["server.address"]
}

// RiepilogoQuery raggruppa le query del trace per forma, dalla piu' costosa.
//
// In un trace con un N+1 la tabella e' molto piu' leggibile del waterfall: al
// posto di cento righe uguali c'e' una riga con il conteggio.
func (t Trace) RiepilogoQuery() []GruppoQuery {
	per := make(map[string]*GruppoQuery)

	for _, s := range t.Spans {
		stmt := statementDi(s)
		if stmt == "" {
			continue
		}
		g := per[stmt]
		if g == nil {
			g = &GruppoQuery{Statement: stmt}
			per[stmt] = g
		}
		g.Chiamate++
		ms := float64(s.DurNS) / 1e6
		g.TotaleMS += ms
		if ms > g.MaxMS {
			g.MaxMS = ms
		}
	}

	out := make([]GruppoQuery, 0, len(per))
	for _, g := range per {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotaleMS > out[j].TotaleMS })
	return out
}

// TempoQueryMS e' il tempo totale passato in query dentro il trace.
func (t Trace) TempoQueryMS() float64 {
	var totale float64
	for _, s := range t.Spans {
		if statementDi(s) != "" {
			totale += float64(s.DurNS) / 1e6
		}
	}
	return totale
}

// TempoEsterneMS e' il tempo totale passato in chiamate di rete uscenti.
func (t Trace) TempoEsterneMS() float64 {
	var totale float64
	for _, s := range t.Spans {
		if hostDi(s) != "" {
			totale += float64(s.DurNS) / 1e6
		}
	}
	return totale
}

// propriDi calcola il tempo proprio di ogni span: durata meno quella dei figli.
func (t Trace) propriDi() map[string]uint64 {
	figliNS := make(map[string]uint64, len(t.Spans))
	for _, s := range t.Spans {
		if s.Parent != "" {
			figliNS[s.Parent] += s.DurNS
		}
	}

	propri := make(map[string]uint64, len(t.Spans))
	for _, s := range t.Spans {
		if s.DurNS > figliNS[s.ID] {
			propri[s.ID] = s.DurNS - figliNS[s.ID]
		}
	}
	return propri
}

// Rilievi elenca cosa guardare, dal piu' costoso.
//
// Le soglie sono relative alla durata del trace e non assolute: su una
// richiesta da dodici secondi cento millisecondi sono rumore, su una da
// duecento sono un terzo del problema.
func (t Trace) Rilievi() []Rilievo {
	totale := t.DurationMS()
	if totale <= 0 || len(t.Spans) == 0 {
		return nil
	}

	// Soglia: il 5% della richiesta, con un minimo che eviti di segnalare
	// inezie su richieste gia' lente.
	soglia := totale * 0.05
	if soglia < 20 {
		soglia = 20
	}

	var out []Rilievo
	quota := func(ms float64) float64 { return ms / totale * 100 }

	// 1. Funzioni interne che dominano.
	for _, p := range t.Profilo {
		if p.MS() < soglia {
			continue
		}
		out = append(out, Rilievo{
			Tipo:   RilievoProfilo,
			Titolo: fmt.Sprintf("%s costa %.0f ms", p.Funzione, p.MS()),
			Dettaglio: fmt.Sprintf("%d chiamate da %.2f ms l'una",
				p.Chiamate, p.PerChiamataMS()),
			MS:       p.MS(),
			QuotaPct: quota(p.MS()),
			Azione:   azioneProfilo(p.Funzione),
		})
	}

	// 2. Query ripetute: il sospetto di N+1.
	for _, g := range t.RiepilogoQuery() {
		if !g.Ripetuta() || g.TotaleMS < soglia/2 {
			continue
		}
		dettaglio := fmt.Sprintf("%.0f ms in totale, %.1f ms l'una — %s",
			g.TotaleMS, g.MediaMS(), abbrevia(g.Statement, 90))
		if da := t.origineQuery(g.Statement); da != "" {
			dettaglio += "\nda " + da
		}

		out = append(out, Rilievo{
			Tipo:      RilievoRipetizione,
			Titolo:    fmt.Sprintf("La stessa query eseguita %d volte", g.Chiamate),
			Dettaglio: dettaglio,
			MS:        g.TotaleMS,
			QuotaPct:  quota(g.TotaleMS),
			Azione: "Sembra un ciclo che carica un oggetto per volta. " +
				"Si risolve caricando in blocco prima del ciclo.",
		})
	}

	// 3. Query singole lente.
	for _, s := range t.Spans {
		stmt := statementDi(s)
		if stmt == "" {
			continue
		}
		ms := float64(s.DurNS) / 1e6
		if ms < soglia {
			continue
		}
		dettaglio := abbrevia(stmt, 140)
		if da := posizioneUtile(s.Pila); da != "" {
			dettaglio += "\nda " + da
		}

		out = append(out, Rilievo{
			Tipo:      RilievoQuery,
			Titolo:    fmt.Sprintf("Una singola query da %.0f ms", ms),
			Dettaglio: dettaglio,
			MS:        ms,
			QuotaPct:  quota(ms),
			Azione:    azioneQuery(stmt),
		})
	}

	// 4. Span con molto tempo proprio: il codice PHP che lavora davvero.
	propri := t.propriDi()
	for _, s := range t.Spans {
		if statementDi(s) != "" || hostDi(s) != "" || s.Parent == "" {
			continue
		}
		ms := float64(propri[s.ID]) / 1e6
		if ms < soglia {
			continue
		}

		interne := float64(s.InterneNS) / 1e6
		tipo := RilievoSpan
		dettaglio := fmt.Sprintf("%.0f ms di tempo proprio su %.0f ms, %d chiamate dentro",
			ms, float64(s.DurNS)/1e6, s.Chiamate)
		azione := "Il tempo si ferma qui: e' questo codice a lavorare, non qualcosa piu' in basso."

		switch {
		case interne > ms*0.5:
			dettaglio += fmt.Sprintf(", di cui %.0f ms in funzioni interne", interne)
			azione = "La maggior parte del tempo e' in funzioni interne di PHP: " +
				"guarda la tabella delle funzioni per sapere quali."
		case s.Chiamate < 50 && interne < ms*0.1:
			tipo = RilievoAttesa
			azione = "Poche chiamate e quasi nessuna funzione interna: " +
				"sembra attesa su qualcosa che orma non vede, non lavoro."
		}

		if s.Def != nil {
			dettaglio += fmt.Sprintf(" — definita in %s:%d", s.Def.File, s.Def.Linea)
		}

		out = append(out, Rilievo{
			Tipo:      tipo,
			Titolo:    fmt.Sprintf("%s trattiene %.0f ms", abbrevia(s.Name, 70), ms),
			Dettaglio: dettaglio,
			MS:        ms,
			QuotaPct:  quota(ms),
			Azione:    azione,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].MS > out[j].MS })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// posizioneUtile sceglie il punto piu' informativo di una pila di chiamata.
//
// Prende il livello piu' lontano fra quelli registrati, non il chiamante
// immediato: quello e' quasi sempre l'astrazione del framework — su WordPress
// wpdb::query — e dire "la query viene da wpdb" non aiuta nessuno.
func posizioneUtile(pila []Posizione) string {
	for i := len(pila) - 1; i >= 0; i-- {
		if pila[i].File != "" {
			return fmt.Sprintf("%s:%d", pila[i].File, pila[i].Linea)
		}
	}
	return ""
}

// origineQuery trova da dove parte la prima esecuzione di una query.
func (t Trace) origineQuery(statement string) string {
	for _, s := range t.Spans {
		if statementDi(s) == statement {
			return posizioneUtile(s.Pila)
		}
	}
	return ""
}

func azioneProfilo(funzione string) string {
	switch funzione {
	case "preg_replace_callback", "preg_replace", "preg_match", "preg_match_all", "preg_split":
		return "Espressioni regolari su testi grossi, ripetute molte volte. " +
			"Su WordPress questa firma e' quasi sempre do_shortcode() dentro un ciclo."
	case "unserialize", "serialize":
		return "Serializzazione: tipicamente option o meta molto grandi, " +
			"lette e riscritte a ogni richiesta."
	case "json_decode", "json_encode":
		return "Strutture JSON grandi: costruttori di pagine e configurazioni di plugin."
	case "file_exists", "is_file", "is_dir", "realpath", "filemtime", "glob", "scandir":
		return "Molti accessi al filesystem: di solito ricerca di template o di traduzioni. " +
			"Un opcache con revalidate_freq alto e un disco veloce aiutano."
	case "file_get_contents", "fopen":
		return "Lettura di file: traduzioni, template o cache su disco."
	case "sleep", "usleep":
		return "Attesa esplicita nel codice: qualcuno sta dormendo dentro la richiesta."
	case "curl_multi_exec", "curl_multi_select", "get_headers", "gethostbyname", "dns_get_record":
		return "Rete: la richiesta aspetta un servizio esterno."
	default:
		return ""
	}
}

func azioneQuery(stmt string) string {
	switch {
	case contieneTutte(stmt, "SQL_CALC_FOUND_ROWS"):
		return "SQL_CALC_FOUND_ROWS obbliga MySQL a contare l'intero risultato. " +
			"Su WooCommerce si evita con una query di conteggio separata."
	case contieneTutte(stmt, "CAST(", "meta_value"):
		return "Il CAST su meta_value impedisce l'uso dell'indice: la tabella viene scandita per intero."
	case contieneTutte(stmt, "postmeta", "IN ("):
		return "Caricamento in blocco di metadati: se e' lento, spesso la tabella dei meta e' cresciuta troppo."
	case contieneTutte(stmt, "termmeta"), contieneTutte(stmt, "term_taxonomy"):
		return "Tassonomie: qualcuno sta caricando l'intero albero di categorie o attributi."
	case contieneTutte(stmt, "LIKE ?"):
		return "Una LIKE con carattere jolly iniziale non puo' usare l'indice."
	default:
		return ""
	}
}

func contieneTutte(s string, parti ...string) bool {
	for _, p := range parti {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func abbrevia(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
