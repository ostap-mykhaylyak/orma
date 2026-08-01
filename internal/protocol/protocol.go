// Package protocol decodifica i frame prodotti dall'estensione PHP.
//
// Il formato e' descritto in DESIGN.md §3 ed e' implementato lato agent in
// ext/orma_proto.c. Le due implementazioni vanno tenute allineate a mano: se
// cambia una, cambia l'altra.
//
// Tutto cio' che arriva qui e' input non fidato. Il decoder non deve mai andare
// in panico ne' allocare in base a un numero dichiarato dal mittente senza
// prima verificarlo contro i byte effettivamente disponibili.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Version e' la versione del protocollo supportata. La 2 aggiunge i warning e
// gli eventi; la 3 i frame che l'agent non ha consegnato; la 4 separa quei
// frame per causa e aggiunge il conteggio delle chiamate e il profilo delle
// funzioni interne.
const Version = 4

// Limiti di sanita': un mittente corretto non li raggiunge mai.
const (
	maxStrings = 4096
	maxSpans   = 4096
	maxAttrs   = 256
	maxEvents  = 256
	maxProfilo = 256
)

// Severity distingue cio' che rompe una richiesta da cio' che la sporca.
type Severity uint8

const (
	SeveritaAvviso Severity = 0
	SeveritaErrore Severity = 1
)

// Event e' un errore PHP o un'eccezione lanciata.
type Event struct {
	Class    string
	Message  string
	File     string
	Line     uint32
	Severity Severity
	UnixNano uint64
}

// SpanKind segue la classificazione di OpenTelemetry.
type SpanKind uint8

const (
	KindInternal SpanKind = 0
	KindServer   SpanKind = 1
	KindClient   SpanKind = 2
)

func (k SpanKind) String() string {
	switch k {
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	default:
		return "internal"
	}
}

// AttrType e' il tipo di un attributo di span.
type AttrType uint8

const (
	AttrString AttrType = 0
	AttrInt64  AttrType = 1
	AttrDouble AttrType = 2
	AttrBool   AttrType = 3
)

// Attr e' un attributo di span, con chiavi che seguono le semantic conventions
// di OpenTelemetry.
type Attr struct {
	Key    string
	Type   AttrType
	Str    string
	Int    int64
	Double float64
	Bool   bool
}

// Value restituisce il valore nel tipo dichiarato, per il logging.
func (a Attr) Value() any {
	switch a.Type {
	case AttrString:
		return a.Str
	case AttrInt64:
		return a.Int
	case AttrDouble:
		return a.Double
	case AttrBool:
		return a.Bool
	}
	return nil
}

// Span e' un intervallo di tempo attribuito a un'operazione.
type Span struct {
	TraceID       [16]byte
	SpanID        [8]byte
	ParentSpanID  [8]byte
	Name          string
	Kind          SpanKind
	StartUnixNano uint64
	DurationNano  uint64
	Status        uint8
	// Chiamate avvenute dentro questo span, annidate comprese. Distingue uno
	// span lento perche' fa moltissimo da uno lento perche' aspetta.
	Chiamate uint32
	Attrs    []Attr
}

// Transaction e' una richiesta completa: lo span radice piu' i metadati di
// processo che non appartengono a nessuno span in particolare.
type Transaction struct {
	App           string
	Host          string
	Name          string
	PID           uint32
	Background    bool
	HTTPStatus    uint16
	StartUnixNano uint64
	DurationNano  uint64
	PeakMemory    uint64
	CPUUserNano   uint64
	CPUSysNano    uint64
	// Errors conta i soli eventi fatali: e' su questo che si decide se la
	// transazione e' andata male. I warning stanno a parte, perche' un sito
	// pieno di deprecation non e' un sito rotto.
	Errors       uint32
	Warnings     uint32
	SpansDropped uint32
	// Perse dice quante transazioni l'agent non ha consegnato dall'ultima
	// consegna riuscita, e per quale causa. E' l'unico modo che il daemon ha
	// per sapere di essere cieco su una parte del traffico, e sapere la causa
	// e' cio' che distingue "daemon fermo" da "macchina carica".
	Perse PerseAgent
	// Chiamate e' il numero di chiamate di funzione utente della richiesta,
	// comprese quelle rimaste sotto soglia.
	Chiamate uint32
	Spans    []Span
	Events   []Event
	Profilo  []VoceProfilo
}

