package store

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
)

// ErrKey identifica una forma di errore dentro una finestra.
type ErrKey struct {
	App         string
	Fingerprint string
}

// ErrBucket accumula le occorrenze di una forma di errore.
type ErrBucket struct {
	Class    string
	Message  string
	File     string
	Line     uint32
	Txn      string
	Severity uint8
	Count    uint64
}

// Fingerprint raggruppa gli errori per forma. Il messaggio entra solo per i
// primi caratteri: oltre, tende a contenere valori variabili (identificativi,
// percorsi) che moltiplicherebbero i gruppi senza aggiungere informazione.
func Fingerprint(class, message, file string, line uint32) string {
	troncato := message
	if len(troncato) > 120 {
		troncato = troncato[:120]
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(class))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(troncato))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(file))
	_, _ = h.Write([]byte(fmt.Sprintf("|%d", line)))
	return fmt.Sprintf("%016x", h.Sum64())
}

const errorSchema = `
CREATE TABLE IF NOT EXISTS errors (
	app_id      INTEGER NOT NULL,
	bucket_ts   INTEGER NOT NULL,
	fingerprint TEXT    NOT NULL,
	class       TEXT    NOT NULL,
	message     TEXT    NOT NULL,
	file        TEXT    NOT NULL,
	line        INTEGER NOT NULL,
	txn_name    TEXT    NOT NULL,
	severity    INTEGER NOT NULL,
	count       INTEGER NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, fingerprint)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS errors_ts ON errors (bucket_ts);
`

func writeErrors(tx *sql.Tx, appIDs map[string]int64, window int64, errs map[ErrKey]*ErrBucket) error {
	for key, b := range errs {
		var existing int64
		err := tx.QueryRow(
			`SELECT count FROM errors WHERE app_id = ? AND bucket_ts = ? AND fingerprint = ?`,
			appIDs[key.App], window, key.Fingerprint).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO errors
			 (app_id, bucket_ts, fingerprint, class, message, file, line, txn_name, severity, count)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appIDs[key.App], window, key.Fingerprint,
			b.Class, b.Message, b.File, int64(b.Line), b.Txn, int(b.Severity),
			int64(b.Count)+existing); err != nil {
			return err
		}
	}
	return nil
}

// ErrStat e' una riga della pagina Errori.
type ErrStat struct {
	Class    string
	Message  string
	File     string
	Line     uint32
	Txn      string
	Severity uint8
	Count    uint64
}

// Fatale indica se la forma di errore ha fatto fallire le transazioni.
func (e ErrStat) Fatale() bool {
	return e.Severity == 1
}

// Posizione e' il punto nel codice, abbreviato per la tabella.
func (e ErrStat) Posizione() string {
	if e.File == "" {
		return "—"
	}
	file := e.File
	if i := strings.LastIndex(file, "/"); i >= 0 && len(file) > 48 {
		file = "…" + file[i:]
	}
	return fmt.Sprintf("%s:%d", file, e.Line)
}

// Errors restituisce le forme di errore, dalla piu' frequente.
func (s *Store) Errors(since int64, limit int) ([]ErrStat, error) {
	rows, err := s.db.Query(
		`SELECT class, message, file, line, txn_name, MAX(severity), SUM(count)
		   FROM errors
		  WHERE bucket_ts >= ?
		  GROUP BY fingerprint
		  ORDER BY MAX(severity) DESC, SUM(count) DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ErrStat
	for rows.Next() {
		var e ErrStat
		var line, severity, count int64
		if err := rows.Scan(&e.Class, &e.Message, &e.File, &line, &e.Txn, &severity, &count); err != nil {
			return nil, err
		}
		e.Line = uint32(line)
		e.Severity = uint8(severity)
		e.Count = uint64(count)
		out = append(out, e)
	}
	return out, rows.Err()
}
