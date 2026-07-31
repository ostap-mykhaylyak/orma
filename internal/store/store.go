// Package store persiste le metriche su SQLite.
//
// Il driver e' quello WASM di ncruces: non richiede CGO, cosi' il binario
// resta compilabile in modo incrociato e senza toolchain C.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/ostap-mykhaylyak/orma/internal/metrics"
)

// Key identifica una serie di metriche.
type Key struct {
	App      string
	Txn      string
	Category string
}

// Bucket e' l'accumulo di una serie dentro una finestra.
//
// La somma dei quadrati e' in millisecondi al quadrato, non in nanosecondi:
// in nanosecondi un solo campione da un secondo vale 1e18, e pochi campioni
// basterebbero a saturare un intero a 64 bit.
type Bucket struct {
	Count   uint64
	Errors  uint64
	SumNS   uint64
	SumSqMS float64
	MinNS   uint64
	MaxNS   uint64
	Hist    metrics.Histogram
}

// Store e' l'accesso al database.
type Store struct {
	db *sql.DB

	mu     sync.Mutex
	appIDs map[string]int64
}

const schema = `
CREATE TABLE IF NOT EXISTS apps (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE,
	apdex_t    REAL    NOT NULL DEFAULT 0.5,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS metrics_1m (
	app_id    INTEGER NOT NULL,
	bucket_ts INTEGER NOT NULL,
	txn_name  TEXT    NOT NULL,
	category  TEXT    NOT NULL,
	count     INTEGER NOT NULL,
	errors    INTEGER NOT NULL,
	sum_ns    INTEGER NOT NULL,
	sumsq_ms  REAL    NOT NULL,
	min_ns    INTEGER NOT NULL,
	max_ns    INTEGER NOT NULL,
	histogram BLOB    NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, txn_name, category)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS metrics_1m_ts ON metrics_1m (bucket_ts);
`

// Open apre il database, creando file e schema se mancano.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creazione della directory del database: %w", err)
	}

	dsn := "file:" + path + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=synchronous(normal)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("apertura di %s: %w", path, err)
	}

	// Una sola connessione in scrittura evita di dover gestire "database is
	// locked" su un carico che comunque scrive una volta al minuto.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creazione dello schema: %w", err)
	}

	return &Store{db: db, appIDs: make(map[string]int64)}, nil
}

// Close chiude il database.
func (s *Store) Close() error {
	return s.db.Close()
}

// appID restituisce l'identificativo dell'applicazione, creandola se serve.
func (s *Store) appID(name string) (int64, error) {
	s.mu.Lock()
	if id, ok := s.appIDs[name]; ok {
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()

	if _, err := s.db.Exec(
		`INSERT INTO apps (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`,
		name, time.Now().Unix()); err != nil {
		return 0, err
	}

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, err
	}

	s.mu.Lock()
	s.appIDs[name] = id
	s.mu.Unlock()
	return id, nil
}

