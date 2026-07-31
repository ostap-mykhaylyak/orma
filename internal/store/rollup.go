package store

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/metrics"
)

// Granularita' disponibili. Le tabelle hanno la stessa forma e differiscono
// solo per l'ampiezza del bucket.
const (
	Tabella1m = "metrics_1m"
	Tabella5m = "metrics_5m"
	Tabella1h = "metrics_1h"
)

const (
	passo5m int64 = 300
	passo1h int64 = 3600
)

const rollupSchema = `
CREATE TABLE IF NOT EXISTS metrics_5m (
	app_id    INTEGER NOT NULL,
	bucket_ts INTEGER NOT NULL,
	txn_name  TEXT    NOT NULL,
	kind      TEXT    NOT NULL,
	category  TEXT    NOT NULL,
	count     INTEGER NOT NULL,
	errors    INTEGER NOT NULL,
	sum_ns    INTEGER NOT NULL,
	sumsq_ms  REAL    NOT NULL,
	min_ns    INTEGER NOT NULL,
	max_ns    INTEGER NOT NULL,
	histogram BLOB    NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, txn_name, kind, category)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS metrics_5m_ts ON metrics_5m (bucket_ts);

CREATE TABLE IF NOT EXISTS metrics_1h (
	app_id    INTEGER NOT NULL,
	bucket_ts INTEGER NOT NULL,
	txn_name  TEXT    NOT NULL,
	kind      TEXT    NOT NULL,
	category  TEXT    NOT NULL,
	count     INTEGER NOT NULL,
	errors    INTEGER NOT NULL,
	sum_ns    INTEGER NOT NULL,
	sumsq_ms  REAL    NOT NULL,
	min_ns    INTEGER NOT NULL,
	max_ns    INTEGER NOT NULL,
	histogram BLOB    NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, txn_name, kind, category)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS metrics_1h_ts ON metrics_1h (bucket_ts);
`

// Retention descrive quanto si conserva ogni granularita'.
type Retention struct {
	Minuto  time.Duration
	Cinque  time.Duration
	Ora     time.Duration
	Tracce  time.Duration
	Errori  time.Duration
	QuerySQ time.Duration
}

// chiaveRollup identifica una serie nel bucket di destinazione.
type chiaveRollup struct {
	AppID  int64
	Bucket int64
	Txn    string
	Kind   string
	Cat    string
}

// Rollup ricalcola gli ultimi bucket completi della granularita' piu' grossa a
// partire da quella piu' fine.
//
// Il ricalcolo e' idempotente e limitato agli ultimi N bucket, invece di
// tenere un segnalibro di avanzamento: se un giro salta, il successivo rimedia
// da solo, e non c'e' uno stato da riparare a mano quando qualcosa va storto.
func (s *Store) Rollup(log *slog.Logger) error {
	now := time.Now().Unix()

	if err := s.rollupTra(Tabella1m, Tabella5m, passo5m, now, 3); err != nil {
		return fmt.Errorf("rollup 1m verso 5m: %w", err)
	}
	if err := s.rollupTra(Tabella5m, Tabella1h, passo1h, now, 2); err != nil {
		return fmt.Errorf("rollup 5m verso 1h: %w", err)
	}
	if log != nil {
		log.Debug("rollup completato")
	}
	return nil
}

func (s *Store) rollupTra(src, dst string, passo, now int64, bucket int) error {
	// Solo i bucket gia' chiusi: quello in corso cambierebbe ancora.
	fine := now - now%passo
	inizio := fine - passo*int64(bucket)

	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT app_id, bucket_ts, txn_name, kind, category,
		        count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram
		   FROM %s WHERE bucket_ts >= ? AND bucket_ts < ?`, src), inizio, fine)
	if err != nil {
		return err
	}

	unito := make(map[chiaveRollup]*Bucket)
	for rows.Next() {
		var (
			appID, ts, count, errCount, sumNS, minNS, maxNS int64
			txn, kind, cat                                  string
			sumSq                                           float64
			hist                                            []byte
		)
		if err := rows.Scan(&appID, &ts, &txn, &kind, &cat,
			&count, &errCount, &sumNS, &sumSq, &minNS, &maxNS, &hist); err != nil {
			rows.Close()
			return err
		}

		key := chiaveRollup{AppID: appID, Bucket: ts - ts%passo, Txn: txn, Kind: kind, Cat: cat}
		b := unito[key]
		if b == nil {
			b = &Bucket{MinNS: uint64(minNS)}
			unito[key] = b
		}

		b.Count += uint64(count)
		b.Errors += uint64(errCount)
		b.SumNS += uint64(sumNS)
		b.SumSqMS += sumSq
		if uint64(minNS) < b.MinNS {
			b.MinNS = uint64(minNS)
		}
		if uint64(maxNS) > b.MaxNS {
			b.MaxNS = uint64(maxNS)
		}
		h := metrics.Decode(hist)
		b.Hist.Merge(&h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(unito) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, b := range unito {
		if _, err := tx.Exec(fmt.Sprintf(
			`INSERT OR REPLACE INTO %s
			 (app_id, bucket_ts, txn_name, kind, category, count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, dst),
			key.AppID, key.Bucket, key.Txn, key.Kind, key.Cat,
			b.Count, b.Errors, b.SumNS, b.SumSqMS, b.MinNS, b.MaxNS, b.Hist.Encode()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Purge elimina i dati oltre la loro conservazione.
func (s *Store) Purge(r Retention, log *slog.Logger) error {
	now := time.Now()

	tagli := []struct {
		tabella string
		colonna string
		durata  time.Duration
	}{
		{Tabella1m, "bucket_ts", r.Minuto},
		{Tabella5m, "bucket_ts", r.Cinque},
		{Tabella1h, "bucket_ts", r.Ora},
		{"traces", "ts", r.Tracce},
		{"errors", "bucket_ts", r.Errori},
		{"slow_sql", "bucket_ts", r.QuerySQ},
		{"externals", "bucket_ts", r.QuerySQ},
	}

	var totale int64
	for _, t := range tagli {
		if t.durata <= 0 {
			continue
		}
		limite := now.Add(-t.durata).Unix()
		res, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, t.tabella, t.colonna), limite)
		if err != nil {
			return fmt.Errorf("purga di %s: %w", t.tabella, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			totale += n
		}
	}

	if totale > 0 && log != nil {
		log.Info("dati scaduti rimossi", "righe", totale)
	}
	return nil
}

// tabellaPer sceglie la granularita' adatta all'intervallo richiesto: piu' si
// guarda indietro, piu' grosso e' il bucket, perche' quello fine e' gia' stato
// purgato.
func (s *Store) tabellaPer(since int64, r Retention) string {
	eta := time.Since(time.Unix(since, 0))

	switch {
	case eta <= r.Minuto:
		return Tabella1m
	case eta <= r.Cinque:
		return Tabella5m
	default:
		return Tabella1h
	}
}
