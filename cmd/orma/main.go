// Comando orma: daemon di ingestione APM e CLI di servizio.
//
// Convenzione della CLI: i verbi di servizio si scrivono senza trattini
// (start, stop, reload, restart, status), tutto il resto obbligatoriamente con
// i trattini (--init, --check-config, --version).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/agg"
	"github.com/ostap-mykhaylyak/orma/internal/allarmi"
	"github.com/ostap-mykhaylyak/orma/internal/config"
	"github.com/ostap-mykhaylyak/orma/internal/ingest"
	"github.com/ostap-mykhaylyak/orma/internal/install"
	"github.com/ostap-mykhaylyak/orma/internal/pidfile"
	"github.com/ostap-mykhaylyak/orma/internal/proc"
	"github.com/ostap-mykhaylyak/orma/internal/protocol"
	"github.com/ostap-mykhaylyak/orma/internal/store"
	"github.com/ostap-mykhaylyak/orma/internal/version"
	"github.com/ostap-mykhaylyak/orma/internal/web"
)

const usage = `orma — APM per PHP

Uso:
  orma <verbo>            start | stop | reload | restart | status
  orma --init             genera la configurazione e installa l'estensione
  orma --purge            disinstalla l'estensione e rimuove dati e configurazione
  orma --enable           riattiva la raccolta e la ricarica in php-fpm
  orma --disable          sospende la raccolta senza disinstallare nulla
  orma --export <dir>     genera le pagine come HTML statico, da consultare
                          senza passare dal pannello
  orma --check-config     valida la configurazione ed esce
  orma --version          stampa la versione
  orma --help             stampa questo messaggio

Opzioni:
  --config <percorso>     configurazione da usare (default: ` + config.DefaultPath + `)
  --extension <percorso>  estensione da installare con --init (default: orma.so
                          accanto a questo binario)
  --php <percorso>        interprete su cui installare l'estensione (default: php)
  --senza-estensione      con --init, scrive solo la configurazione del daemon
`

var verbs = map[string]bool{
	"start": true, "stop": true, "reload": true, "restart": true, "status": true,
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "orma: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	configPath      string
	verb            string
	action          string
	extensionPath   string
	phpBin          string
	senzaEstensione bool
	exportDir       string
	minuti          int
}

