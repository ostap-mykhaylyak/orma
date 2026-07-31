// Package metrics contiene l'istogramma usato per stimare i percentili.
//
// Tenere i campioni per calcolare i percentili esatti costerebbe memoria
// proporzionale al traffico. Un istogramma a bucket logaritmici costa invece
// una dimensione fissa per chiave, al prezzo di un errore relativo limitato
// dalla base scelta.
package metrics

import (
	"encoding/binary"
	"math"
)

const (
	// Buckets e' il numero di bucket per istogramma: 512 byte una volta serializzato.
	Buckets = 128

	// base e' il rapporto fra due bucket contigui, e quindi l'errore relativo
	// massimo della stima: 15%.
	base = 1.15

	// unitNS e' il valore del primo bucket: 0.1 ms.
	unitNS = 100000.0
)

var logBase = math.Log(base)

// Histogram e' un istogramma a dimensione fissa.
type Histogram [Buckets]uint32

// Bucket restituisce l'indice del bucket per una durata in nanosecondi.
func Bucket(durationNS uint64) int {
	v := float64(durationNS) / unitNS
	if v < 1 {
		return 0
	}
	idx := int(math.Log(v)/logBase) + 1
	if idx >= Buckets {
		return Buckets - 1
	}
	return idx
}

// Add registra un campione.
func (h *Histogram) Add(durationNS uint64) {
	h[Bucket(durationNS)]++
}

// Merge somma src dentro h.
func (h *Histogram) Merge(src *Histogram) {
	for i := range h {
		h[i] += src[i]
	}
}

// bucketValueMS e' il valore rappresentativo di un bucket, in millisecondi.
func bucketValueMS(idx int) float64 {
	if idx <= 0 {
		return 0
	}
	return (unitNS * math.Pow(base, float64(idx-1))) / 1e6
}

// Percentile stima il percentile richiesto (0..1) in millisecondi.
func (h *Histogram) Percentile(p float64) float64 {
	var total uint64
	for _, c := range h {
		total += uint64(c)
	}
	if total == 0 {
		return 0
	}

	target := uint64(math.Ceil(p * float64(total)))
	if target == 0 {
		target = 1
	}

	var seen uint64
	for i, c := range h {
		seen += uint64(c)
		if seen >= target {
			return bucketValueMS(i)
		}
	}
	return bucketValueMS(Buckets - 1)
}

// Apdex misura la soddisfazione rispetto a una soglia T in millisecondi:
// le richieste entro T contano intere, quelle entro 4T contano meta', le altre
// non contano. Un istogramma vuoto vale 1: nessuna richiesta, nessun scontento.
//
// Si calcola dall'istogramma, quindi eredita il suo errore di quantizzazione:
// una richiesta a cavallo di T puo' finire dalla parte sbagliata.
func (h *Histogram) Apdex(tMS float64) float64 {
	if tMS <= 0 {
		return 1
	}

	var total, satisfied, tolerating uint64
	for i, c := range h {
		if c == 0 {
			continue
		}
		total += uint64(c)
		switch v := bucketValueMS(i); {
		case v <= tMS:
			satisfied += uint64(c)
		case v <= 4*tMS:
			tolerating += uint64(c)
		}
	}

	if total == 0 {
		return 1
	}
	return (float64(satisfied) + float64(tolerating)/2) / float64(total)
}

// Encode serializza l'istogramma per lo storage.
func (h *Histogram) Encode() []byte {
	out := make([]byte, Buckets*4)
	for i, c := range h {
		binary.LittleEndian.PutUint32(out[i*4:], c)
	}
	return out
}

// Decode legge un istogramma serializzato. Un blob di dimensione sbagliata
// produce un istogramma vuoto invece di un errore: una riga di metriche
// corrotta non deve impedire di mostrare tutte le altre.
func Decode(raw []byte) Histogram {
	var h Histogram
	if len(raw) != Buckets*4 {
		return h
	}
	for i := range h {
		h[i] = binary.LittleEndian.Uint32(raw[i*4:])
	}
	return h
}