// PerseAgent sono le transazioni non consegnate, per causa.
type PerseAgent struct {
	Connessione uint32
	Timeout     uint32
	Scrittura   uint32
}

// Totale e' la somma delle tre cause.
func (p PerseAgent) Totale() uint32 {
	return p.Connessione + p.Timeout + p.Scrittura
}

// VoceProfilo e' il costo complessivo di una funzione interna nella richiesta.
type VoceProfilo struct {
	Funzione string
	Chiamate uint32
	Nano     uint64
}

// ErrVersion indica un frame prodotto da una versione di protocollo diversa.
var ErrVersion = errors.New("versione di protocollo non supportata")

// reader e' un cursore con controllo dei limiti: al primo errore si blocca e
// tutte le letture successive diventano no-op, cosi' il chiamante puo'
// verificare una volta sola alla fine.
type reader struct {
	buf []byte
	pos int
	err error
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.pos+n > len(r.buf) {
		r.fail("frame troncato: servivano %d byte alla posizione %d, disponibili %d", n, r.pos, len(r.buf)-r.pos)
		return nil
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b
}

func (r *reader) u8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// remaining e' quanto resta da leggere: serve a validare i conteggi dichiarati
// prima di allocare in base a essi.
func (r *reader) remaining() int {
	if r.err != nil {
		return 0
	}
	return len(r.buf) - r.pos
}

// Decode interpreta un frame, che inizia con il byte di versione: il campo di
// lunghezza e' gia' stato consumato da chi ha letto dal socket.
func Decode(frame []byte) (*Transaction, error) {
	r := &reader{buf: frame}

	version := r.u8()
	if r.err != nil {
		return nil, r.err
	}
	if version != Version {
		return nil, fmt.Errorf("%w: ricevuta %d, attesa %d", ErrVersion, version, Version)
	}
	_ = r.u8() // flags di frame, non ancora usati

	strings, err := decodeStringTable(r)
	if err != nil {
		return nil, err
	}

	lookup := func(idx uint32) string {
		if int(idx) >= len(strings) {
			r.fail("indice di stringa fuori tabella: %d su %d", idx, len(strings))
			return ""
		}
		return strings[idx]
	}

	txn := &Transaction{}
	txn.App = lookup(r.u32())
	txn.Host = lookup(r.u32())
	txn.Name = lookup(r.u32())
	txn.PID = r.u32()
	txn.Background = r.u8()&1 != 0
	txn.HTTPStatus = r.u16()
	txn.StartUnixNano = r.u64()
	txn.DurationNano = r.u64()
	txn.PeakMemory = r.u64()
	txn.CPUUserNano = r.u64()
	txn.CPUSysNano = r.u64()
	txn.Errors = r.u32()
	txn.Warnings = r.u32()
	txn.SpansDropped = r.u32()
	txn.Perse.Connessione = r.u32()
	txn.Perse.Timeout = r.u32()
	txn.Perse.Scrittura = r.u32()
	if c := r.u64(); c > math.MaxUint32 {
		txn.Chiamate = math.MaxUint32
	} else {
		txn.Chiamate = uint32(c)
	}

	spanCount := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if spanCount > maxSpans {
		return nil, fmt.Errorf("troppi span dichiarati: %d, massimo %d", spanCount, maxSpans)
	}
	// Ogni span occupa almeno 50 byte: se il conteggio dichiarato non ci sta
	// nel frame, il frame e' malformato e non si alloca nulla.
	if int(spanCount)*50 > r.remaining() {
		return nil, fmt.Errorf("dichiarati %d span ma restano %d byte", spanCount, r.remaining())
	}

	txn.Spans = make([]Span, 0, spanCount)
	for i := uint32(0); i < spanCount; i++ {
		span, err := decodeSpan(r, lookup)
		if err != nil {
			return nil, err
		}
		txn.Spans = append(txn.Spans, span)
	}

	events, err := decodeEvents(r, lookup)
	if err != nil {
		return nil, err
	}
	txn.Events = events

	profilo, err := decodeProfilo(r, lookup)
	if err != nil {
		return nil, err
	}
	txn.Profilo = profilo

	if r.err != nil {
		return nil, r.err
	}
	return txn, nil
}

func decodeProfilo(r *reader, lookup func(uint32) string) ([]VoceProfilo, error) {
	count := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if count > maxProfilo {
		return nil, fmt.Errorf("troppe voci di profilo dichiarate: %d, massimo %d", count, maxProfilo)
	}
	// Ogni voce occupa 16 byte.
	if int(count)*16 > r.remaining() {
		return nil, fmt.Errorf("dichiarate %d voci di profilo ma restano %d byte", count, r.remaining())
	}

	out := make([]VoceProfilo, 0, count)
	for i := uint32(0); i < count; i++ {
		v := VoceProfilo{
			Funzione: lookup(r.u32()),
			Chiamate: r.u32(),
			Nano:     r.u64(),
		}
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeEvents(r *reader, lookup func(uint32) string) ([]Event, error) {
	count := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if count > maxEvents {
		return nil, fmt.Errorf("troppi eventi dichiarati: %d, massimo %d", count, maxEvents)
	}
	// Ogni evento occupa almeno 25 byte.
	if int(count)*25 > r.remaining() {
		return nil, fmt.Errorf("dichiarati %d eventi ma restano %d byte", count, r.remaining())
	}

	out := make([]Event, 0, count)
	for i := uint32(0); i < count; i++ {
		var e Event
		e.Class = lookup(r.u32())
		e.Message = lookup(r.u32())
		e.File = lookup(r.u32())
		e.Line = r.u32()
		e.Severity = Severity(r.u8())
		e.UnixNano = r.u64()
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, e)
	}
	return out, nil
}

func decodeStringTable(r *reader) ([]string, error) {
	count := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if count > maxStrings {
		return nil, fmt.Errorf("troppe stringhe dichiarate: %d, massimo %d", count, maxStrings)
	}
	// Ogni voce occupa almeno i 2 byte di lunghezza.
	if int(count)*2 > r.remaining() {
		return nil, fmt.Errorf("dichiarate %d stringhe ma restano %d byte", count, r.remaining())
	}

	out := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		n := r.u16()
		b := r.take(int(n))
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, string(b))
	}
	return out, nil
}

