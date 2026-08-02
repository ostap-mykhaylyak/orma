// Package web serve la UI. I template sono incorporati nel binario: orma si
// installa copiando un file, senza portarsi dietro una directory di asset.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/store"
	"github.com/ostap-mykhaylyak/orma/internal/version"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server e' la UI HTTP.
type Server struct {
	store    *store.Store
	log      *slog.Logger
	tmpl     *template.Template
	http     *http.Server
	apdexTMS float64
	token    string
	stato    StatoDaemon
}

// StatoDaemon fornisce i contatori interni per la pagina di auto-osservazione.
type StatoDaemon func() Stato

// Opzioni raccoglie la configurazione del server.
type Opzioni struct {
	Addr     string
	ApdexTMS float64
	Token    string
	Stato    StatoDaemon
}

// New costruisce il server.
func New(st *store.Store, log *slog.Logger, opts Opzioni) (*Server, error) {
	funcs := template.FuncMap{
		"ms":    func(v float64) string { return fmt.Sprintf("%.1f ms", v) },
		"num":   func(v uint64) string { return strconv.FormatUint(v, 10) },
		"num32": func(v uint32) string { return strconv.FormatUint(uint64(v), 10) },
		"perRich": func(v float64) string {
			if v >= 10 {
				return strconv.FormatFloat(v, 'f', 0, 64)
			}
			return strconv.FormatFloat(v, 'f', 1, 64)
		},
		"perc":    func(v float64) string { return fmt.Sprintf("%.2f%%", v) },
		"rate":    func(v float64) string { return fmt.Sprintf("%.1f/min", v) },
		"seconds": func(v float64) string { return fmt.Sprintf("%.2f s", v/1000) },
		"quota":   quota,
		"perRichiesta": func(totale float64, n uint64) float64 {
			if n == 0 {
				return 0
			}
			return totale / float64(n)
		},
		"pct":    func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) },
		"offset": func(ns uint64) string { return fmt.Sprintf("+%.1f ms", float64(ns)/1e6) },
		"orario": func(ts int64) string { return time.Unix(ts, 0).Format("15:04:05") },
		"apdex":  func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
		// Il rientro del waterfall e' limitato: un albero profondo non deve
		// spingere i nomi fuori dalla colonna.
		"indent": func(depth int) string {
			if depth > 8 {
				depth = 8
			}
			return strconv.FormatFloat(float64(depth)*0.9, 'f', 2, 64)
		},
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}

	s := &Server{
		store:    st,
		log:      log,
		tmpl:     tmpl,
		apdexTMS: opts.ApdexTMS,
		token:    opts.Token,
		stato:    opts.Stato,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.autentica(s.handleOverview))
	mux.HandleFunc("GET /transazione", s.autentica(s.handleTransaction))
	mux.HandleFunc("GET /database", s.autentica(s.handleDatabase))
	mux.HandleFunc("GET /esterne", s.autentica(s.handleExternals))
	mux.HandleFunc("GET /errori", s.autentica(s.handleErrors))
	mux.HandleFunc("GET /tracce", s.autentica(s.handleTraces))
	mux.HandleFunc("GET /traccia", s.autentica(s.handleTrace))
	mux.HandleFunc("GET /stato", s.autentica(s.handleStato))

	// La salute resta fuori dall'autenticazione: serve a un supervisore per
	// sapere se il processo risponde, e non espone dati raccolti.
	mux.HandleFunc("GET /salute", s.handleHealth)

	s.http = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// quota e' la percentuale di parte su totale, per le barre di scomposizione.
func quota(part, total float64) string {
	if total <= 0 {
		return "0"
	}
	v := part / total * 100
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Serve avvia il server e lo chiude quando il contesto termina.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
	}()

	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "orma %s\n", version.Version)
}

// comune sono i campi che ogni pagina condivide con l'intestazione.
type comune struct {
	Titolo   string
	Pagina   string
	Minuti   int
	Version  string
	Generato string
	// Statico distingue le pagine servite da quelle esportate su file: le
	// prime si collegano per URL, le seconde per nome di file, perche' un
	// export si consulta aprendo un file, senza nessun server davanti.
	Statico bool
}

func newComune(titolo, pagina string, minuti int) comune {
	return comune{
		Titolo:   titolo,
		Pagina:   pagina,
		Minuti:   minuti,
		Version:  version.Version,
		Generato: time.Now().Format("2006-01-02 15:04:05"),
	}
}

// Href e' il collegamento a una pagina. Stringa vuota per la panoramica.
func (c comune) Href(pagina string) string {
	if c.Statico {
		if pagina == "" {
			return "panoramica.html"
		}
		return pagina + ".html"
	}
	if pagina == "" {
		return fmt.Sprintf("/?minuti=%d", c.Minuti)
	}
	return fmt.Sprintf("/%s?minuti=%d", pagina, c.Minuti)
}

// HrefIntervallo e' il selettore di finestra temporale, che in un export non
// ha senso: i dati sono quelli congelati al momento della generazione.
func (c comune) HrefIntervallo(minuti int) string {
	return fmt.Sprintf("?minuti=%d", minuti)
}