func run(args []string) error {
	opts, err := parse(args)
	if err != nil {
		return err
	}

	switch opts.action {
	case "help":
		fmt.Print(usage)
		return nil
	case "version":
		fmt.Printf("orma %s (%s)\n", version.Version, version.Commit)
		return nil
	case "init":
		return doInit(opts)
	case "purge":
		return doPurge(opts)
	case "enable":
		return doAbilita(opts, true)
	case "disable":
		return doAbilita(opts, false)
	case "export":
		return doExport(opts)
	case "check-config":
		return doCheckConfig(opts.configPath)
	}

	if opts.verb == "" {
		fmt.Print(usage)
		return nil
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	switch opts.verb {
	case "start":
		return doStart(cfg, opts.configPath)
	case "stop":
		return doStop(cfg)
	case "reload":
		return doReload(cfg)
	case "restart":
		return doRestart(cfg, opts.configPath)
	case "status":
		return doStatus(cfg)
	}
	return fmt.Errorf("verbo non gestito: %s", opts.verb)
}

func parse(args []string) (options, error) {
	opts := options{configPath: config.DefaultPath}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				return opts, errors.New("--config richiede un percorso")
			}
			i++
			opts.configPath = args[i]

		case strings.HasPrefix(arg, "--config="):
			opts.configPath = strings.TrimPrefix(arg, "--config=")
			if opts.configPath == "" {
				return opts, errors.New("--config richiede un percorso")
			}

		case arg == "--extension", arg == "--php":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s richiede un percorso", arg)
			}
			i++
			if arg == "--extension" {
				opts.extensionPath = args[i]
			} else {
				opts.phpBin = args[i]
			}

		case strings.HasPrefix(arg, "--extension="):
			opts.extensionPath = strings.TrimPrefix(arg, "--extension=")

		case strings.HasPrefix(arg, "--php="):
			opts.phpBin = strings.TrimPrefix(arg, "--php=")

		case arg == "--senza-estensione":
			opts.senzaEstensione = true

		case arg == "--export":
			if i+1 >= len(args) {
				return opts, errors.New("--export richiede una directory")
			}
			i++
			opts.exportDir = args[i]
			opts.action = "export"

		case strings.HasPrefix(arg, "--export="):
			opts.exportDir = strings.TrimPrefix(arg, "--export=")
			opts.action = "export"

		case arg == "--minuti":
			if i+1 >= len(args) {
				return opts, errors.New("--minuti richiede un numero")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return opts, fmt.Errorf("--minuti vuole un numero positivo, non %q", args[i])
			}
			opts.minuti = n

		case arg == "--init", arg == "--purge", arg == "--enable", arg == "--disable",
			arg == "--check-config", arg == "--version", arg == "--help":
			action := strings.TrimPrefix(arg, "--")
			if opts.action != "" && opts.action != action {
				return opts, fmt.Errorf("--%s e %s si escludono a vicenda", opts.action, arg)
			}
			opts.action = action

		case strings.HasPrefix(arg, "-"):
			// Errore frequente: i verbi di servizio non vogliono i trattini.
			if bare := strings.TrimLeft(arg, "-"); verbs[bare] {
				return opts, fmt.Errorf("i verbi di servizio si scrivono senza trattini: usa %q, non %q", bare, arg)
			}
			return opts, fmt.Errorf("opzione sconosciuta: %s", arg)

		case verbs[arg]:
			if opts.verb != "" {
				return opts, fmt.Errorf("un verbo alla volta: %q e %q", opts.verb, arg)
			}
			opts.verb = arg

		default:
			return opts, fmt.Errorf("verbo sconosciuto: %q", arg)
		}
	}

	if opts.verb != "" && opts.action != "" && opts.action != "help" {
		return opts, fmt.Errorf("il verbo %q e --%s si escludono a vicenda", opts.verb, opts.action)
	}
	return opts, nil
}

func doInit(opts options) error {
	path := opts.configPath

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s esiste gia': rimuovilo se vuoi rigenerarlo", path)
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creazione di %s: %w", filepath.Dir(path), err)
	}
	// Il token si genera qui e non si lascia vuoto: un default aperto e' un
	// default che resta aperto. La configurazione va scritta 0600, perche' da
	// questo momento contiene una credenziale.
	token, err := generaToken()
	if err != nil {
		return err
	}
	contenuto := config.Template + "\nui_token: " + token + "\n"
	if err := os.WriteFile(path, []byte(contenuto), 0o600); err != nil {
		return fmt.Errorf("scrittura di %s: %w", path, err)
	}

	cfg := config.Default()
	for _, dir := range []string{filepath.Dir(cfg.Socket), filepath.Dir(cfg.Database)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creazione di %s: %w", dir, err)
		}
	}

	fmt.Printf("Scritto  %s\n", path)
	fmt.Printf("Create   %s, %s\n", filepath.Dir(cfg.Socket), filepath.Dir(cfg.Database))
	fmt.Printf("\nToken di accesso all'interfaccia:\n\n  %s\n\n", token)
	fmt.Printf("  http://%s/?token=%s\n\n", cfg.Listen, token)

	if opts.senzaEstensione {
		fmt.Print("\nEstensione non installata (--senza-estensione).\n")
		return nil
	}

	nome, _ := os.Hostname()
	if nome == "" {
		nome = "default"
	}

	esito, err := install.Estensione(opts.phpBin, opts.extensionPath, nome, cfg.Socket)
	if err != nil {
		// La configurazione del daemon e' comunque a posto: si dice cosa manca
		// invece di far sembrare fallito tutto.
		fmt.Fprintf(os.Stderr, "\nEstensione non installata: %v\n", err)
		fmt.Fprint(os.Stderr, "Il daemon e' configurato: reinstalla l'estensione con --init dopo aver risolto.\n")
		return err
	}

	fmt.Printf("Copiata  %s\n", esito.Destinazione)
	fmt.Printf("Scritto  %s\n", esito.IniPath)
	fmt.Printf("\nVerificato: php carica l'estensione.\n")
	if esito.Nota != "" {
		fmt.Printf("Passo successivo: %s\n", esito.Nota)
	}
	return nil
}