func decodeSpan(r *reader, lookup func(uint32) string) (Span, error) {
	var s Span

	copy(s.TraceID[:], r.take(16))
	copy(s.SpanID[:], r.take(8))
	copy(s.ParentSpanID[:], r.take(8))
	s.Name = lookup(r.u32())
	s.Kind = SpanKind(r.u8())
	s.StartUnixNano = r.u64()
	s.DurationNano = r.u64()
	s.Status = r.u8()
	s.Chiamate = r.u32()

	attrCount := r.u16()
	if r.err != nil {
		return s, r.err
	}
	if attrCount > maxAttrs {
		return s, fmt.Errorf("troppi attributi dichiarati: %d, massimo %d", attrCount, maxAttrs)
	}
	if int(attrCount)*5 > r.remaining() {
		return s, fmt.Errorf("dichiarati %d attributi ma restano %d byte", attrCount, r.remaining())
	}

	s.Attrs = make([]Attr, 0, attrCount)
	for i := uint16(0); i < attrCount; i++ {
		var a Attr
		a.Key = lookup(r.u32())
		a.Type = AttrType(r.u8())

		switch a.Type {
		case AttrString:
			a.Str = lookup(r.u32())
		case AttrInt64:
			a.Int = int64(r.u64())
		case AttrDouble:
			a.Double = math.Float64frombits(r.u64())
		case AttrBool:
			a.Bool = r.u8() != 0
		default:
			return s, fmt.Errorf("tipo di attributo sconosciuto: %d", a.Type)
		}

		if r.err != nil {
			return s, r.err
		}
		s.Attrs = append(s.Attrs, a)
	}

	return s, r.err
}
