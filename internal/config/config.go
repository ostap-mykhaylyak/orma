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
	// TraceThresholdMS e' la durata sopra la quale si conserva il trace
	// completo. Le transazioni in errore si conservano comunque.
	TraceThresholdMS int `yaml:"trace_threshold_ms"`
	// TraceMaxPerMin limita quanti trace si conservano al minuto.
	TraceMaxPerMin int `yaml:"trace_max_per_min"`
	// MaxTxnNames e' il tetto ai nomi di transazione distinti per minuto.
	MaxTxnNames int `yaml:"max_txn_names"`
	// ApdexTMS e' la soglia di soddisfazione, in millisecondi: entro questa
	// una richiesta conta intera, entro il quadruplo conta meta'.
	ApdexTMS int `yaml:"apdex_t_ms"`
}

// Default restituisce la configurazione predefinita.
func Default() Config {
	return Config{
		Socket:   "/run/orma/orma.sock",
		PidFile:  "/run/orma/orma.pid",
		Database: "/var/lib/orma/orma.db",
		Listen:   "127.0.0.1:8737",
		LogLevel: "info",

		TraceThresholdMS: 500,
		TraceMaxPerMin:   20,
		MaxTxnNames:      5000,
		ApdexTMS:         500,
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
	if c.TraceThresholdMS < 0 {
		return fmt.Errorf("trace_threshold_ms non puo' essere negativo")
	}
	if c.TraceMaxPerMin <= 0 {
		return fmt.Errorf("trace_max_per_min deve essere maggiore di zero")
	}
	if c.MaxTxnNames <= 0 {
		return fmt.Errorf("max_txn_names deve essere maggiore di zero")
	}
	if c.ApdexTMS <= 0 {
		return fmt.Errorf("apdex_t_ms deve essere maggiore di zero")
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

# Durata oltre la quale si conserva il trace completo, in millisecondi.
# Le transazioni in errore si conservano comunque. A 0 si conserva tutto,
# utile solo per una diagnosi puntuale.
#trace_threshold_ms: 500

# Quanti trace conservare al massimo per minuto.
#trace_max_per_min: 20

# Tetto ai nomi di transazione distinti per minuto: oltre, i nuovi
# confluiscono in OtherTransaction/*. E' la valvola che impedisce a
# un'applicazione con URL generati di riempire il database.
#max_txn_names: 5000

# Soglia di soddisfazione per l'apdex, in millisecondi: entro questa una
# richiesta conta intera, entro il quadruplo conta meta', oltre non conta.
#apdex_t_ms: 500
`
