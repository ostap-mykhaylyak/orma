package install

import (
	"strings"
	"testing"
)

func TestUnitContieneLeDirettiveCheContano(t *testing.T) {
	generata := unit("/usr/local/bin/orma", "/etc/orma/orma.yaml",
		[]string{"/var/lib/orma", "/run/orma"})

	obbligatorie := map[string]string{
		"ExecStart=/usr/local/bin/orma start --config /etc/orma/orma.yaml": "il comando di avvio deve puntare al binario e alla configurazione",
		"Restart=on-failure":         "always annullerebbe un arresto voluto",
		"RuntimeDirectory=orma":      "/run e' tmpfs: senza, la directory del socket sparisce al riavvio",
		"StateDirectory=orma":        "il database deve sopravvivere agli aggiornamenti",
		"WantedBy=multi-user.target": "senza, il servizio non parte al boot",
		"ReadWritePaths=/var/lib/orma /run/orma": "con ProtectSystem=strict il resto del filesystem e' in sola lettura",
	}

	for direttiva, perche := range obbligatorie {
		if !strings.Contains(generata, direttiva) {
			t.Errorf("manca %q: %s", direttiva, perche)
		}
	}

	// Restart=always e' l'errore facile: farebbe ripartire il daemon dopo un
	// "orma stop" voluto, e sembrerebbe un baco del comando stop.
	if strings.Contains(generata, "Restart=always") {
		t.Error("Restart=always vanificherebbe l'arresto manuale")
	}
}

func TestPercorsoStabile(t *testing.T) {
	casi := map[string]bool{
		"/usr/local/bin/orma": true,
		"/usr/bin/orma":       true,
		"/opt/orma/orma":      true,
		"/root/orma":          false,
		"/tmp/orma":           false,
		"/home/utente/orma":   false,
	}

	for percorso, atteso := range casi {
		if got := percorsoStabile(percorso); got != atteso {
			t.Errorf("percorsoStabile(%q) = %v, atteso %v", percorso, got, atteso)
		}
	}
}