// doAbilita accende o spegne la raccolta senza disinstallare nulla: utile per
// sospenderla durante un intervento, o per riattivarla dopo.
func doAbilita(opts options, attiva bool) error {
	iniPath, err := install.TrovaIni(opts.phpBin)
	if err != nil {
		return err
	}
	if iniPath == "" {
		return errors.New("estensione non installata: usa \"orma --init\"")
	}

	prima, err := install.Attiva(iniPath)
	if err != nil {
		return err
	}
	if prima == attiva {
		fmt.Printf("La raccolta era gia' %s (%s).\n", statoTesto(attiva), iniPath)
		return nil
	}

	if err := install.Abilita(iniPath, attiva); err != nil {
		return err
	}

	fmt.Printf("Raccolta %s in %s\n", statoTesto(attiva), iniPath)
	fmt.Printf("\nPerche' abbia effetto sulle richieste in corso serve ricaricare php-fpm:\n  %s\n",
		install.ComandoRicarica(opts.phpBin))
	if !attiva {
		fmt.Print("\nL'estensione resta caricata ma non registra nemmeno l'observer:\n" +
			"con la raccolta spenta il costo e' zero misurabile.\n")
	}
	return nil
}

func statoTesto(attiva bool) string {
	if attiva {
		return "attivata"
	}
	return "sospesa"
}

// doExport genera le pagine come file, per quando al pannello non si puo'
// arrivare: niente proxy da configurare, niente porta da esporre.
func doExport(opts options) error {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.Database, nil, cfg.Retention())
	if err != nil {
		return err
	}
	defer st.Close()

	minuti := opts.minuti
	if minuti <= 0 {
		minuti = 1440
	}

	srv, err := web.New(st, newLogger(cfg.LogLevel), web.Opzioni{
		Addr:     cfg.Listen,
		ApdexTMS: float64(cfg.ApdexTMS),
	})
	if err != nil {
		return err
	}

	scritti, err := srv.Esporta(opts.exportDir, minuti)
	if err != nil {
		return err
	}

	fmt.Printf("Scritti %d file in %s (ultimi %d minuti)\n", len(scritti), opts.exportDir, minuti)
	fmt.Printf("Apri %s\n", filepath.Join(opts.exportDir, "panoramica.html"))
	return nil
}

func generaToken() (string, error) {
	grezzo := make([]byte, 24)
	if _, err := rand.Read(grezzo); err != nil {
		return "", fmt.Errorf("generazione del token: %w", err)
	}
	return hex.EncodeToString(grezzo), nil
}

// doPurge disinstalla tutto. Rifiuta di procedere con il daemon in esecuzione:
// rimuovere il database sotto un processo che ci sta scrivendo lascerebbe uno
// stato peggiore di quello di partenza.
func doPurge(opts options) error {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		// Configurazione illeggibile: si procede con i default, perche' lo
		// scopo di --purge e' proprio ripulire una situazione rotta.
		cfg = config.Default()
	}

	if pid, err := pidfile.Read(cfg.PidFile); err == nil && pidfile.Running(pid) {
		return fmt.Errorf("orma e' in esecuzione con pid %d: fermalo prima con \"orma stop\"", pid)
	}

	fmt.Println("Rimozione in corso. Verranno cancellati i dati raccolti.")

	var problemi []string

	if !opts.senzaEstensione {
		rimossi, err := install.Rimuovi(opts.phpBin)
		for _, r := range rimossi {
			fmt.Printf("  rimosso  %s\n", r)
		}
		if err != nil {
			problemi = append(problemi, err.Error())
		}
	}

	for _, percorso := range []string{
		cfg.Database, cfg.Database + "-wal", cfg.Database + "-shm",
		cfg.Socket, cfg.PidFile, opts.configPath,
	} {
		if err := os.Remove(percorso); err != nil {
			if !os.IsNotExist(err) {
				problemi = append(problemi, err.Error())
			}
			continue
		}
		fmt.Printf("  rimosso  %s\n", percorso)
	}

	// Le directory si rimuovono solo se sono rimaste vuote: potrebbero
	// contenere roba di qualcun altro.
	for _, dir := range []string{filepath.Dir(cfg.Database), filepath.Dir(cfg.Socket), filepath.Dir(opts.configPath)} {
		if err := os.Remove(dir); err == nil {
			fmt.Printf("  rimossa  %s\n", dir)
		}
	}

	if len(problemi) > 0 {
		return fmt.Errorf("rimozione incompleta:\n  %s", strings.Join(problemi, "\n  "))
	}

	fmt.Println("\nFatto. Riavvia php-fpm perche' l'estensione smetta di essere caricata.")
	return nil
}

