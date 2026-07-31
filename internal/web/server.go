// Package web serve la UI. Il template e' incorporato nel binario: orma si
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
	store *store.Store
	log   *slog.Logger
	tmpl  *template.Template
	http  *http.Server
}

// New costruisce il server sull'indirizzo indicato.
func New(addr string, st *store.Store, log *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"ms":      func(v float64) string { return fmt.Sprintf("%.1f ms", v) },
		"num":     func(v uint64) string { return strconv.FormatUint(v, 10) },
		"perc":    func(v float64) string { return fmt.Sprintf("%.2f%%", v) },
		"rate":    func(v float64) string { return fmt.Sprintf("%.1f/min", v) },
		"seconds": func(v float64) string { return fmt.Sprintf("%.2f s", v/1000) },
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}

	s := &Server{store: st, log: log, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleOverview)
	mux.HandleFunc("GET /salute", s.handleHealth)

	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
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

type overviewData struct {
	Version  string
	Minuti   int
	Summary  store.Summary
	Txns     []store.TxnStat
	Generato string
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	minuti := 60
	if v := r.URL.Query().Get("minuti"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60*24*30 {
			minuti = n
		}
	}
	since := time.Now().Add(-time.Duration(minuti) * time.Minute).Unix()

	summary, err := s.store.Summary(since)
	if err != nil {
		s.fail(w, "riepilogo", err)
		return
	}
	txns, err := s.store.TopTransactions(since, 50)
	if err != nil {
		s.fail(w, "classifica delle transazioni", err)
		return
	}

	data := overviewData{
		Version:  version.Version,
		Minuti:   minuti,
		Summary:  summary,
		Txns:     txns,
		Generato: time.Now().Format("15:04:05"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "panoramica.html", data); err != nil {
		s.log.Error("rendering della panoramica fallito", "errore", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("query fallita", "cosa", what, "errore", err)
	http.Error(w, "errore nel recupero dei dati: "+what, http.StatusInternalServerError)
}
