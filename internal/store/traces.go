package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TraceSpan e' uno span dentro un trace salvato. I nomi dei campi JSON sono
// corti perche' il payload viene scritto una volta per trace conservato.
//
// L'inizio e' relativo all'inizio della transazione: cosi' il waterfall si
// disegna senza sottrazioni, e il payload non dipende dall'ora assoluta.
type TraceSpan struct {
	ID        string `json:"i"`
	Parent    string `json:"p,omitempty"`
	Name      string `json:"n"`
	Kind      uint8  `json:"k"`
	OffsetNS  uint64 `json:"o"`
	DurNS     uint64 `json:"d"`
	Status    uint8  `json:"s,omitempty"`
	Chiamate  uint32 `json:"c,omitempty"`
	InterneNS uint64 `json:"in,omitempty"`
	// Def e' dove la funzione e' scritta: assente per le query e per le
	// funzioni interne di PHP, che non stanno in nessun file.
	Def *Posizione `json:"df,omitempty"`
	// Pila e' da dove e' partita la chiamata, dal chiamante immediato in su.
	Pila  []Posizione       `json:"pl,omitempty"`
	Attrs map[string]string `json:"a,omitempty"`
}

// Posizione e' un punto nel codice.
type Posizione struct {
	File  string `json:"f"`
	Linea uint32 `json:"l"`
}

// Vuota indica una posizione non disponibile.
func (p Posizione) Vuota() bool { return p.File == "" }

// TraceProfilo e' il costo di una funzione interna dentro un trace.
type TraceProfilo struct {
	Funzione string `json:"f"`
	Chiamate uint32 `json:"c"`
	Nano     uint64 `json:"n"`
}

// MS e' il tempo totale in millisecondi.
func (t TraceProfilo) MS() float64 { return float64(t.Nano) / 1e6 }

// PerChiamataMS distingue tante chiamate banali da poche costose.
func (t TraceProfilo) PerChiamataMS() float64 {
	if t.Chiamate == 0 {
		return 0
	}
	return t.MS() / float64(t.Chiamate)
}

// Trace e' una transazione conservata per intero.
type Trace struct {
	ID         int64
	App        string
	Name       string
	Kind       string
	TS         int64
	DurationNS uint64
	HTTPStatus uint16
	HasError   bool
	Chiamate   uint32
	// SpansDropped e' quanti span non sono entrati nel tetto. Un waterfall
	// troncato che non dichiara di esserlo fa cercare a lungo qualcosa che
	// non c'e'.
	SpansDropped uint32
	Spans        []TraceSpan
	Profilo      []TraceProfilo
}

// Troncato indica che il waterfall non e' completo.
func (t Trace) Troncato() bool {
	return t.SpansDropped > 0
}

// ProfiloMS e' il tempo attribuito alle funzioni interne, contando solo le
// chiamate piu' esterne.
//
// Non e' la somma della tabella per funzione: li' il tempo e' inclusivo, e una
// md5 dentro una preg_replace_callback comparirebbe due volte, portando il
// totale sopra la durata della richiesta. Il valore giusto arriva dallo span
// radice, dove l'agent accumula solo le chiamate non annidate.
func (t Trace) ProfiloMS() float64 {
	if len(t.Spans) > 0 && t.Spans[0].Parent == "" {
		return float64(t.Spans[0].InterneNS) / 1e6
	}

	var totale float64
	for _, p := range t.Profilo {
		totale += p.MS()
	}
	return totale
}

// DurationMS e' la durata in millisecondi.
func (t Trace) DurationMS() float64 {
	return float64(t.DurationNS) / 1e6
}

// Riga e' una riga del waterfall: uno span con la sua profondita' nell'albero.
type Riga struct {
	Span      TraceSpan
	Depth     int
	OffsetPct float64
	WidthPct  float64
	// PropriNS e' la durata meno quella dei figli registrati: e' il tempo
	// speso davvero qui. Senza, un genitore da cinque secondi con un figlio da
	// cinque secondi sembra il colpevole mentre non lo e'.
	PropriNS uint64
	// Figli registrati, per capire se il tempo proprio e' lavoro vero o solo
	// chiamate rimaste sotto soglia.
	Figli int
	// Ripetuto e' il numero di esecuzioni identiche raccolte in questa riga:
	// cento query uguali diventano una riga con cento, invece di cento righe.
	Ripetuto int
}

// InterneMS e' il tempo passato in funzioni interne dentro questo span.
func (r Riga) InterneMS() float64 {
	return float64(r.Span.InterneNS) / 1e6
}

// PropriInterneMS e' quanta parte del tempo proprio se ne va in funzioni
// interne: se e' quasi tutto, il codice non sta lavorando, sta chiamando.
func (r Riga) PropriInterneMS() float64 {
	interne := r.InterneMS()
	if interne > r.PropriMS() {
		return r.PropriMS()
	}
	return interne
}

