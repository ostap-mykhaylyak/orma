package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/metrics"
)

// campioni conta i campioni dentro un istogramma serializzato.
func campioniIstogramma(raw []byte) uint64 {
	h := metrics.Decode(raw)
	var n uint64
	for _, c := range h {
		n += uint64(c)
	}
	return n
}

func apriProva(t *testing.T, r Retention) *Store {
	t.Helper()

	st, err := Open(filepath.Join(t.TempDir(), "prova.db"), nil, r)
	if err != nil {
		t.Fatalf("apertura del database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func retentionProva() Retention {
	return Retention{
		Minuto:  24 * time.Hour,
		Cinque:  7 * 24 * time.Hour,
		Ora:     395 * 24 * time.Hour,
		Tracce:  7 * 24 * time.Hour,
		Errori:  30 * 24 * time.Hour,
		QuerySQ: 7 * 24 * time.Hour,
	}
}

// scriviMinuto inserisce una serie in un bucket da un minuto.
func scriviMinuto(t *testing.T, st *Store, bucket int64, durataNS uint64, quante int) {
	t.Helper()

	w := NewWindow()
	key := Key{App: "prova", Txn: "/carrello", Kind: "web", Category: CategoriaTotale}
	b := &Bucket{MinNS: durataNS}
	for i := 0; i < quante; i++ {
		b.Count++
		b.SumNS += durataNS
		if durataNS > b.MaxNS {
			b.MaxNS = durataNS
		}
		b.Hist.Add(durataNS)
	}
	w.Metrics[key] = b

	if err := st.WriteWindow(bucket, w); err != nil {
		t.Fatalf("scrittura della finestra %d: %v", bucket, err)
	}
}

func conta(t *testing.T, st *Store, tabella string) (righe int, campioni int64) {
	t.Helper()

	err := st.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(count), 0) FROM `+tabella).Scan(&righe, &campioni)
	if err != nil {
		t.Fatalf("conteggio su %s: %v", tabella, err)
	}
	return righe, campioni
}

func TestRollupUnisceIMinutiInCinque(t *testing.T) {
	st := apriProva(t, retentionProva())

	// L'ultimo bucket da cinque minuti gia' chiuso: quello in corso verrebbe
	// ignorato, perche' cambierebbe ancora.
	now := time.Now().Unix()
	bucket5 := now - now%300 - 300

	scriviMinuto(t, st, bucket5, 10*1e6, 3)
	scriviMinuto(t, st, bucket5+60, 20*1e6, 2)
	scriviMinuto(t, st, bucket5+120, 30*1e6, 1)

	if err := st.Rollup(nil); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	righe, campioni := conta(t, st, Tabella5m)
	if righe != 1 {
		t.Fatalf("metrics_5m: %d righe, attesa 1", righe)
	}
	if campioni != 6 {
		t.Errorf("metrics_5m: %d campioni, attesi 6", campioni)
	}

	// Il rollup non consuma la granularita' fine: e' la purga a rimuoverla.
	if righe, _ := conta(t, st, Tabella1m); righe != 3 {
		t.Errorf("metrics_1m: %d righe dopo il rollup, attese 3", righe)
	}

	var sumNS, minNS, maxNS int64
	var hist []byte
	if err := st.db.QueryRow(
		`SELECT sum_ns, min_ns, max_ns, histogram FROM metrics_5m`).
		Scan(&sumNS, &minNS, &maxNS, &hist); err != nil {
		t.Fatalf("lettura della riga aggregata: %v", err)
	}

	atteso := int64(3*10+2*20+1*30) * 1e6
	if sumNS != atteso {
		t.Errorf("sum_ns = %d, atteso %d", sumNS, atteso)
	}
	if minNS != 10*1e6 || maxNS != 30*1e6 {
		t.Errorf("min/max = %d/%d, attesi %d/%d", minNS, maxNS, int64(10*1e6), int64(30*1e6))
	}
	if h := campioniIstogramma(hist); h != 6 {
		t.Errorf("istogramma aggregato: %d campioni, attesi 6", h)
	}
}

// Il ricalcolo e' idempotente: girare due volte non deve raddoppiare nulla.
func TestRollupIdempotente(t *testing.T) {
	st := apriProva(t, retentionProva())

	now := time.Now().Unix()
	bucket5 := now - now%300 - 300
	scriviMinuto(t, st, bucket5, 10*1e6, 4)

	for i := 0; i < 3; i++ {
		if err := st.Rollup(nil); err != nil {
			t.Fatalf("rollup %d: %v", i, err)
		}
	}

	righe, campioni := conta(t, st, Tabella5m)
	if righe != 1 || campioni != 4 {
		t.Errorf("dopo tre giri: %d righe e %d campioni, attesi 1 e 4", righe, campioni)
	}
}

func TestPurgaRimuoveSoloLoScaduto(t *testing.T) {
	st := apriProva(t, retentionProva())

	now := time.Now().Unix()
	recente := now - now%60 - 60
	vecchio := now - 48*3600

	scriviMinuto(t, st, recente, 10*1e6, 1)
	scriviMinuto(t, st, vecchio, 10*1e6, 1)

	if righe, _ := conta(t, st, Tabella1m); righe != 2 {
		t.Fatalf("prima della purga: %d righe, attese 2", righe)
	}

	if err := st.Purge(retentionProva(), nil); err != nil {
		t.Fatalf("purga: %v", err)
	}

	righe, _ := conta(t, st, Tabella1m)
	if righe != 1 {
		t.Errorf("dopo la purga: %d righe, attesa 1", righe)
	}
}

func TestTabellaPerSceglieLaGranularita(t *testing.T) {
	st := apriProva(t, retentionProva())
	now := time.Now()

	casi := []struct {
		nome   string
		since  time.Time
		attesa string
	}{
		{"un'ora", now.Add(-time.Hour), Tabella1m},
		{"tre giorni", now.Add(-72 * time.Hour), Tabella5m},
		{"un mese", now.Add(-30 * 24 * time.Hour), Tabella1h},
	}

	for _, c := range casi {
		t.Run(c.nome, func(t *testing.T) {
			if got := st.tabellaPer(c.since.Unix(), retentionProva()); got != c.attesa {
				t.Errorf("tabellaPer = %s, attesa %s", got, c.attesa)
			}
		})
	}
}