// MostraIntervalli nasconde il selettore nelle pagine esportate.
func (c comune) MostraIntervalli() bool {
	return !c.Statico
}

func (c comune) HrefTraccia(id int64) string {
	if c.Statico {
		return fmt.Sprintf("traccia-%d.html", id)
	}
	return fmt.Sprintf("/traccia?id=%d&minuti=%d", id, c.Minuti)
}

func (c comune) HrefTransazione(nome string) string {
	if c.Statico {
		return "transazione-" + nomeFile(nome) + ".html"
	}
	return "/transazione?nome=" + url.QueryEscape(nome) + fmt.Sprintf("&minuti=%d", c.Minuti)
}

// nomeFile riduce un nome di transazione a qualcosa che si puo' scrivere su
// disco su qualunque filesystem, restando riconoscibile a occhio.
func nomeFile(nome string) string {
	var b strings.Builder
	for _, r := range nome {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	ridotto := strings.Trim(b.String(), "-")
	for strings.Contains(ridotto, "--") {
		ridotto = strings.ReplaceAll(ridotto, "--", "-")
	}
	if ridotto == "" {
		ridotto = "radice"
	}
	if len(ridotto) > 80 {
		ridotto = ridotto[:80]
	}
	return ridotto
}

// riepilogo e' l'aggregato mostrato nelle caselle delle pagine di dettaglio.
type riepilogo struct {
	Count   uint64
	TotalMS float64
}

func (r riepilogo) AvgMS() float64 {
	if r.Count == 0 {
		return 0
	}
	return r.TotalMS / float64(r.Count)
}

type datiPanoramica struct {
	comune
	Summary store.Summary
	Txns    []store.TxnStat
	Grafico Grafico
}

type datiTransazione struct {
	comune
	Dettaglio store.DettaglioTxn
	Grafico   Grafico
	Tracce    []store.TraceRow
	Profilo   []store.ProfStat
}

type datiDatabase struct {
	comune
	Totale    riepilogo
	Query     []store.SQLStat
	Richieste uint64
}

type datiEsterne struct {
	comune
	Totale riepilogo
	Host   []store.HostStat
}

// sinceDaMinuti converte una finestra in timestamp di partenza.
func sinceDaMinuti(minuti int) int64 {
	return time.Now().Add(-time.Duration(minuti) * time.Minute).Unix()
}

// intervallo legge la finestra temporale richiesta, con un'ora come default.
func intervallo(r *http.Request) (minuti int, since int64) {
	minuti = 60
	if v := r.URL.Query().Get("minuti"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60*24*30 {
			minuti = n
		}
	}
	return minuti, sinceDaMinuti(minuti)
}

// I costruttori sono separati dagli handler perche' servono a due padroni: le
// pagine servite e quelle esportate su file.

func (s *Server) costruisciPanoramica(since int64, c comune) (datiPanoramica, error) {
	var out datiPanoramica

	summary, err := s.store.Summary(since, s.apdexTMS)
	if err != nil {
		return out, fmt.Errorf("riepilogo: %w", err)
	}
	txns, err := s.store.TopTransactions(since, 50)
	if err != nil {
		return out, fmt.Errorf("classifica delle transazioni: %w", err)
	}
	punti, passo, err := s.store.Serie(since, "")
	if err != nil {
		return out, fmt.Errorf("serie temporale: %w", err)
	}

	return datiPanoramica{comune: c, Summary: summary, Txns: txns,
		Grafico: costruisciGrafico(punti, passo)}, nil
}

func (s *Server) costruisciTransazione(since int64, nome string, c comune) (datiTransazione, error) {
	var out datiTransazione

	dettaglio, err := s.store.Dettaglio(since, nome)
	if err != nil {
		return out, fmt.Errorf("dettaglio della transazione: %w", err)
	}
	punti, passo, err := s.store.Serie(since, nome)
	if err != nil {
		return out, fmt.Errorf("serie della transazione: %w", err)
	}
	tracce, err := s.store.TracceDi(since, nome, 25)
	if err != nil {
		return out, fmt.Errorf("trace della transazione: %w", err)
	}
	profilo, err := s.store.Profilo(since, nome, 25)
	if err != nil {
		return out, fmt.Errorf("profilo della transazione: %w", err)
	}

	return datiTransazione{comune: c, Dettaglio: dettaglio,
		Grafico: costruisciGrafico(punti, passo), Tracce: tracce, Profilo: profilo}, nil
}

func (s *Server) costruisciDatabase(since int64, c comune) (datiDatabase, error) {
	query, err := s.store.SlowSQL(since, 100)
	if err != nil {
		return datiDatabase{}, fmt.Errorf("query lente: %w", err)
	}

	// Le esecuzioni per richiesta si ricavano dal numero di richieste servite
	// nello stesso intervallo: senza il denominatore, un conteggio alto puo'
	// voler dire un ciclo oppure solo tanto traffico.
	riepilogoTot, err := s.store.Summary(since, s.apdexTMS)
	if err != nil {
		return datiDatabase{}, fmt.Errorf("riepilogo per il rapporto per richiesta: %w", err)
	}

	var totale riepilogo
	for i := range query {
		totale.Count += query[i].Count
		totale.TotalMS += query[i].TotalMS
		if riepilogoTot.Requests > 0 {
			query[i].PerRichiesta = float64(query[i].Count) / float64(riepilogoTot.Requests)
		}
	}
	return datiDatabase{comune: c, Totale: totale, Query: query, Richieste: riepilogoTot.Requests}, nil
}

func (s *Server) costruisciEsterne(since int64, c comune) (datiEsterne, error) {
	host, err := s.store.Externals(since, 100)
	if err != nil {
		return datiEsterne{}, fmt.Errorf("chiamate esterne: %w", err)
	}

	var totale riepilogo
	for _, h := range host {
		totale.Count += h.Count
		totale.TotalMS += h.TotalMS
	}
	return datiEsterne{comune: c, Totale: totale, Host: host}, nil
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	dati, err := s.costruisciPanoramica(since, newComune("Panoramica", "panoramica", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "panoramica.html", dati)
}

func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	nome := r.URL.Query().Get("nome")
	if nome == "" {
		http.Error(w, "manca il nome della transazione", http.StatusBadRequest)
		return
	}

	dati, err := s.costruisciTransazione(since, nome, newComune(nome, "panoramica", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "transazione.html", dati)
}

func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	dati, err := s.costruisciDatabase(since, newComune("Database", "database", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "database.html", dati)
}

func (s *Server) handleExternals(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	dati, err := s.costruisciEsterne(since, newComune("Esterne", "esterne", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "esterne.html", dati)
}

type datiErrori struct {
	comune
	Errori []store.ErrStat
	Fatali uint64
	Avvisi uint64
}

func (s *Server) costruisciErrori(since int64, c comune) (datiErrori, error) {
	errori, err := s.store.Errors(since, 100)
	if err != nil {
		return datiErrori{}, fmt.Errorf("elenco degli errori: %w", err)
	}

	var fatali, avvisi uint64
	for _, e := range errori {
		if e.Fatale() {
			fatali += e.Count
		} else {
			avvisi += e.Count
		}
	}
	return datiErrori{comune: c, Errori: errori, Fatali: fatali, Avvisi: avvisi}, nil
}

func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	dati, err := s.costruisciErrori(since, newComune("Errori", "errori", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "errori.html", dati)
}

type datiTracce struct {
	comune
	Tracce []store.TraceRow
}

type datiTraccia struct {
	comune
	Traccia  *store.Trace
	Righe    []store.Riga
	Nascoste int
	Soglia   float64
	Rilievi  []store.Rilievo
	Query    []store.GruppoQuery
}

// HrefSoglia costruisce il collegamento per cambiare la soglia del waterfall.
func (d datiTraccia) HrefSoglia(ms float64) string {
	return fmt.Sprintf("?id=%d&minuti=%d&min=%g", d.Traccia.ID, d.Minuti, ms)
}

func (s *Server) costruisciTracce(since int64, c comune) (datiTracce, error) {
	tracce, err := s.store.Traces(since, 100)
	if err != nil {
		return datiTracce{}, fmt.Errorf("elenco dei trace: %w", err)
	}
	return datiTracce{comune: c, Tracce: tracce}, nil
}

func (s *Server) costruisciTraccia(id int64, c comune, sogliaMS float64) (datiTraccia, error) {
	traccia, err := s.store.Trace(id)
	if err != nil {
		return datiTraccia{}, err
	}

	righe := traccia.Waterfall(sogliaMS)
	return datiTraccia{
		comune:   c,
		Traccia:  traccia,
		Righe:    righe,
		Nascoste: len(traccia.Spans) - len(righe),
		Soglia:   sogliaMS,
		Rilievi:  traccia.Rilievi(),
		Query:    traccia.RiepilogoQuery(),
	}, nil
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	dati, err := s.costruisciTracce(since, newComune("Tracce", "tracce", minuti))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "tracce.html", dati)
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	minuti, _ := intervallo(r)

	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "identificativo del trace non valido", http.StatusBadRequest)
		return
	}

	// Soglia predefinita a 1 ms: un trace di mille righe e' illeggibile, e le
	// righe sotto il millisecondo non hanno mai spiegato un rallentamento.
	soglia := 1.0
	if v := r.URL.Query().Get("min"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			soglia = n
		}
	}

	dati, err := s.costruisciTraccia(id, newComune("Trace", "tracce", minuti), soglia)
	if err != nil {
		http.Error(w, "trace non trovato", http.StatusNotFound)
		return
	}
	dati.Titolo = "Trace " + dati.Traccia.Name

	s.render(w, "traccia.html", dati)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering fallito", "pagina", name, "errore", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("recupero dei dati fallito", "errore", err)
	http.Error(w, "errore nel recupero dei dati", http.StatusInternalServerError)
}
