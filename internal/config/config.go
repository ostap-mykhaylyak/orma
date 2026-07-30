// Package config carica e valida la configurazione del daemon orma.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultPath e' il percorso usato quando non se ne indica un altro con --config.
const DefaultPath = "/etc/orma/orma.yaml"

// Config e' la configurazione del daemon. Tutti i campi hanno un default
// utilizzabile: un file vuoto e' una configurazione valida.
type Config struct {
	// Socket e' il socket unix su cui l'estensione consegna i payload.
	Socket string `yaml:"socket"`
	// PidFile traccia l'istanza in esecuzione per stop, reload e status.
	PidFile string `yaml:"pid_file"`
	// Database e' il file SQLite con metriche, trace, slow SQL ed errori.
	Database string `yaml:"database"`
	// Listen e' l'indirizzo della UI web.
	Listen string `yaml:"listen"`
	// LogLevel e' uno fra debug, info, warn, error.
	LogLevel string `yaml:"log_level"`
}

// Default restituisce la configurazione predefinita.
func Default() Config {
	return Config{
		Socket:   "/run/orma/orma.sock",
		PidFile:  "/run/orma/orma.pid",
		Database: "/var/lib/orma/orma.db",
		Listen:   "127.0.0.1:8737",
		LogLevel: "info",
	}
}

// Load legge la configurazione dal percorso indicato, applicando i default per i
// campi assenti. Un percorso inesistente non e' un errore solo se e' quello
// predefinito: se l'utente ne indica uno esplicito e non c'e', vuole saperlo.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && path == DefaultPath {
			return cfg, nil
		}
		return cfg, fmt.Errorf("lettura di %s: %w", path, err)
	}

	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s non e' YAML valido: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate verifica che la configurazione sia utilizzabile.
func (c Config) Validate() error {
	if c.Socket == "" {
		return fmt.Errorf("socket non puo' essere vuoto")
	}
	if !filepath.IsAbs(c.Socket) {
		return fmt.Errorf("socket deve essere un percorso assoluto, non %q", c.Socket)
	}
	if c.PidFile == "" {
		return fmt.Errorf("pid_file non puo' essere vuoto")
	}
	if c.Database == "" {
		return fmt.Errorf("database non puo' essere vuoto")
	}
	if c.Listen == "" {
		return fmt.Errorf("listen non puo' essere vuoto")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q non valido: usa debug, info, warn o error", c.LogLevel)
	}
	return nil
}

// Template e' la configurazione commentata scritta da "orma --init".
const Template = `# Configurazione di orma.
# Tutti i valori qui sotto sono i default: e' sufficiente rimuovere il commento
# dalle righe che si vogliono cambiare.

# Socket unix su cui l'estensione PHP consegna i payload.
# Deve coincidere con orma.socket nell'INI dell'estensione.
#socket: /run/orma/orma.sock

# File che traccia l'istanza in esecuzione.
#pid_file: /run/orma/orma.pid

# Database SQLite con metriche, trace, slow SQL ed errori.
#database: /var/lib/orma/orma.db

# Indirizzo della UI web. Tienila dietro a un reverse proxy: non ha autenticazione propria.
#listen: 127.0.0.1:8737

# Verbosita': debug, info, warn, error.
#log_level: info
`
