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
	"path/filepath"
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
	log     *slog.Logger
	handler Handler
	ln      net.Listener

	frames   atomic.Uint64
	bytes    atomic.Uint64
	rejected atomic.Uint64
}

// New prepara un listener sul percorso indicato. handler puo' essere nil, e in
// quel caso i frame vengono decodificati e scartati.
func New(path string, log *slog.Logger, handler Handler) *Listener {
	return &Listener{path: path, log: log, handler: handler}
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

	// I worker php-fpm girano con un altro utente e devono poter scrivere.
	// M7: restringere a 0660 con un gruppo dedicato invece di aprire a tutti.
	if err := os.Chmod(l.path, 0o666); err != nil {
		ln.Close()
		return fmt.Errorf("permessi del socket: %w", err)
	}

	l.ln = ln
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
