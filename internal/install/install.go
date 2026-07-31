// Package install mette l'estensione al posto giusto e verifica che PHP la
// carichi davvero.
//
// L'ABI di un'estensione dipende dalla versione di PHP, dal flag ZTS e dal
// flag debug. Invece di confrontare quei tre valori sperando di non
// dimenticarne uno, si chiede a PHP di caricare l'estensione e si guarda se
// compare fra i moduli: e' l'unica verifica che non puo' mentire.
package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Esito descrive cosa e' stato fatto.
type Esito struct {
	ExtensionDir string
	Destinazione string
	IniPath      string
	Nota         string
}

// Estensione copia il file .so nella extension_dir di PHP, scrive l'INI e
// verifica il caricamento. phpBin vuoto significa "php" nel PATH; soPath vuoto
// significa "orma.so" accanto al binario di orma.
func Estensione(phpBin, soPath, appName, socket string) (Esito, error) {
	var esito Esito

	if phpBin == "" {
		phpBin = "php"
	}
	if _, err := exec.LookPath(phpBin); err != nil {
		return esito, fmt.Errorf("%s non trovato: indica l'interprete con --php", phpBin)
	}

	if soPath == "" {
		var err error
		if soPath, err = accanto("orma.so"); err != nil {
			return esito, err
		}
	}
	if _, err := os.Stat(soPath); err != nil {
		return esito, fmt.Errorf("estensione non trovata in %s: indicala con --extension", soPath)
	}

	dir, err := valore(phpBin, `echo ini_get("extension_dir");`)
	if err != nil {
		return esito, fmt.Errorf("lettura di extension_dir: %w", err)
	}
	if dir == "" {
		return esito, fmt.Errorf("PHP non riporta una extension_dir")
	}
	esito.ExtensionDir = dir

	esito.Destinazione = filepath.Join(dir, "orma.so")
	if err := copia(soPath, esito.Destinazione); err != nil {
		return esito, err
	}

	// La verifica prima dell'INI: se l'estensione non si carica, meglio non
	// aver lasciato in giro un INI che rompe ogni esecuzione di PHP.
	if err := verifica(phpBin); err != nil {
		os.Remove(esito.Destinazione)
		return esito, err
	}

	esito.IniPath, esito.Nota, err = scriviIni(phpBin, appName, socket)
	if err != nil {
		return esito, err
	}
	return esito, nil
}

func accanto(nome string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), nome), nil
}

func valore(phpBin, codice string) (string, error) {
	out, err := exec.Command(phpBin, "-r", codice).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func copia(da, a string) error {
	sorgente, err := os.Open(da)
	if err != nil {
		return err
	}
	defer sorgente.Close()

	destinazione, err := os.OpenFile(a, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("scrittura di %s: %w", a, err)
	}
	defer destinazione.Close()

	if _, err := io.Copy(destinazione, sorgente); err != nil {
		return fmt.Errorf("copia in %s: %w", a, err)
	}
	return nil
}

func verifica(phpBin string) error {
	out, err := exec.Command(phpBin, "-d", "extension=orma.so", "-m").CombinedOutput()
	if err != nil {
		return fmt.Errorf("php non ha potuto caricare l'estensione: %w\n%s", err, out)
	}
	for _, riga := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(riga) == "orma" {
			return nil
		}
	}
	return fmt.Errorf("php non elenca orma fra i moduli: l'artefatto non corrisponde "+
		"a questa build di PHP\n%s", out)
}

// scriviIni mette l'INI dove la distribuzione se lo aspetta. Su Debian e
// Ubuntu e' mods-available, da abilitare con phpenmod; altrove e' la directory
// di scansione riportata da PHP.
func scriviIni(phpBin, appName, socket string) (percorso, nota string, err error) {
	contenuto := fmt.Sprintf(`; Generato da orma --init
extension=orma.so

orma.app_name=%s
orma.socket=%s

; 0 nessuna instrumentazione delle funzioni utente, 1 solo sopra soglia, 2 tutto
orma.detail=1
orma.function_ms=5
orma.max_depth=5

; Classi di eccezione da non registrare, separate da virgola.
;orma.ignored_exceptions=
`, appName, socket)

	versione, err := valore(phpBin, `echo PHP_MAJOR_VERSION . "." . PHP_MINOR_VERSION;`)
	if err == nil && versione != "" {
		mods := filepath.Join("/etc/php", versione, "mods-available")
		if info, statErr := os.Stat(mods); statErr == nil && info.IsDir() {
			percorso = filepath.Join(mods, "orma.ini")
			if err := os.WriteFile(percorso, []byte(contenuto), 0o644); err != nil {
				return "", "", fmt.Errorf("scrittura di %s: %w", percorso, err)
			}
			return percorso, "abilitala con: phpenmod orma && systemctl restart php" + versione + "-fpm", nil
		}
	}

	scan, err := directoryDiScansione(phpBin)
	if err != nil {
		return "", "", err
	}
	percorso = filepath.Join(scan, "99-orma.ini")
	if err := os.WriteFile(percorso, []byte(contenuto), 0o644); err != nil {
		return "", "", fmt.Errorf("scrittura di %s: %w", percorso, err)
	}
	return percorso, "riavvia php-fpm perche' l'estensione venga caricata", nil
}

func directoryDiScansione(phpBin string) (string, error) {
	out, err := exec.Command(phpBin, "--ini").Output()
	if err != nil {
		return "", err
	}
	for _, riga := range strings.Split(string(out), "\n") {
		_, dopo, trovato := strings.Cut(riga, "Scan for additional .ini files in:")
		if !trovato {
			continue
		}
		dir := strings.TrimSpace(dopo)
		if dir != "" && dir != "(none)" {
			return dir, nil
		}
	}
	return "", fmt.Errorf("php non riporta una directory di scansione degli INI: " +
		"aggiungi a mano extension=orma.so al php.ini")
}
