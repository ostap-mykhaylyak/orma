package store

import (
	"fmt"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/metrics"
)

// Punto e' un intervallo della serie temporale.
type Punto struct {
	TS     int64
	Count  uint64
	Errors uint64
	AvgMS  float64
	P95MS  float64
}

// Vuoto indica un intervallo senza traffico. Serve alla UI per distinguere
// «zero richieste» da «nessun dato», che disegnati allo stesso modo
// racconterebbero due storie diverse con la stessa linea.
func (p Punto) Vuoto() bool {
	return p.Count == 0
}

// Serie restituisce l'andamento nel tempo, un punto per bucket, con i buchi
// riempiti a zero. txn vuoto significa tutte le transazioni.
//
// Restituisce anche il passo in secondi, perche' cambia con l'intervallo
// chiesto e la UI deve sapere quanto e' largo un punto.
func (s *Store) Serie(since int64, txn string) ([]Punto, int64, error) {
	tabella := s.tabellaPer(since, s.retention)

	var passo int64
	switch tabella {
	case Tabella5m:
		passo = passo5m
	case Tabella1h:
		passo = passo1h
	default:
		passo = 60
	}

	query := fmt.Sprintf(
		`SELECT bucket_ts, count, errors, sum_ns, histogram
		   FROM %s WHERE bucket_ts >= ? AND category = ?`, tabella)
	args := []any{since, CategoriaTotale}
	if txn != "" {
		query += ` AND txn_name = ?`
		args = append(args, txn)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, passo, err
	}
	defer rows.Close()

	type accumulo struct {
		count, errs uint64
		sumNS       uint64
		hist        metrics.Histogram
	}
	per := make(map[int64]*accumulo)

	for rows.Next() {
		var ts, count, errs, sumNS int64
		var raw []byte
		if err := rows.Scan(&ts, &count, &errs, &sumNS, &raw); err != nil {
			return nil, passo, err
		}
		a := per[ts]
		if a == nil {
			a = &accumulo{}
			per[ts] = a
		}
		a.count += uint64(count)
		a.errs += uint64(errs)
		a.sumNS += uint64(sumNS)
		h := metrics.Decode(raw)
		a.hist.Merge(&h)
	}
	if err := rows.Err(); err != nil {
		return nil, passo, err
	}

	inizio := since - since%passo
	fine := time.Now().Unix()
	fine = fine - fine%passo

	// Un intervallo molto lungo su granularita' fine produrrebbe decine di
	// migliaia di punti, che nessuno schermo distingue: si taglia.
	const maxPunti = 720
	if (fine-inizio)/passo > maxPunti {
		inizio = fine - passo*maxPunti
	}

	var out []Punto
	for ts := inizio; ts <= fine; ts += passo {
		p := Punto{TS: ts}
		if a := per[ts]; a != nil {
			p.Count = a.count
			p.Errors = a.errs
			if a.count > 0 {
				p.AvgMS = float64(a.sumNS) / 1e6 / float64(a.count)
			}
			p.P95MS = a.hist.Percentile(0.95)
		}
		out = append(out, p)
	}
	return out, passo, nil
}

// DettaglioTxn e' il riepilogo di una singola transazione.
type DettaglioTxn struct {
	Nome    string
	Kind    string
	Count   uint64
	Errors  uint64
	TotalMS float64
	AvgMS   float64
	P50MS   float64
	P95MS   float64
	P99MS   float64
	MaxMS   float64
	DBMS    float64
	ExtMS   float64
}

// PHPMS e' il tempo che non e' ne' database ne' rete.
func (d DettaglioTxn) PHPMS() float64 {
	v := d.TotalMS - d.DBMS - d.ExtMS
	if v < 0 {
		return 0
	}
	return v
}

// ErrorRate e' la percentuale di richieste fallite.
func (d DettaglioTxn) ErrorRate() float64 {
	if d.Count == 0 {
		return 0
	}
	return float64(d.Errors) / float64(d.Count) * 100
}

// Dettaglio raccoglie i numeri di una transazione, con la scomposizione per
// categoria e i percentili sull'istogramma unito.
func (s *Store) Dettaglio(since int64, txn string) (DettaglioTxn, error) {
	tabella := s.tabellaPer(since, s.retention)
	out := DettaglioTxn{Nome: txn}

	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT kind, category, count, errors, sum_ns, max_ns, histogram
		   FROM %s WHERE bucket_ts >= ? AND txn_name = ?`, tabella), since, txn)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var totale metrics.Histogram
	var maxNS uint64

	for rows.Next() {
		var kind, categoria string
		var count, errs, sumNS, rigaMax int64
		var raw []byte
		if err := rows.Scan(&kind, &categoria, &count, &errs, &sumNS, &rigaMax, &raw); err != nil {
			return out, err
		}

		switch categoria {
		case CategoriaTotale:
			out.Kind = kind
			out.Count += uint64(count)
			out.Errors += uint64(errs)
			out.TotalMS += float64(sumNS) / 1e6
			if uint64(rigaMax) > maxNS {
				maxNS = uint64(rigaMax)
			}
			h := metrics.Decode(raw)
			totale.Merge(&h)
		case CategoriaDatabase:
			out.DBMS += float64(sumNS) / 1e6
		case CategoriaEsterne:
			out.ExtMS += float64(sumNS) / 1e6
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	out.MaxMS = float64(maxNS) / 1e6
	if out.Count > 0 {
		out.AvgMS = out.TotalMS / float64(out.Count)
	}
	out.P50MS = totale.Percentile(0.50)
	out.P95MS = totale.Percentile(0.95)
	out.P99MS = totale.Percentile(0.99)
	return out, nil
}

// TracceDi elenca i trace conservati per una transazione.
func (s *Store) TracceDi(since int64, txn string, limit int) ([]TraceRow, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, txn_name, kind, duration_ns, http_status, has_error,
		        json_array_length(spans)
		   FROM traces
		  WHERE ts >= ? AND txn_name = ?
		  ORDER BY duration_ns DESC
		  LIMIT ?`, since, txn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TraceRow
	for rows.Next() {
		var r TraceRow
		var durNS int64
		var hasError, status int
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