func doCheckConfig(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		fmt.Printf("%s non esiste: verrebbero usati i default.\n", path)
	}
	fmt.Printf("Configurazione valida.\n")
	fmt.Printf("  socket    %s", cfg.Socket)
	if cfg.SocketGroup != "" {
		fmt.Printf(" (gruppo %s)", cfg.SocketGroup)
	} else {
		fmt.Printf(" (aperto a tutti: valuta socket_group)")
	}
	fmt.Println()
	fmt.Printf("  pid_file  %s\n", cfg.PidFile)
	fmt.Printf("  database  %s\n", cfg.Database)
	fmt.Printf("  listen    %s\n", cfg.Listen)
	fmt.Printf("  log_level %s\n", cfg.LogLevel)
	return nil
}

func doStart(cfg config.Config, configPath string) error {
	log := newLogger(cfg.LogLevel)

	pf, err := pidfile.Acquire(cfg.PidFile)
	if err != nil {
		return err
	}
	defer pf.Release()

	st, err := store.Open(cfg.Database, log, cfg.Retention())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aggregator := agg.New(st, log, agg.Options{
		MaxNames:          cfg.MaxTxnNames,
		TraceThresholdNS:  uint64(cfg.TraceThresholdMS) * 1e6,
		TraceMaxPerWindow: cfg.TraceMaxPerMin,
		SlowestNames:      cfg.TraceSlowestNames,
	})
	aggDone := make(chan struct{})
	go func() {
		defer close(aggDone)
		aggregator.Run(ctx)
	}()

	manutDone := make(chan struct{})

	ln := ingest.New(cfg.Socket, cfg.SocketGroup, log, func(txn *protocol.Transaction) {
		aggregator.Add(txn)
		log.Debug("transazione",
			"app", txn.App,
			"nome", txn.Name,
			"durata_ms", float64(txn.DurationNano)/1e6,
			"stato", txn.HTTPStatus,
			"memoria", txn.PeakMemory,
			"background", txn.Background,
			"span", len(txn.Spans))
	})
	if err := ln.Open(); err != nil {
		return err
	}
	defer ln.Close()

	avvio := time.Now()
	srv, err := web.New(st, log, web.Opzioni{
		Addr:     cfg.Listen,
		ApdexTMS: float64(cfg.ApdexTMS),
		Token:    cfg.UIToken,
		Stato: func() web.Stato {
			frames, bytes, rifiutati := ln.Stats()
			s := aggregator.Stats()
			return web.Stato{
				Versione:        version.Version,
				Avvio:           avvio,
				Socket:          cfg.Socket,
				SocketGruppo:    cfg.SocketGroup,
				Database:        cfg.Database,
				DimensioneDB:    st.Dimensione(cfg.Database),
				FrameRicevuti:   frames,
				ByteRicevuti:    bytes,
				FrameRifiutati:  rifiutati,
				AgentPerse:      s.AgentPerse,
				FinestreScritte: s.FinestreScritte,
				FinestrePerse:   s.FinestrePerse,
				FinestreAperte:  s.FinestreAperte,
			}
		},
	})
	if err != nil {
		return err
	}

	if cfg.UIToken == "" {
		log.Warn("interfaccia senza autenticazione: imposta ui_token, "+
			"perche' le pagine espongono query, host contattati e messaggi d'errore",
			"listen", cfg.Listen)
	}

	valutatore := allarmi.Nuovo(st, log, cfg.Regole(), float64(cfg.ApdexTMS))

	go func() {
		defer close(manutDone)
		manutenzione(ctx, st, cfg.Retention(), aggregator, valutatore, log)
	}()

	served := make(chan error, 1)
	go func() { served <- ln.Serve(ctx) }()

	webErr := make(chan error, 1)
	go func() { webErr <- srv.Serve(ctx) }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	log.Info("orma avviato",
		"versione", version.Version, "pid", os.Getpid(),
		"socket", cfg.Socket, "ui", cfg.Listen, "database", cfg.Database)

	// stop riversa le metriche ancora in memoria prima che il database venga
	// chiuso dal defer: l'ultimo minuto non si perde.
	stop := func(cause error) error {
		cancel()
		frames, bytes, rejected := ln.Stats()
		log.Info("ingestione", "frame", frames, "byte", bytes, "scartati", rejected)
		<-aggDone
		<-manutDone
		if cause != nil {
			return cause
		}
		return <-served
	}

	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				if _, err := config.Load(configPath); err != nil {
					log.Error("ricarica fallita, resto sulla configurazione precedente", "errore", err)
					continue
				}
				// M2+: applicare a caldo cio' che si puo'. Socket, pid_file e
				// database no: quelli richiedono un riavvio, e va detto invece
				// di fingere.
				log.Info("configurazione ricaricata")
				continue
			}

			log.Info("arresto in corso", "segnale", sig.String())
			return stop(nil)

		case err := <-served:
			return stop(err)

		case err := <-webErr:
			if err != nil {
				return stop(fmt.Errorf("interfaccia web: %w", err))
			}
		}
	}
}

