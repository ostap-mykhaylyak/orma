package protocol

import (
	"encoding/binary"
	"testing"
)

// frameBuilder costruisce frame nello stesso formato di ext/orma_proto.c.
// Se questo helper e il file C divergono, i test passano e la produzione no:
// e' per questo che la prova end-to-end in test/smoke.sh resta necessaria
// anche avendo questi test.
type frameBuilder struct{ b []byte }

func (f *frameBuilder) u8(v uint8)  { f.b = append(f.b, v) }
func (f *frameBuilder) u16(v uint16) { f.b = binary.LittleEndian.AppendUint16(f.b, v) }
func (f *frameBuilder) u32(v uint32) { f.b = binary.LittleEndian.AppendUint32(f.b, v) }
func (f *frameBuilder) u64(v uint64) { f.b = binary.LittleEndian.AppendUint64(f.b, v) }
func (f *frameBuilder) raw(n int)    { f.b = append(f.b, make([]byte, n)...) }

func validFrame() []byte {
	f := &frameBuilder{}

	f.u8(Version)
	f.u8(0)

	strs := []string{"", "prova", "srv1", "/prodotti/{id}", "http.request.method", "GET", "php.memory.peak_bytes"}
	f.u32(uint32(len(strs)))
	for _, s := range strs {
		f.u16(uint16(len(s)))
		f.b = append(f.b, s...)
	}

	f.u32(1) // app
	f.u32(2) // host
	f.u32(3) // nome
	f.u32(4242)
	f.u8(0)   // non background
	f.u16(200)
	f.u64(1700000000000000000)
	f.u64(1500000) // 1.5 ms
	f.u64(2097152)
	f.u64(1000)
	f.u64(2000)
	f.u32(0)
	f.u32(0)

	f.u32(1) // uno span
	f.raw(16)
	f.raw(8)
	f.raw(8)
	f.u32(3)
	f.u8(uint8(KindServer))
	f.u64(1700000000000000000)
	f.u64(1500000)
	f.u8(0)
	f.u16(2)
	f.u32(4)
	f.u8(uint8(AttrString))
	f.u32(5)
	f.u32(6)
	f.u8(uint8(AttrInt64))
	f.u64(2097152)

	return f.b
}

func TestDecodeValido(t *testing.T) {
	txn, err := Decode(validFrame())
	if err != nil {
		t.Fatalf("decodifica fallita: %v", err)
	}

	if txn.App != "prova" || txn.Host != "srv1" || txn.Name != "/prodotti/{id}" {
		t.Errorf("intestazione errata: app=%q host=%q nome=%q", txn.App, txn.Host, txn.Name)
	}
	if txn.HTTPStatus != 200 || txn.DurationNano != 1500000 || txn.Background {
		t.Errorf("campi errati: stato=%d durata=%d background=%v",
			txn.HTTPStatus, txn.DurationNano, txn.Background)
	}
	if len(txn.Spans) != 1 {
		t.Fatalf("attesi 1 span, trovati %d", len(txn.Spans))
	}

	span := txn.Spans[0]
	if span.Kind != KindServer || len(span.Attrs) != 2 {
		t.Fatalf("span errato: kind=%v attributi=%d", span.Kind, len(span.Attrs))
	}
	if span.Attrs[0].Key != "http.request.method" || span.Attrs[0].Str != "GET" {
		t.Errorf("attributo stringa errato: %+v", span.Attrs[0])
	}
	if span.Attrs[1].Type != AttrInt64 || span.Attrs[1].Int != 2097152 {
		t.Errorf("attributo intero errato: %+v", span.Attrs[1])
	}
}

// Il decoder legge input non fidato: qualunque troncamento deve produrre un
// errore, mai un panico e mai una transazione a meta'.
func TestDecodeTroncatoNonVaInPanico(t *testing.T) {
	full := validFrame()
	for n := 0; n < len(full); n++ {
		n := n
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panico decodificando %d byte su %d: %v", n, len(full), r)
				}
			}()
			if _, err := Decode(full[:n]); err == nil {
				t.Fatalf("troncato a %d byte ma decodificato senza errore", n)
			}
		}()
	}
}

func TestDecodeVersioneSbagliata(t *testing.T) {
	frame := validFrame()
	frame[0] = 99
	if _, err := Decode(frame); err == nil {
		t.Fatal("versione 99 accettata")
	}
}

// Un conteggio enorme non deve far allocare: va respinto confrontandolo con i
// byte effettivamente disponibili.
func TestDecodeConteggiAssurdi(t *testing.T) {
	casi := map[string]func([]byte){
		"stringhe": func(b []byte) { binary.LittleEndian.PutUint32(b[2:], 0xFFFFFF) },
		"span": func(b []byte) {
			// Il conteggio degli span sta dopo tabella e transazione: si
			// azzera tutto il resto e si dichiara un numero impossibile.
			for i := len(b) - 4; i >= 0; i-- {
				if binary.LittleEndian.Uint32(b[i:]) == 1 {
					binary.LittleEndian.PutUint32(b[i:], 0xFFFFFF)
					return
				}
			}
		},
	}

	for nome, guasta := range casi {
		t.Run(nome, func(t *testing.T) {
			frame := validFrame()
			guasta(frame)
			if _, err := Decode(frame); err == nil {
				t.Fatal("conteggio assurdo accettato")
			}
		})
	}
}

func TestDecodeIndiceStringaFuoriTabella(t *testing.T) {
	f := &frameBuilder{}
	f.u8(Version)
	f.u8(0)
	f.u32(1)
	f.u16(0) // una sola stringa, vuota
	f.u32(99) // app_idx fuori tabella
	f.u32(0)
	f.u32(0)
	f.u32(1)
	f.u8(0)
	f.u16(0)
	f.u64(0)
	f.u64(0)
	f.u64(0)
	f.u64(0)
	f.u64(0)
	f.u32(0)
	f.u32(0)
	f.u32(0)

	if _, err := Decode(f.b); err == nil {
		t.Fatal("indice di stringa fuori tabella accettato")
	}
}
