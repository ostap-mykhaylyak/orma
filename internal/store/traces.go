package store

import (
	"encoding/json"
	"fmt"
)

// TraceSpan e' uno span dentro un trace salvato. I nomi dei campi JSON sono
// corti perche' il payload viene scritto una volta per trace conservato.
//
// L'inizio e' relativo all'inizio della transazione: cosi' il waterfall si
// disegna senza sottrazioni, e il payload non dipende dall'ora assoluta.
type TraceSpan struct {
	ID       string            `json:"i"`
	Parent   string            `json:"p,omitempty"`
	Name     string            `json:"n"`
	Kind     uint8             `json:"k"`
	OffsetNS uint64            `json:"o"`
	DurNS    uint64            `json:"d"`
	Status   uint8             `json:"s,omitempty"`
	Attrs    map[string]string `json:"a,omitempty"`
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
	Spans      []TraceSpan
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
}

// DurMS e' la durata dello span in millisecondi.
func (r Riga) DurMS() float64 {
	return float64(r.Span.DurNS) / 1e6
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

// Waterfall ordina gli span ad albero e calcola le proporzioni delle barre.
//
// Uno span il cui genitore non e' presente viene appeso alla radice invece di
// sparire: puo' succedere se il tetto degli span ha troncato il genitore.
func (t Trace) Waterfall() []Riga {
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

	var out []Riga
	var visit func(span TraceSpan, depth int)
	visit = func(span TraceSpan, depth int) {
		width := float64(span.DurNS) / total * 100
		if width < 0.4 {
			width = 0.4 // altrimenti gli span brevissimi diventano invisibili
		}
		if width > 100 {
			width = 100
		}
		offset := float64(span.OffsetNS) / total * 100
		if offset+width > 100 {
			offset = 100 - width
		}
		if offset < 0 {
			offset = 0
		}

		out = append(out, Riga{Span: span, Depth: depth, OffsetPct: offset, WidthPct: width})

		kids := children[span.ID]
		for i := 0; i < len(kids); i++ {
			for j := i + 1; j < len(kids); j++ {
				if kids[j].OffsetNS < kids[i].OffsetNS {
					kids[i], kids[j] = kids[j], kids[i]
				}
			}
		}
		for _, kid := range kids {
			visit(kid, depth+1)
		}
	}

	for _, root := range roots {
		visit(root, 0)
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
	-- TEXT e non BLOB: le funzioni json_* di SQLite lavorano sul testo.
	spans       TEXT    NOT NULL
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

	err := s.db.QueryRow(
		`SELECT t.id, a.name, t.txn_name, t.kind, t.ts, t.duration_ns,
		        t.http_status, t.has_error, t.spans
		   FROM traces t JOIN apps a ON a.id = t.app_id
		  WHERE t.id = ?`, id).
		Scan(&t.ID, &t.App, &t.Name, &t.Kind, &t.TS, &durNS, &status, &hasError, &payload)
	if err != nil {
		return nil, err
	}

	t.DurationNS = uint64(durNS)
	t.HTTPStatus = uint16(status)
	t.HasError = hasError != 0

	if err := json.Unmarshal(payload, &t.Spans); err != nil {
		return nil, fmt.Errorf("payload del trace %d illeggibile: %w", id, err)
	}
	return &t, nil
}

func writeTraces(exec func(string, ...any) error, appIDs map[string]int64, traces []*Trace) error {
	for _, t := range traces {
		payload, err := json.Marshal(t.Spans)
		if err != nil {
			return err
		}
		hasError := 0
		if t.HasError {
			hasError = 1
		}
		if err := exec(
			`INSERT INTO traces (app_id, ts, txn_name, kind, duration_ns, http_status, has_error, spans)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			appIDs[t.App], t.TS, t.Name, t.Kind, int64(t.DurationNS),
			int(t.HTTPStatus), hasError, string(payload)); err != nil {
			return err
		}
	}
	return nil
}
