package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PercorsoUnit e' dove finisce l'unit systemd.
const PercorsoUnit = "/etc/systemd/system/orma.service"

// PercorsoBinario e' dove --init mette il binario se lo trova in un posto
// non adatto a essere referenziato da un servizio.
const PercorsoBinario = "/usr/local/bin/orma"

// SystemdDisponibile indica se c'e' systemd con cui parlare. Nei container
// senza init non c'e', e non e' un errore: si installa tutto il resto.
func SystemdDisponibile() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// systemctl esiste anche dove systemd non e' il processo 1, per esempio
	// in un container docker: li' qualunque comando fallirebbe.
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return true
}

// UnitInstallata indica se l'unit e' presente.
func UnitInstallata() bool {
	_, err := os.Stat(PercorsoUnit)
	return err == nil
}

// UnitAttiva indica se il servizio sta girando sotto systemd.
func UnitAttiva() bool {
	if !SystemdDisponibile() || !UnitInstallata() {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", "orma").Run() == nil
}

// Systemctl esegue un'azione sul servizio e restituisce l'output combinato.
func Systemctl(azione string) (string, error) {
	out, err := exec.Command("systemctl", azione, "orma").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// unit genera il contenuto dell'unit.
//
// Gira come root perche' deve poter assegnare il socket al gruppo di php-fpm e
// scrivere in /var/lib: e' anche il modo in cui il daemon viene lanciato a mano
// durante le prove, quindi non si creano conflitti di proprieta' sui file
// passando dall'uno all'altro. Le direttive di irrobustimento limitano cio' che
// quel root puo' toccare.
func unit(binario, configurazione string, scrivibili []string) string {
	if len(scrivibili) == 0 {
		scrivibili = []string{"/var/lib/orma", "/run/orma"}
	}
	return fmt.Sprintf(`[Unit]
Description=orma — APM per PHP
Documentation=https://github.com/ostap-mykhaylyak/orma
After=network.target

[Service]
Type=simple
ExecStart=%s start --config %s
ExecReload=/bin/kill -HUP $MAINPID

# on-failure e non always: un "orma stop" volontario non deve essere annullato
# da systemd che lo rimette su.
Restart=on-failure
RestartSec=5s

# La directory del socket la crea systemd e la ricrea a ogni avvio: /run e'
# tmpfs e sparisce a ogni riavvio della macchina.
RuntimeDirectory=orma
RuntimeDirectoryMode=0755
StateDirectory=orma
StateDirectoryMode=0755

# Irrobustimento: il daemon legge un socket, scrive un database e serve una
# pagina in locale. Non gli serve nient'altro.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`, binario, configurazione, strings.Join(scrivibili, " "))
}

// EsitoServizio racconta cosa e' stato fatto.
type EsitoServizio struct {
	Unit          string
	Binario       string
	BinarioCopiato bool
	Abilitato     bool
}

// Servizio installa l'unit systemd e la abilita all'avvio.
//
// Se il binario si trova in un posto non adatto a essere referenziato da un
// servizio — la home di root, una directory temporanea — viene copiato in
// /usr/local/bin: un ExecStart che punta a /root/orma funziona finche' quel
// file non viene spostato, e poi il servizio non riparte piu' al boot senza
// che nessuno capisca perche'.
func Servizio(configurazione string, scrivibili []string) (EsitoServizio, error) {
	var esito EsitoServizio

	if !SystemdDisponibile() {
		return esito, fmt.Errorf("systemd non disponibile")
	}

	binario, err := os.Executable()
	if err != nil {
		return esito, err
	}
	if binario, err = filepath.EvalSymlinks(binario); err != nil {
		return esito, err
	}

	if !percorsoStabile(binario) {
		if err := copia(binario, PercorsoBinario); err != nil {
			return esito, fmt.Errorf("copia del binario in %s: %w", PercorsoBinario, err)
		}
		if err := os.Chmod(PercorsoBinario, 0o755); err != nil {
			return esito, err
		}
		binario = PercorsoBinario
		esito.BinarioCopiato = true
	}
	esito.Binario = binario

	if err := os.WriteFile(PercorsoUnit, []byte(unit(binario, configurazione, scrivibili)), 0o644); err != nil {
		return esito, fmt.Errorf("scrittura di %s: %w", PercorsoUnit, err)
	}
	esito.Unit = PercorsoUnit

	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return esito, fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}
	if out, err := exec.Command("systemctl", "enable", "orma").CombinedOutput(); err != nil {
		return esito, fmt.Errorf("systemctl enable orma: %w\n%s", err, out)
	}
	esito.Abilitato = true

	return esito, nil
}

// percorsoStabile indica se il binario sta in una directory da cui un servizio
// puo' ragionevolmente essere avviato.
func percorsoStabile(binario string) bool {
	for _, prefisso := range []string{"/usr/local/bin/", "/usr/bin/", "/usr/sbin/", "/opt/"} {
		if strings.HasPrefix(binario, prefisso) {
			return true
		}
	}
	return false
}

// RimuoviServizio ferma, disabilita e cancella l'unit. Restituisce cio' che ha
// rimosso.
func RimuoviServizio() ([]string, error) {
	var rimossi []string

	if !UnitInstallata() {
		return rimossi, nil
	}

	if SystemdDisponibile() {
		_, _ = Systemctl("stop")
		_, _ = Systemctl("disable")
	}

	r, err := rimuoviSeEsiste(PercorsoUnit)
	if err != nil {
		return rimossi, err
	}
	if r != "" {
		rimossi = append(rimossi, r)
	}

	if SystemdDisponibile() {
		_, _ = exec.Command("systemctl", "daemon-reload").CombinedOutput()
	}
	return rimossi, nil
}