// WriteMetrics riversa una finestra. Se la riga esiste gia' i valori vengono
// fusi: puo' succedere se il daemon riparte mentre la finestra e' aperta.
func (s *Store) WriteMetrics(window int64, buckets map[Key]*Bucket) error {
	// Gli identificativi delle applicazioni si risolvono prima di aprire la
	// transazione. Il pool ha una sola connessione: chiederne un'altra mentre
	// la transazione tiene occupata l'unica disponibile sarebbe un blocco
	// senza uscita.
	ids := make(map[string]int64)
	for key := range buckets {
		if _, ok := ids[key.App]; ok {
			continue
		}
		id, err := s.appID(key.App)
		if err != nil {
			return fmt.Errorf("applicazione %q: %w", key.App, err)
		}
		ids[key.App] = id
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, b := range buckets {
		appID := ids[key.App]
		merged := *b

		var (
			exCount, exErrors, exSum, exMin, exMax int64
			exSumSq                                float64
			exHist                                 []byte
		)
		row := tx.QueryRow(
			`SELECT count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram
			   FROM metrics_1m
			  WHERE app_id = ? AND bucket_ts = ? AND txn_name = ? AND category = ?`,
			appID, window, key.Txn, key.Category)

		switch err := row.Scan(&exCount, &exErrors, &exSum, &exSumSq, &exMin, &exMax, &exHist); err {
		case nil:
			merged.Count += uint64(exCount)
			merged.Errors += uint64(exErrors)
			merged.SumNS += uint64(exSum)
			merged.SumSqMS += exSumSq
			if uint64(exMin) < merged.MinNS {
				merged.MinNS = uint64(exMin)
			}
			if uint64(exMax) > merged.MaxNS {
				merged.MaxNS = uint64(exMax)
			}
			existing := metrics.Decode(exHist)
			merged.Hist.Merge(&existing)
		case sql.ErrNoRows:
		default:
			return err
		}

		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO metrics_1m
			 (app_id, bucket_ts, txn_name, category, count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appID, window, key.Txn, key.Category,
			merged.Count, merged.Errors, merged.SumNS, merged.SumSqMS,
			merged.MinNS, merged.MaxNS, merged.Hist.Encode()); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Summary e' il riepilogo mostrato nella Panoramica.
type Summary struct {
	Requests   uint64
	Errors     uint64
	TotalMS    float64
	ThroughputPerMin float64
	P50MS      float64
	P95MS      float64
	P99MS      float64
	Apps       []string
}

// ErrorRate e' la percentuale di richieste in errore.
func (s Summary) ErrorRate() float64 {
	if s.Requests == 0 {
		return 0
	}
	return float64(s.Errors) / float64(s.Requests) * 100
}

// AvgMS e' la durata media.
func (s Summary) AvgMS() float64 {
	if s.Requests == 0 {
		return 0
	}
	return s.TotalMS / float64(s.Requests)
}

// Summary calcola il riepilogo sulle finestre a partire da since.
func (s *Store) Summary(since int64) (Summary, error) {
	var out Summary

	rows, err := s.db.Query(
		`SELECT m.count, m.errors, m.sum_ns, m.histogram, a.name
		   FROM metrics_1m m JOIN apps a ON a.id = m.app_id
		  WHERE m.bucket_ts >= ?`, since)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var total metrics.Histogram
	apps := make(map[string]struct{})

	for rows.Next() {
		var count, errCount, sumNS int64
		var hist []byte
		var app string
		if err := rows.Scan(&count, &errCount, &sumNS, &hist, &app); err != nil {
			return out, err
		}
		out.Requests += uint64(count)
		out.Errors += uint64(errCount)
		out.TotalMS += float64(sumNS) / 1e6
		h := metrics.Decode(hist)
		total.Merge(&h)
		apps[app] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	out.P50MS = total.Percentile(0.50)
	out.P95MS = total.Percentile(0.95)
	out.P99MS = total.Percentile(0.99)

	elapsedMin := float64(time.Now().Unix()-since) / 60
	if elapsedMin > 0 {
		out.ThroughputPerMin = float64(out.Requests) / elapsedMin
	}

	for app := range apps {
		out.Apps = append(out.Apps, app)
	}
	return out, nil
}

// TxnStat e' una riga della classifica delle transazioni.
type TxnStat struct {
	App      string
	Name     string
	Category string
	Count    uint64
	Errors   uint64
	TotalMS  float64
	AvgMS    float64
	P95MS    float64
	MaxMS    float64
}

// TopTransactions restituisce le transazioni ordinate per tempo totale
// consumato, non per durata media: una pagina da 300 ms chiamata diecimila
// volte pesa piu' di una da 3 secondi chiamata due volte.
func (s *Store) TopTransactions(since int64, limit int) ([]TxnStat, error) {
	rows, err := s.db.Query(
		`SELECT a.name, m.txn_name, m.category,
		        SUM(m.count), SUM(m.errors), SUM(m.sum_ns), MAX(m.max_ns)
		   FROM metrics_1m m JOIN apps a ON a.id = m.app_id
		  WHERE m.bucket_ts >= ?
		  GROUP BY a.name, m.txn_name, m.category
		  ORDER BY SUM(m.sum_ns) DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TxnStat
	for rows.Next() {
		var st TxnStat
		var count, errCount, sumNS, maxNS int64
		if err := rows.Scan(&st.App, &st.Name, &st.Category, &count, &errCount, &sumNS, &maxNS); err != nil {
			return nil, err
		}
		st.Count = uint64(count)
		st.Errors = uint64(errCount)
		st.TotalMS = float64(sumNS) / 1e6
		st.MaxMS = float64(maxNS) / 1e6
		if count > 0 {
			st.AvgMS = st.TotalMS / float64(count)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// I percentili richiedono la fusione degli istogrammi, che SQL non sa
	// fare: si recuperano a parte, una query per riga della classifica.
	for i := range out {
		p95, err := s.percentile(since, out[i].App, out[i].Name, out[i].Category, 0.95)
		if err != nil {
			return nil, err
		}
		out[i].P95MS = p95
	}

	return out, nil
}

func (s *Store) percentile(since int64, app, txn, category string, p float64) (float64, error) {
	rows, err := s.db.Query(
		`SELECT m.histogram
		   FROM metrics_1m m JOIN apps a ON a.id = m.app_id
		  WHERE m.bucket_ts >= ? AND a.name = ? AND m.txn_name = ? AND m.category = ?`,
		since, app, txn, category)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total metrics.Histogram
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		h := metrics.Decode(raw)
		total.Merge(&h)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return total.Percentile(p), nil
}
