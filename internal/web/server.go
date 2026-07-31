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
	"strconv"
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
		"ms":      func(v float64) string { return fmt.Sprintf("%.1f ms", v) },
		"num":     func(v uint64) string { return strconv.FormatUint(v, 10) },
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
		"pct":     func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) },
		"offset":  func(ns uint64) string { return fmt.Sprintf("+%.1f ms", float64(ns)/1e6) },
		"orario":  func(ts int64) string { return time.Unix(ts, 0).Format("15:04:05") },
		"apdex":   func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
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
}

func newComune(titolo, pagina string, minuti int) comune {
	return comune{
		Titolo:   titolo,
		Pagina:   pagina,
		Minuti:   minuti,
		Version:  version.Version,
		Generato: time.Now().Format("15:04:05"),
	}
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
}

type datiDatabase struct {
	comune
	Totale riepilogo
	Query  []store.SQLStat
}

type datiEsterne struct {
	comune
	Totale riepilogo
	Host   []store.HostStat
}

// intervallo legge la finestra temporale richiesta, con un'ora come default.
func intervallo(r *http.Request) (minuti int, since int64) {
	minuti = 60
	if v := r.URL.Query().Get("minuti"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60*24*30 {
			minuti = n
		}
	}
	return minuti, time.Now().Add(-time.Duration(minuti) * time.Minute).Unix()
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	summary, err := s.store.Summary(since, s.apdexTMS)
	if err != nil {
		s.fail(w, "riepilogo", err)
		return
	}
	txns, err := s.store.TopTransactions(since, 50)
	if err != nil {
		s.fail(w, "classifica delle transazioni", err)
		return
	}

	punti, passo, err := s.store.Serie(since, "")
	if err != nil {
		s.fail(w, "serie temporale", err)
		return
	}

	s.render(w, "panoramica.html", datiPanoramica{
		comune:  newComune("Panoramica", "panoramica", minuti),
		Summary: summary,
		Txns:    txns,
		Grafico: costruisciGrafico(punti, passo),
	})
}

func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	nome := r.URL.Query().Get("nome")
	if nome == "" {
		http.Error(w, "manca il nome della transazione", http.StatusBadRequest)
		return
	}

	dettaglio, err := s.store.Dettaglio(since, nome)
	if err != nil {
		s.fail(w, "dettaglio della transazione", err)
		return
	}
	punti, passo, err := s.store.Serie(since, nome)
	if err != nil {
		s.fail(w, "serie della transazione", err)
		return
	}
	tracce, err := s.store.TracceDi(since, nome, 25)
	if err != nil {
		s.fail(w, "trace della transazione", err)
		return
	}

	s.render(w, "transazione.html", datiTransazione{
		comune:    newComune(nome, "panoramica", minuti),
		Dettaglio: dettaglio,
		Grafico:   costruisciGrafico(punti, passo),
		Tracce:    tracce,
	})
}

func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	query, err := s.store.SlowSQL(since, 100)
	if err != nil {
		s.fail(w, "query lente", err)
		return
	}

	var totale riepilogo
	for _, q := range query {
		totale.Count += q.Count
		totale.TotalMS += q.TotalMS
	}

	s.render(w, "database.html", datiDatabase{
		comune: newComune("Database", "database", minuti),
		Totale: totale,
		Query:  query,
	})
}

func (s *Server) handleExternals(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	host, err := s.store.Externals(since, 100)
	if err != nil {
		s.fail(w, "chiamate esterne", err)
		return
	}

	var totale riepilogo
	for _, h := range host {
		totale.Count += h.Count
		totale.TotalMS += h.TotalMS
	}

	s.render(w, "esterne.html", datiEsterne{
		comune: newComune("Esterne", "esterne", minuti),
		Totale: totale,
		Host:   host,
	})
}

type datiErrori struct {
	comune
	Errori  []store.ErrStat
	Fatali  uint64
	Avvisi  uint64
}

func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	errori, err := s.store.Errors(since, 100)
	if err != nil {
		s.fail(w, "elenco degli errori", err)
		return
	}

	var fatali, avvisi uint64
	for _, e := range errori {
		if e.Fatale() {
			fatali += e.Count
		} else {
			avvisi += e.Count
		}
	}

	s.render(w, "errori.html", datiErrori{
		comune: newComune("Errori", "errori", minuti),
		Errori: errori,
		Fatali: fatali,
		Avvisi: avvisi,
	})
}

type datiTracce struct {
	comune
	Tracce []store.TraceRow
}

type datiTraccia struct {
	comune
	Traccia *store.Trace
	Righe   []store.Riga
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	minuti, since := intervallo(r)

	tracce, err := s.store.Traces(since, 100)
	if err != nil {
		s.fail(w, "elenco dei trace", err)
		return
	}

	s.render(w, "tracce.html", datiTracce{
		comune: newComune("Tracce", "tracce", minuti),
		Tracce: tracce,
	})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	minuti, _ := intervallo(r)

	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "identificativo del trace non valido", http.StatusBadRequest)
		return
	}

	traccia, err := s.store.Trace(id)
	if err != nil {
		http.Error(w, "trace non trovato", http.StatusNotFound)
		return
	}

	s.render(w, "traccia.html", datiTraccia{
		comune:  newComune("Trace "+traccia.Name, "tracce", minuti),
		Traccia: traccia,
		Righe:   traccia.Waterfall(),
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering fallito", "pagina", name, "errore", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("query fallita", "cosa", what, "errore", err)
	http.Error(w, "errore nel recupero dei dati: "+what, http.StatusInternalServerError)
}