// DurMS e' la durata dello span in millisecondi.
func (r Riga) DurMS() float64 {
	return float64(r.Span.DurNS) / 1e6
}

// PropriMS e' il tempo speso in questo span e non nei figli registrati.
func (r Riga) PropriMS() float64 {
	return float64(r.PropriNS) / 1e6
}

// PropriPct e' la quota del tempo dello span che gli appartiene davvero.
func (r Riga) PropriPct() float64 {
	if r.Span.DurNS == 0 {
		return 0
	}
	return float64(r.PropriNS) / float64(r.Span.DurNS) * 100
}

// Sospetto marca le righe su cui vale la pena guardare: quasi tutto il loro
// tempo e' proprio, quindi il rallentamento e' li' e non piu' in basso.
func (r Riga) Sospetto() bool {
	return r.PropriNS > 50*1e6 && r.PropriPct() >= 60
}

// Categoria classifica lo span per colorarlo nel waterfall.
func (r Riga) Categoria() string {
	if _, ok := r.Span.Attrs["db.statement"]; ok {
		return "db"
	}
	if _, ok := r.Span.Attrs["server.address"]; ok {
		return "ext"
	}
	return "php"
}

// Statement restituisce lo statement SQL se lo span e' una query.
func (r Riga) Statement() string {
	return r.Span.Attrs["db.statement"]
}

// Definizione e' dove la funzione e' scritta, se si sa.
func (r Riga) Definizione() *Posizione {
	return r.Span.Def
}

// Chiamante e' il punto da cui e' partita la chiamata.
func (r Riga) Chiamante() *Posizione {
	if len(r.Span.Pila) == 0 {
		return nil
	}
	return &r.Span.Pila[0]
}

// Risalita sono i livelli oltre il chiamante immediato: servono quando quello
// e' l'astrazione del framework e non chi ha davvero voluto la chiamata.
func (r Riga) Risalita() []Posizione {
	if len(r.Span.Pila) <= 1 {
		return nil
	}
	return r.Span.Pila[1:]
}

// Componente dice di chi e' il codice: quale plugin, quale tema, quale
// pacchetto. E' la risposta alla domanda che si fa davanti a una query lenta —
// "chi la esegue?" — e il percorso ce l'ha gia' dentro.
//
// Si guarda prima dove la funzione e' definita, poi si risale la pila dal
// chiamante piu' vicino: il primo pezzo di codice riconoscibile e' quello che
// ha voluto il lavoro. Le astrazioni del framework stanno nel suo core, che non
// somiglia a nessuno di questi schemi e viene scavalcato.
func (r Riga) Componente() string { return componenteSpan(r.Span) }

func componenteSpan(s TraceSpan) string {
	if s.Def != nil {
		if c := componenteDi(s.Def.File); c != "" {
			return c
		}
	}
	for _, p := range s.Pila {
		if c := componenteDi(p.File); c != "" {
			return c
		}
	}
	return ""
}

// Directory che contengono un componente per nome. Coprono WordPress
// (wp-content/plugins, themes, mu-plugins), Composer (vendor/autore/pacchetto)
// e la convenzione modules/ di Drupal e di parecchi framework.
var contenitori = map[string]int{
	"plugins":    1,
	"mu-plugins": 1,
	"themes":     1,
	"modules":    1,
	"extensions": 1,
	"vendor":     2,
}

func componenteDi(file string) string {
	parti := strings.Split(file, "/")
	for i, p := range parti {
		n, ok := contenitori[p]
		if !ok || i+n >= len(parti) {
			continue
		}
		// Il nome del componente, non il file: con vendor servono due segmenti,
		// perche' "vendor/guzzlehttp" da solo non dice quale pacchetto.
		return strings.Join(parti[i+1:i+1+n], "/")
	}
	return ""
}

// RadiceComune e' la parte iniziale condivisa da tutti i percorsi del trace.
//
// Serve solo a togliere rumore: su un'installazione tipica ogni riga
// comincerebbe con /home/utente/public_html/wp-content, che si legge una volta
// sola e poi disturba.
func (t Trace) RadiceComune() string {
	var radice string
	primo := true

	visita := func(p Posizione) {
		if p.File == "" {
			return
		}
		dir := p.File
		if i := strings.LastIndexByte(dir, '/'); i > 0 {
			dir = dir[:i+1]
		} else {
			dir = ""
		}
		if primo {
			radice = dir
			primo = false
			return
		}
		radice = prefissoComune(radice, dir)
	}

	for _, s := range t.Spans {
		if s.Def != nil {
			visita(*s.Def)
		}
		for _, p := range s.Pila {
			visita(p)
		}
	}

	// Una radice troppo corta non toglie rumore e fa solo perdere contesto.
	if len(radice) < 8 {
		return ""
	}
	// Il prefisso comune di due percorsi puo' fermarsi in mezzo a un nome:
	// wp-content e wp-config danno wp-co, che non e' una directory. In quel
	// caso si torna indietro all'ultima barra.
	if strings.HasSuffix(radice, "/") {
		return radice
	}
	if i := strings.LastIndexByte(radice, '/'); i >= 0 {
		return radice[:i+1]
	}
	return ""
}