// manutenzione tiene il database a dimensione stabile: aggrega le metriche
// verso granularita' piu' grosse e rimuove cio' che e' scaduto.
//
// Un errore non ferma il daemon: la raccolta e' piu' importante della
// manutenzione, e il giro successivo riprova.
func manutenzione(ctx context.Context, st *store.Store, r store.Retention,
	aggregatore *agg.Aggregator, valutatore *allarmi.Valutatore, log *slog.Logger) {

	rollup := time.NewTicker(time.Minute)
	defer rollup.Stop()
	purga := time.NewTicker(10 * time.Minute)
	defer purga.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-rollup.C:
			if err := st.Rollup(log); err != nil {
				log.Error("rollup fallito", "errore", err)
			}
			valutatore.Valuta(aggregatore.Stats())
		case <-purga.C:
			if err := st.Purge(r, log); err != nil {
				log.Error("purga fallita", "errore", err)
			}
		}
	}
}

func doStop(cfg config.Config) error {
	pid, err := pidfile.Signal(cfg.PidFile, syscall.SIGTERM)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidfile.Running(pid) {
			fmt.Printf("orma fermato (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("il pid %d non e' uscito entro 10 secondi", pid)
}

func doReload(cfg config.Config) error {
	pid, err := pidfile.Signal(cfg.PidFile, syscall.SIGHUP)
	if err != nil {
		return err
	}
	fmt.Printf("configurazione ricaricata (pid %d)\n", pid)
	return nil
}

func doRestart(cfg config.Config, configPath string) error {
	if err := doStop(cfg); err != nil && !errors.Is(err, pidfile.ErrNotRunning) {
		return err
	}
	pid, err := proc.SpawnDetached("start", "--config", configPath)
	if err != nil {
		return err
	}
	fmt.Printf("orma avviato (pid %d)\n", pid)
	return nil
}

func doStatus(cfg config.Config) error {
	pid, err := pidfile.Read(cfg.PidFile)
	if err != nil {
		return err
	}
	if !pidfile.Running(pid) {
		return fmt.Errorf("il pidfile indica %d ma il processo non esiste", pid)
	}
	fmt.Printf("orma in esecuzione (pid %d)\n", pid)
	fmt.Printf("  socket   %s\n", cfg.Socket)
	fmt.Printf("  database %s\n", cfg.Database)
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
