package web

import (
	"fmt"
	"os"
	"path/filepath"
)

// Quanti elementi seguire nell'export. Un export deve stare in una directory
// che si copia e si guarda, non diventare un mirror del database.
const (
	maxTransazioniEsportate = 30
	maxTracceEsportate      = 40
)

// Esporta genera le pagine come file HTML autonomi.
//
// Serve dove al pannello non si arriva: nessun proxy da configurare, nessuna
// porta da esporre, nessun token da passare. I file si aprono con un doppio
// clic e i collegamenti fra loro restano relativi, quindi la directory si
// copia altrove e continua a funzionare.
//
// I dati sono congelati al momento della generazione, e per questo il
// selettore di intervallo sparisce: lasciarlo darebbe collegamenti morti.
func (s *Server) Esporta(dir string, minuti int) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creazione di %s: %w", dir, err)
	}

	since := sinceDaMinuti(minuti)
	var scritti []string

	scrivi := func(nome, template string, dati any) error {
		percorso := filepath.Join(dir, nome)
		f, err := os.Create(percorso)
		if err != nil {
			return fmt.Errorf("creazione di %s: %w", percorso, err)
		}
		defer f.Close()

		if err := s.tmpl.ExecuteTemplate(f, template, dati); err != nil {
			return fmt.Errorf("rendering di %s: %w", nome, err)
		}
		scritti = append(scritti, percorso)
		return nil
	}

	base := func(titolo, pagina string) comune {
		c := newComune(titolo, pagina, minuti)
		c.Statico = true
		return c
	}

	panoramica, err := s.costruisciPanoramica(since, base("Panoramica", "panoramica"))
	if err != nil {
		return scritti, err
	}
	if err := scrivi("panoramica.html", "panoramica.html", panoramica); err != nil {
		return scritti, err
	}

	database, err := s.costruisciDatabase(since, base("Database", "database"))
	if err != nil {
		return scritti, err
	}
	if err := scrivi("database.html", "database.html", database); err != nil {
		return scritti, err
	}

	esterne, err := s.costruisciEsterne(since, base("Esterne", "esterne"))
	if err != nil {
		return scritti, err
	}
	if err := scrivi("esterne.html", "esterne.html", esterne); err != nil {
		return scritti, err
	}

	errori, err := s.costruisciErrori(since, base("Errori", "errori"))
	if err != nil {
		return scritti, err
	}
	if err := scrivi("errori.html", "errori.html", errori); err != nil {
		return scritti, err
	}

	tracce, err := s.costruisciTracce(since, base("Tracce", "tracce"))
	if err != nil {
		return scritti, err
	}
	if err := scrivi("tracce.html", "tracce.html", tracce); err != nil {
		return scritti, err
	}

	// Il dettaglio delle transazioni piu' pesanti: sono quelle per cui si
	// apre un export.
	for i, txn := range panoramica.Txns {
		if i >= maxTransazioniEsportate {
			break
		}
		dati, err := s.costruisciTransazione(since, txn.Name, base(txn.Name, "panoramica"))
		if err != nil {
			return scritti, err
		}
		if err := scrivi("transazione-"+nomeFile(txn.Name)+".html", "transazione.html", dati); err != nil {
			return scritti, err
		}
	}

	for i, riga := range tracce.Tracce {
		if i >= maxTracceEsportate {
			break
		}
		dati, err := s.costruisciTraccia(riga.ID, base("Trace "+riga.Name, "tracce"))
		if err != nil {
			// Un trace sparito fra l'elenco e il dettaglio non deve far
			// fallire tutto l'export.
			continue
		}
		if err := scrivi(fmt.Sprintf("traccia-%d.html", riga.ID), "traccia.html", dati); err != nil {
			return scritti, err
		}
	}

	return scritti, nil
}