func prefissoComune(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// Waterfall ordina gli span ad albero e calcola le proporzioni delle barre.
//
// Uno span il cui genitore non e' presente viene appeso alla radice invece di
// sparire: puo' succedere se il tetto degli span ha troncato il genitore.
//
// Fa due cose per rendere leggibile un trace di mille righe:
//
//   - raccoglie in una riga sola le query identiche ripetute sotto lo stesso
//     genitore, che sono la firma di un N+1 e da sole riempiono il waterfall;
//   - nasconde le righe sotto sogliaMS, tenendo pero' quelle che hanno un
//     discendente visibile, altrimenti si spezzerebbe l'albero.
func (t Trace) Waterfall(sogliaMS float64) []Riga {
	if len(t.Spans) == 0 {
		return nil
	}

	byID := make(map[string]TraceSpan, len(t.Spans))
	for _, s := range t.Spans {
		byID[s.ID] = s
	}

	children := make(map[string][]TraceSpan, len(t.Spans))
	var roots []TraceSpan
	for _, s := range t.Spans {
		if s.Parent == "" {
			roots = append(roots, s)
			continue
		}
		if _, ok := byID[s.Parent]; !ok {
			roots = append(roots, s)
			continue
		}
		children[s.Parent] = append(children[s.Parent], s)
	}

	total := float64(t.DurationNS)
	if total <= 0 {
		total = 1
	}

	geometria := func(offsetNS, durNS uint64) (offset, width float64) {
		width = float64(durNS) / total * 100
		if width < 0.4 {
			width = 0.4 // altrimenti gli span brevissimi diventano invisibili
		}
		if width > 100 {
			width = 100
		}
		offset = float64(offsetNS) / total * 100
		if offset+width > 100 {
			offset = 100 - width
		}
		if offset < 0 {
			offset = 0
		}
		return offset, width
	}

	var out []Riga
	var visit func(span TraceSpan, depth int)
	visit = func(span TraceSpan, depth int) {
		kids := children[span.ID]
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].OffsetNS < kids[j].OffsetNS })

		var figliNS uint64
		for _, kid := range kids {
			figliNS += kid.DurNS
		}
		propri := uint64(0)
		if span.DurNS > figliNS {
			propri = span.DurNS - figliNS
		}

		offset, width := geometria(span.OffsetNS, span.DurNS)
		out = append(out, Riga{
			Span: span, Depth: depth, OffsetPct: offset, WidthPct: width,
			PropriNS: propri, Figli: len(kids),
		})

		// Quante volte ricorre ogni query fra i figli diretti.
		ricorrenze := make(map[string]int)
		for _, kid := range kids {
			if stmt := statementDi(kid); stmt != "" {
				ricorrenze[stmt]++
			}
		}
		emesse := make(map[string]bool)

		for _, kid := range kids {
			stmt := statementDi(kid)
			if stmt != "" && ricorrenze[stmt] >= 3 {
				if emesse[stmt] {
					continue
				}
				emesse[stmt] = true

				var totaleNS, maxNS uint64
				primo := kid
				for _, altro := range kids {
					if statementDi(altro) != stmt {
						continue
					}
					totaleNS += altro.DurNS
					if altro.DurNS > maxNS {
						maxNS = altro.DurNS
					}
				}

				raccolto := primo
				raccolto.DurNS = totaleNS
				o, w := geometria(primo.OffsetNS, maxNS)
				out = append(out, Riga{
					Span: raccolto, Depth: depth + 1, OffsetPct: o, WidthPct: w,
					PropriNS: totaleNS, Ripetuto: ricorrenze[stmt],
				})
				continue
			}
			visit(kid, depth+1)
		}
	}

	for _, root := range roots {
		visit(root, 0)
	}

	return filtraPerDurata(out, sogliaMS)
}

