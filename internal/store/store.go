// Package store persiste le metriche su SQLite.
//
// Il driver e' quello WASM di ncruces: non richiede CGO, cosi' il binario
// resta compilabile in modo incrociato e senza toolchain C.
package store

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/ostap-mykhaylyak/orma/internal/metrics"
)

// schemaVersion cambia a ogni modifica incompatibile delle tabelle. Prima
// della 1.0 le tabelle vengono ricreate invece di essere migrate: i dati di
// telemetria sono rimpiazzabili, il codice di migrazione no.
const schemaVersion = 3

// Categorie di metrica.
const (
	CategoriaTotale   = "totale"
	CategoriaDatabase = "database"
	CategoriaEsterne  = "esterne"
)

// Key identifica una serie di metriche.
type Key struct {
	App      string
	Txn      string
	Kind     string // web oppure background
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

// SQLKey identifica una query gia' offuscata dall'estensione.
type SQLKey struct {
	App       string
	Statement string
}

// HostKey identifica un host contattato.
type HostKey struct {
	App  string
	Host string
}

// Simple e' un accumulo senza istogramma, per query e host.
type Simple struct {
	Count  uint64
	Errors uint64
	SumNS  uint64
	MaxNS  uint64
}

// Add contabilizza un campione.
func (s *Simple) Add(durationNS uint64, failed bool) {
	s.Count++
	if failed {
		s.Errors++
	}
	s.SumNS += durationNS
	if durationNS > s.MaxNS {
		s.MaxNS = durationNS
	}
}

// Window e' tutto cio' che si accumula in un minuto.
type Window struct {
	Metrics map[Key]*Bucket
	SQL     map[SQLKey]*Simple
	Hosts   map[HostKey]*Simple
	Traces  []*Trace
}

// NewWindow costruisce una finestra vuota.
func NewWindow() *Window {
	return &Window{
		Metrics: make(map[Key]*Bucket),
		SQL:     make(map[SQLKey]*Simple),
		Hosts:   make(map[HostKey]*Simple),
	}
}

// Empty indica se non c'e' nulla da scrivere.
func (w *Window) Empty() bool {
	return len(w.Metrics) == 0 && len(w.SQL) == 0 && len(w.Hosts) == 0 && len(w.Traces) == 0
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

CREATE INDEX IF NOT EXISTS metrics_1m_ts ON metrics_1m (bucket_ts);

CREATE TABLE IF NOT EXISTS slow_sql (
	app_id     INTEGER NOT NULL,
	bucket_ts  INTEGER NOT NULL,
	stmt_hash  TEXT    NOT NULL,
	statement  TEXT    NOT NULL,
	count      INTEGER NOT NULL,
	errors     INTEGER NOT NULL,
	sum_ns     INTEGER NOT NULL,
	max_ns     INTEGER NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, stmt_hash)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS slow_sql_ts ON slow_sql (bucket_ts);

CREATE TABLE IF NOT EXISTS externals (
	app_id    INTEGER NOT NULL,
	bucket_ts INTEGER NOT NULL,
	host      TEXT    NOT NULL,
	count     INTEGER NOT NULL,
	errors    INTEGER NOT NULL,
	sum_ns    INTEGER NOT NULL,
	max_ns    INTEGER NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, host)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS externals_ts ON externals (bucket_ts);
`

// Open apre il database, creando file e schema se mancano.
func Open(path string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creazione della directory del database: %w", err)
	}

	dsn := "file:" + path + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=synchronous(normal)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("apertura di %s: %w", path, err)
	}

	// Una sola connessione evita di dover gestire "database is locked" su un
	// carico che scrive una volta al minuto.
	db.SetMaxOpenConns(1)

	if err := migrate(db, log); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, appIDs: make(map[string]int64)}, nil
}

// migrate applica lo schema. Prima della 1.0 un cambio di versione ricrea le
// tabelle delle metriche invece di migrarle.
func migrate(db *sql.DB, log *slog.Logger) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("lettura di user_version: %w", err)
	}

	if current != 0 && current != schemaVersion {
		if log != nil {
			log.Warn("schema del database obsoleto, le metriche esistenti vengono scartate",
				"trovata", current, "attesa", schemaVersion)
		}
		for _, t := range []string{"metrics_1m", "slow_sql", "externals", "traces"} {
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				return fmt.Errorf("rimozione di %s: %w", t, err)
			}
		}
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creazione dello schema: %w", err)
	}
	if _, err := db.Exec(traceSchema); err != nil {
		return fmt.Errorf("creazione dello schema dei trace: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("scrittura di user_version: %w", err)
	}
	return nil
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

// StatementHash e' la chiave con cui si aggregano le query.
func StatementHash(statement string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(statement))
	return fmt.Sprintf("%016x", h.Sum64())
}

// WriteWindow riversa una finestra. Le righe esistenti vengono fuse: puo'
// succedere se il daemon riparte mentre la finestra e' ancora aperta.
func (s *Store) WriteWindow(window int64, w *Window) error {
	// Gli identificativi delle applicazioni si risolvono prima di aprire la
	// transazione. Il pool ha una sola connessione: chiederne un'altra mentre
	// la transazione tiene occupata l'unica disponibile sarebbe un blocco
	// senza uscita.
	ids := make(map[string]int64)
	resolve := func(app string) error {
		if _, ok := ids[app]; ok {
			return nil
		}
		id, err := s.appID(app)
		if err != nil {
			return fmt.Errorf("applicazione %q: %w", app, err)
		}
		ids[app] = id
		return nil
	}

	for key := range w.Metrics {
		if err := resolve(key.App); err != nil {
			return err
		}
	}
	for key := range w.SQL {
		if err := resolve(key.App); err != nil {
			return err
		}
	}
	for key := range w.Hosts {
		if err := resolve(key.App); err != nil {
			return err
		}
	}
	for _, t := range w.Traces {
		if err := resolve(t.App); err != nil {
			return err
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, b := range w.Metrics {
		if err := writeMetric(tx, ids[key.App], window, key, b); err != nil {
			return err
		}
	}
	for key, v := range w.SQL {
		if err := writeSimple(tx,
			`SELECT count, errors, sum_ns, max_ns FROM slow_sql
			  WHERE app_id = ? AND bucket_ts = ? AND stmt_hash = ?`,
			`INSERT OR REPLACE INTO slow_sql
			 (app_id, bucket_ts, stmt_hash, statement, count, errors, sum_ns, max_ns)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ids[key.App], window, StatementHash(key.Statement), key.Statement, v); err != nil {
			return err
		}
	}
	for key, v := range w.Hosts {
		if err := writeSimple(tx,
			`SELECT count, errors, sum_ns, max_ns FROM externals
			  WHERE app_id = ? AND bucket_ts = ? AND host = ?`,
			`INSERT OR REPLACE INTO externals
			 (app_id, bucket_ts, host, count, errors, sum_ns, max_ns)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ids[key.App], window, key.Host, "", v); err != nil {
			return err
		}
	}

	if err := writeTraces(func(q string, args ...any) error {
		_, err := tx.Exec(q, args...)
		return err
	}, ids, w.Traces); err != nil {
		return err
	}

	return tx.Commit()
}

func writeMetric(tx *sql.Tx, appID, window int64, key Key, b *Bucket) error {
	merged := *b

	var (
		exCount, exErrors, exSum, exMin, exMax int64
		exSumSq                                float64
		exHist                                 []byte
	)
	row := tx.QueryRow(
		`SELECT count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram
		   FROM metrics_1m
		  WHERE app_id = ? AND bucket_ts = ? AND txn_name = ? AND kind = ? AND category = ?`,
		appID, window, key.Txn, key.Kind, key.Category)

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

	_, err := tx.Exec(
		`INSERT OR REPLACE INTO metrics_1m
		 (app_id, bucket_ts, txn_name, kind, category, count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, window, key.Txn, key.Kind, key.Category,
		merged.Count, merged.Errors, merged.SumNS, merged.SumSqMS,
		merged.MinNS, merged.MaxNS, merged.Hist.Encode())
	return err
}

// writeSimple fonde e riscrive una riga senza istogramma. label e' la colonna
// descrittiva (lo statement) quando c'e', altrimenti stringa vuota.
func writeSimple(tx *sql.Tx, selectSQL, insertSQL string, appID, window int64,
	key, label string, v *Simple) error {

	merged := *v

	var exCount, exErrors, exSum, exMax int64
	row := tx.QueryRow(selectSQL, appID, window, key)

	switch err := row.Scan(&exCount, &exErrors, &exSum, &exMax); err {
	case nil:
		merged.Count += uint64(exCount)
		merged.Errors += uint64(exErrors)
		merged.SumNS += uint64(exSum)
		if uint64(exMax) > merged.MaxNS {
			merged.MaxNS = uint64(exMax)
		}
	case sql.ErrNoRows:
	default:
		return err
	}

	var err error
	if label != "" {
		_, err = tx.Exec(insertSQL, appID, window, key, label,
			merged.Count, merged.Errors, merged.SumNS, merged.MaxNS)
	} else {
		_, err = tx.Exec(insertSQL, appID, window, key,
			merged.Count, merged.Errors, merged.SumNS, merged.MaxNS)
	}
	return err
}

// Summary e' il riepilogo mostrato nella Panoramica.
type Summary struct {
	Requests         uint64
	Errors           uint64
	TotalMS          float64
	ThroughputPerMin float64
	P50MS            float64
	P95MS            float64
	P99MS            float64
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
		`SELECT count, errors, sum_ns, histogram
		   FROM metrics_1m
		  WHERE bucket_ts >= ? AND category = ?`, since, CategoriaTotale)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var total metrics.Histogram
	for rows.Next() {
		var count, errCount, sumNS int64
		var hist []byte
		if err := rows.Scan(&count, &errCount, &sumNS, &hist); err != nil {
			return out, err
		}
		out.Requests += uint64(count)
		out.Errors += uint64(errCount)
		out.TotalMS += float64(sumNS) / 1e6
		h := metrics.Decode(hist)
		total.Merge(&h)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	out.P50MS = total.Percentile(0.50)
	out.P95MS = total.Percentile(0.95)
	out.P99MS = total.Percentile(0.99)

	if elapsedMin := float64(time.Now().Unix()-since) / 60; elapsedMin > 0 {
		out.ThroughputPerMin = float64(out.Requests) / elapsedMin
	}
	return out, nil
}

// TxnStat e' una riga della classifica delle transazioni.
type TxnStat struct {
	App     string
	Name    string
	Kind    string
	Count   uint64
	Errors  uint64
	TotalMS float64
	AvgMS   float64
	P95MS   float64
	MaxMS   float64
	DBMS    float64
	ExtMS   float64
}

// PHPMS e' il tempo che non e' ne' database ne' rete: sta nel PHP.
func (t TxnStat) PHPMS() float64 {
	v := t.TotalMS - t.DBMS - t.ExtMS
	if v < 0 {
		return 0
	}
	return v
}

// TopTransactions restituisce le transazioni ordinate per tempo totale
// consumato, non per durata media: una pagina da 300 ms chiamata diecimila
// volte pesa piu' di una da 3 secondi chiamata due volte.
func (s *Store) TopTransactions(since int64, limit int) ([]TxnStat, error) {
	rows, err := s.db.Query(
		`SELECT a.name, m.txn_name, m.kind,
		        SUM(m.count), SUM(m.errors), SUM(m.sum_ns), MAX(m.max_ns)
		   FROM metrics_1m m JOIN apps a ON a.id = m.app_id
		  WHERE m.bucket_ts >= ? AND m.category = ?
		  GROUP BY a.name, m.txn_name, m.kind
		  ORDER BY SUM(m.sum_ns) DESC
		  LIMIT ?`, since, CategoriaTotale, limit)
	if err != nil {
		return nil, err
	}

	var out []TxnStat
	for rows.Next() {
		var st TxnStat
		var count, errCount, sumNS, maxNS int64
		if err := rows.Scan(&st.App, &st.Name, &st.Kind, &count, &errCount, &sumNS, &maxNS); err != nil {
			rows.Close()
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
		rows.Close()
		return nil, err
	}
	rows.Close()

	breakdown, err := s.breakdown(since)
	if err != nil {
		return nil, err
	}
	for i := range out {
		k := [2]string{out[i].Name, out[i].Kind}
		out[i].DBMS = breakdown[k][CategoriaDatabase]
		out[i].ExtMS = breakdown[k][CategoriaEsterne]

		p95, err := s.percentile(since, out[i].App, out[i].Name, out[i].Kind, 0.95)
		if err != nil {
			return nil, err
		}
		out[i].P95MS = p95
	}
	return out, nil
}

// breakdown restituisce, per transazione, i millisecondi spesi per categoria.
func (s *Store) breakdown(since int64) (map[[2]string]map[string]float64, error) {
	rows, err := s.db.Query(
		`SELECT txn_name, kind, category, SUM(sum_ns)
		   FROM metrics_1m
		  WHERE bucket_ts >= ? AND category != ?
		  GROUP BY txn_name, kind, category`, since, CategoriaTotale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[[2]string]map[string]float64)
	for rows.Next() {
		var name, kind, category string
		var sumNS int64
		if err := rows.Scan(&name, &kind, &category, &sumNS); err != nil {
			return nil, err
		}
		k := [2]string{name, kind}
		if out[k] == nil {
			out[k] = make(map[string]float64)
		}
		out[k][category] = float64(sumNS) / 1e6
	}
	return out, rows.Err()
}

func (s *Store) percentile(since int64, app, txn, kind string, p float64) (float64, error) {
	rows, err := s.db.Query(
		`SELECT m.histogram
		   FROM metrics_1m m JOIN apps a ON a.id = m.app_id
		  WHERE m.bucket_ts >= ? AND a.name = ? AND m.txn_name = ?
		    AND m.kind = ? AND m.category = ?`,
		since, app, txn, kind, CategoriaTotale)
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

// SQLStat e' una riga della pagina Database.
type SQLStat struct {
	Statement string
	Count     uint64
	Errors    uint64
	TotalMS   float64
	AvgMS     float64
	MaxMS     float64
}

// SlowSQL restituisce le query aggregate per forma, ordinate per tempo totale.
func (s *Store) SlowSQL(since int64, limit int) ([]SQLStat, error) {
	rows, err := s.db.Query(
		`SELECT statement, SUM(count), SUM(errors), SUM(sum_ns), MAX(max_ns)
		   FROM slow_sql
		  WHERE bucket_ts >= ?
		  GROUP BY stmt_hash, statement
		  ORDER BY SUM(sum_ns) DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SQLStat
	for rows.Next() {
		var st SQLStat
		var count, errCount, sumNS, maxNS int64
		if err := rows.Scan(&st.Statement, &count, &errCount, &sumNS, &maxNS); err != nil {
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
	return out, rows.Err()
}

// HostStat e' una riga della pagina Esterne.
type HostStat struct {
	Host    string
	Count   uint64
	Errors  uint64
	TotalMS float64
	AvgMS   float64
	MaxMS   float64
}

// Externals restituisce le chiamate uscenti aggregate per host.
func (s *Store) Externals(since int64, limit int) ([]HostStat, error) {
	rows, err := s.db.Query(
		`SELECT host, SUM(count), SUM(errors), SUM(sum_ns), MAX(max_ns)
		   FROM externals
		  WHERE bucket_ts >= ?
		  GROUP BY host
		  ORDER BY SUM(sum_ns) DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostStat
	for rows.Next() {
		var st HostStat
		var count, errCount, sumNS, maxNS int64
		if err := rows.Scan(&st.Host, &count, &errCount, &sumNS, &maxNS); err != nil {
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
	return out, rows.Err()
}
