// Comando orma: daemon di ingestione APM e CLI di servizio.
//
// Convenzione della CLI: i verbi di servizio si scrivono senza trattini
// (start, stop, reload, restart, status), tutto il resto obbligatoriamente con
// i trattini (--init, --check-config, --version).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/agg"
	"github.com/ostap-mykhaylyak/orma/internal/config"
	"github.com/ostap-mykhaylyak/orma/internal/ingest"
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
  orma --init             genera la configurazione iniziale
  orma --check-config     valida la configurazione ed esce
  orma --version          stampa la versione
  orma --help             stampa questo messaggio

Opzioni:
  --config <percorso>     configurazione da usare (default: ` + config.DefaultPath + `)
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
	configPath string
	verb       string
	action     string
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
		return doInit(opts.configPath)
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

		case arg == "--init", arg == "--check-config", arg == "--version", arg == "--help":
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

func doInit(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s esiste gia': rimuovilo se vuoi rigenerarlo", path)
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creazione di %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(config.Template), 0o644); err != nil {
		return fmt.Errorf("scrittura di %s: %w", path, err)
	}

	cfg := config.Default()
	for _, dir := range []string{filepath.Dir(cfg.Socket), filepath.Dir(cfg.Database)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creazione di %s: %w", dir, err)
		}
	}

	fmt.Printf("Scritto %s\n", path)
	fmt.Printf("Create   %s, %s\n", filepath.Dir(cfg.Socket), filepath.Dir(cfg.Database))
	fmt.Print("\nManca l'estensione PHP: copia orma.so in extension_dir e aggiungi a conf.d\n\n")
	fmt.Printf("  extension=orma.so\n  orma.app_name=nome-del-sito\n  orma.socket=%s\n\n", cfg.Socket)
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
	fmt.Printf("  socket    %s\n", cfg.Socket)
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

	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aggregator := agg.New(st, log, 0)
	aggDone := make(chan struct{})
	go func() {
		defer close(aggDone)
		aggregator.Run(ctx)
	}()

	ln := ingest.New(cfg.Socket, log, func(txn *protocol.Transaction) {
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

	srv, err := web.New(cfg.Listen, st, log)
	if err != nil {
		return err
	}

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