// filtraPerDurata nasconde le righe brevi, conservando quelle che portano a
// una riga visibile: nascondere un genitore lascerebbe i figli senza contesto.
func filtraPerDurata(righe []Riga, sogliaMS float64) []Riga {
	if sogliaMS <= 0 || len(righe) == 0 {
		return righe
	}
	soglia := uint64(sogliaMS * 1e6)

	maxDepth := 0
	for _, r := range righe {
		if r.Depth > maxDepth {
			maxDepth = r.Depth
		}
	}

	tieni := make([]bool, len(righe))
	// discendenti[d] indica se sotto la profondita' d c'e' qualcosa da tenere.
	discendenti := make([]bool, maxDepth+2)

	// A ritroso: quando si incontra una riga, i suoi discendenti sono gia'
	// stati esaminati.
	for i := len(righe) - 1; i >= 0; i-- {
		d := righe[i].Depth
		tieni[i] = righe[i].Depth == 0 || righe[i].Span.DurNS >= soglia || discendenti[d+1]

		for k := d + 1; k <= maxDepth+1; k++ {
			discendenti[k] = false
		}
		if tieni[i] && d > 0 {
			discendenti[d] = true
		}
	}

	out := make([]Riga, 0, len(righe))
	for i, r := range righe {
		if tieni[i] {
			out = append(out, r)
		}
	}
	return out
}

const traceSchema = `
CREATE TABLE IF NOT EXISTS traces (
	id          INTEGER PRIMARY KEY,
	app_id      INTEGER NOT NULL,
	ts          INTEGER NOT NULL,
	txn_name    TEXT    NOT NULL,
	kind        TEXT    NOT NULL,
	duration_ns INTEGER NOT NULL,
	http_status INTEGER NOT NULL,
	has_error   INTEGER NOT NULL,
	chiamate    INTEGER NOT NULL DEFAULT 0,
	span_persi  INTEGER NOT NULL DEFAULT 0,
	-- TEXT e non BLOB: le funzioni json_* di SQLite lavorano sul testo.
	spans       TEXT    NOT NULL,
	profilo     TEXT    NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS traces_ts ON traces (ts);
CREATE INDEX IF NOT EXISTS traces_durata ON traces (duration_ns DESC);
`

// TraceRow e' una riga dell'elenco dei trace.
type TraceRow struct {
	ID         int64
	TS         int64
	Name       string
	Kind       string
	DurationMS float64
	HTTPStatus uint16
	HasError   bool
	Spans      int
}

// Traces elenca i trace conservati, dal piu' lento.
func (s *Store) Traces(since int64, limit int) ([]TraceRow, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, txn_name, kind, duration_ns, http_status, has_error,
		        json_array_length(spans)
		   FROM traces
		  WHERE ts >= ?
		  ORDER BY duration_ns DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TraceRow
	for rows.Next() {
		var r TraceRow
		var durNS int64
		var hasError int
		var status int
		if err := rows.Scan(&r.ID, &r.TS, &r.Name, &r.Kind, &durNS, &status, &hasError, &r.Spans); err != nil {
			return nil, err
		}
		r.DurationMS = float64(durNS) / 1e6
		r.HTTPStatus = uint16(status)
		r.HasError = hasError != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// Trace recupera un trace completo.
func (s *Store) Trace(id int64) (*Trace, error) {
	var t Trace
	var durNS int64
	var status, hasError int
	var payload []byte

	var profilo []byte
	var chiamate, spanPersi int64

	err := s.db.QueryRow(
		`SELECT t.id, a.name, t.txn_name, t.kind, t.ts, t.duration_ns,
		        t.http_status, t.has_error, t.chiamate, t.span_persi, t.spans, t.profilo
		   FROM traces t JOIN apps a ON a.id = t.app_id
		  WHERE t.id = ?`, id).
		Scan(&t.ID, &t.App, &t.Name, &t.Kind, &t.TS, &durNS, &status, &hasError,
			&chiamate, &spanPersi, &payload, &profilo)
	if err != nil {
		return nil, err
	}

	t.DurationNS = uint64(durNS)
	t.HTTPStatus = uint16(status)
	t.HasError = hasError != 0
	t.Chiamate = uint32(chiamate)
	t.SpansDropped = uint32(spanPersi)

	if err := json.Unmarshal(payload, &t.Spans); err != nil {
		return nil, fmt.Errorf("payload del trace %d illeggibile: %w", id, err)
	}
	if len(profilo) > 0 {
		// Un profilo illeggibile non deve impedire di guardare il waterfall.
		_ = json.Unmarshal(profilo, &t.Profilo)
	}
	return &t, nil
}

func writeTraces(tx *sql.Tx, appIDs map[string]int64, traces []*Trace) error {
	for _, t := range traces {
		payload, err := json.Marshal(t.Spans)
		if err != nil {
			return err
		}
		profilo, err := json.Marshal(t.Profilo)
		if err != nil {
			return err
		}
		hasError := 0
		if t.HasError {
			hasError = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO traces
			 (app_id, ts, txn_name, kind, duration_ns, http_status, has_error,
			  chiamate, span_persi, spans, profilo)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appIDs[t.App], t.TS, t.Name, t.Kind, int64(t.DurationNS),
			int(t.HTTPStatus), hasError, int64(t.Chiamate), int64(t.SpansDropped),
			string(payload), string(profilo)); err != nil {
			return err
		}
	}
	return nil
}
