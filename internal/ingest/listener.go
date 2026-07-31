// Package ingest riceve i payload dell'estensione PHP sul socket unix.
//
// Il formato del frame e' quello descritto in DESIGN.md §3:
//
//	u32 frame_len   numero di byte che seguono questo campo
//	u8  version     versione del protocollo
//	u8  flags
//	...             payload (string table, transaction, span)
//
// Al M0 i frame vengono letti, validati e scartati: serve a fissare il framing,
// cosi' che il M1 debba aggiungere solo il decoder.
package ingest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/protocol"
)

const (
	// ProtocolVersion e' la versione del protocollo agent to daemon.
	ProtocolVersion = protocol.Version

	// MaxFrameSize limita quanto un singolo frame puo' far allocare al daemon.
	// Un client che mente sulla lunghezza non deve poter esaurire la memoria.
	MaxFrameSize = 4 << 20

	headerSize = 2 // version + flags
)

// Handler riceve ogni transazione decodificata. Viene invocato dalla goroutine
// della connessione, quindi deve essere veloce e non bloccare: se ha lavoro da
// fare, lo accodi.
type Handler func(*protocol.Transaction)

// Listener accetta connessioni dai worker PHP sul socket unix.
type Listener struct {
	path    string
	group   string
	log     *slog.Logger
	handler Handler
	ln      net.Listener

	frames   atomic.Uint64
	bytes    atomic.Uint64
	rejected atomic.Uint64
}

// New prepara un listener sul percorso indicato. handler puo' essere nil, e in
// quel caso i frame vengono decodificati e scartati.
// group e' il gruppo unix a cui dare accesso in scrittura, tipicamente quello
// dei worker php-fpm. Vuoto significa socket aperto a tutti.
func New(path, group string, log *slog.Logger, handler Handler) *Listener {
	return &Listener{path: path, group: group, log: log, handler: handler}
}

// Open crea il socket. Un socket residuo di un'istanza morta viene rimosso, ma
// solo dopo aver verificato che nessuno stia ascoltando: se qualcuno risponde,
// c'e' un altro orma vivo e ci fermiamo.
func (l *Listener) Open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("creazione della directory del socket: %w", err)
	}

	if _, err := os.Stat(l.path); err == nil {
		if c, err := net.DialTimeout("unix", l.path, 200*time.Millisecond); err == nil {
			c.Close()
			return fmt.Errorf("%s e' gia' servito da un'altra istanza", l.path)
		}
		if err := os.Remove(l.path); err != nil {
			return fmt.Errorf("rimozione del socket residuo: %w", err)
		}
	}

	ln, err := net.Listen("unix", l.path)
	if err != nil {
		return fmt.Errorf("apertura di %s: %w", l.path, err)
	}

	l.ln = ln
	if err := l.permessi(); err != nil {
		ln.Close()
		os.Remove(l.path)
		l.ln = nil
		return err
	}
	return nil
}

// permessi apre il socket in scrittura ai worker php-fpm, che girano con un
// altro utente.
//
// Con un gruppo configurato il socket e' 0660 e solo quel gruppo scrive. Senza,
// resta 0666: funziona ovunque senza configurazione, ma qualunque utente locale
// puo' iniettare telemetria falsa. Il compromesso e' esplicito e lo si dice.
func (l *Listener) permessi() error {
	if l.group == "" {
		if err := os.Chmod(l.path, 0o666); err != nil {
			return fmt.Errorf("permessi del socket: %w", err)
		}
		l.log.Warn("socket accessibile a tutti gli utenti locali: "+
			"imposta socket_group per restringerlo al gruppo di php-fpm",
			"socket", l.path)
		return nil
	}

	gruppo, err := user.LookupGroup(l.group)
	if err != nil {
		return fmt.Errorf("gruppo %q non trovato: %w", l.group, err)
	}
	gid, err := strconv.Atoi(gruppo.Gid)
	if err != nil {
		return fmt.Errorf("gid non valido per il gruppo %q: %w", l.group, err)
	}

	if err := os.Chown(l.path, -1, gid); err != nil {
		return fmt.Errorf("assegnazione del socket al gruppo %q: %w", l.group, err)
	}
	if err := os.Chmod(l.path, 0o660); err != nil {
		return fmt.Errorf("permessi del socket: %w", err)
	}

	l.log.Info("socket riservato al gruppo", "gruppo", l.group, "socket", l.path)
	return nil
}

// Serve accetta connessioni finche' il contesto non viene annullato.
func (l *Listener) Serve(ctx context.Context) error {
	if l.ln == nil {
		return errors.New("Serve chiamata prima di Open")
	}

	go func() {
		<-ctx.Done()
		l.ln.Close()
	}()

	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go l.handle(conn)
	}
}

// Close chiude il socket e lo rimuove dal filesystem.
func (l *Listener) Close() error {
	if l.ln == nil {
		return nil
	}
	err := l.ln.Close()
	if rmErr := os.Remove(l.path); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
	}
	return err
}

// Stats riporta i contatori di ingestione.
func (l *Listener) Stats() (frames, bytes, rejected uint64) {
	return l.frames.Load(), l.bytes.Load(), l.rejected.Load()
}

// handle consuma i frame di una connessione. Un worker php-fpm tiene la
// connessione aperta e ci scrive un frame per richiesta.
func (l *Listener) handle(conn net.Conn) {
	defer conn.Close()

	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				l.log.Debug("connessione interrotta", "errore", err)
			}
			return
		}

		frameLen := binary.LittleEndian.Uint32(lenBuf[:])
		if frameLen < headerSize || frameLen > MaxFrameSize {
			l.rejected.Add(1)
			l.log.Warn("frame di lunghezza non plausibile, connessione chiusa",
				"lunghezza", frameLen, "massimo", MaxFrameSize)
			return
		}

		frame := make([]byte, frameLen)
		if _, err := io.ReadFull(conn, frame); err != nil {
			l.rejected.Add(1)
			l.log.Debug("frame troncato", "errore", err)
			return
		}

		txn, err := protocol.Decode(frame)
		if err != nil {
			// Un frame malformato desincronizza lo stream: non si puo'
			// riprendere a leggere da meta' payload, si chiude e basta.
			l.rejected.Add(1)
			if errors.Is(err, protocol.ErrVersion) {
				l.log.Warn("versione di protocollo non supportata, connessione chiusa", "errore", err)
			} else {
				l.log.Warn("frame malformato, connessione chiusa", "errore", err)
			}
			return
		}

		l.frames.Add(1)
		l.bytes.Add(uint64(frameLen) + 4)

		if l.handler != nil {
			l.handler(txn)
		}
	}
}
