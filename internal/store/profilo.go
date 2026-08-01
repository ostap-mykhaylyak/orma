package store

import (
	"database/sql"
	"errors"
)

// ProfKey identifica una funzione interna dentro una transazione.
type ProfKey struct {
	App      string
	Txn      string
	Funzione string
}

// ProfBucket accumula chiamate e tempo di una funzione interna.
type ProfBucket struct {
	Chiamate uint64
	SumNS    uint64
}

const profiloSchema = `
CREATE TABLE IF NOT EXISTS profilo (
	app_id    INTEGER NOT NULL,
	bucket_ts INTEGER NOT NULL,
	txn_name  TEXT    NOT NULL,
	funzione  TEXT    NOT NULL,
	chiamate  INTEGER NOT NULL,
	sum_ns    INTEGER NOT NULL,
	PRIMARY KEY (app_id, bucket_ts, txn_name, funzione)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS profilo_ts ON profilo (bucket_ts);
`

func writeProfilo(tx *sql.Tx, appIDs map[string]int64, window int64, voci map[ProfKey]*ProfBucket) error {
	for key, b := range voci {
		var exChiamate, exSum int64
		err := tx.QueryRow(
			`SELECT chiamate, sum_ns FROM profilo
			  WHERE app_id = ? AND bucket_ts = ? AND txn_name = ? AND funzione = ?`,
			appIDs[key.App], window, key.Txn, key.Funzione).Scan(&exChiamate, &exSum)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO profilo
			 (app_id, bucket_ts, txn_name, funzione, chiamate, sum_ns)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			appIDs[key.App], window, key.Txn, key.Funzione,
			int64(b.Chiamate)+exChiamate, int64(b.SumNS)+exSum); err != nil {
			return err
		}
	}
	return nil
}

// ProfStat e' una riga della tabella del profilo.
type ProfStat struct {
	Funzione string
	Chiamate uint64
	TotalMS  float64
	// PerChiamataMS distingue "tante chiamate banali" da "poche chiamate
	// costose": sono due problemi con due rimedi diversi.
	PerChiamataMS float64
	QuotaPct      float64
}

// Profilo restituisce le funzioni interne piu' costose. txn vuoto significa
// tutte le transazioni.
//
// La quota e' calcolata sul tempo totale delle funzioni profilate, non su
// quello della transazione: dire "il 40% del tempo profilato" e' onesto, dire
// "il 40% della richiesta" non lo sarebbe, perche' il profilo copre solo le
// funzioni in elenco.
func (s *Store) Profilo(since int64, txn string, limit int) ([]ProfStat, error) {
	query := `SELECT funzione, SUM(chiamate), SUM(sum_ns)
	            FROM profilo WHERE bucket_ts >= ?`
	args := []any{since}
	if txn != "" {
		query += ` AND txn_name = ?`
		args = append(args, txn)
	}
	query += ` GROUP BY funzione ORDER BY SUM(sum_ns) DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProfStat
	var totale float64

	for rows.Next() {
		var p ProfStat
		var chiamate, sumNS int64
		if err := rows.Scan(&p.Funzione, &chiamate, &sumNS); err != nil {
			return nil, err
		}
		p.Chiamate = uint64(chiamate)
		p.TotalMS = float64(sumNS) / 1e6
		if chiamate > 0 {
			p.PerChiamataMS = p.TotalMS / float64(chiamate)
		}
		totale += p.TotalMS
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if totale > 0 {
		for i := range out {
			out[i].QuotaPct = out[i].TotalMS / totale * 100
		}
	}
	return out, nil
}
