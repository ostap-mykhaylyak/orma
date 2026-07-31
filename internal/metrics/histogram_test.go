package metrics

import (
	"math"
	"testing"
)

// L'errore relativo della stima e' limitato dalla base dei bucket.
const erroreMassimo = base - 1

func TestPercentileSuValoreCostante(t *testing.T) {
	casi := []float64{1, 10, 100, 1000, 10000} // millisecondi

	for _, ms := range casi {
		var h Histogram
		for i := 0; i < 1000; i++ {
			h.Add(uint64(ms * 1e6))
		}

		for _, p := range []float64{0.5, 0.95, 0.99} {
			got := h.Percentile(p)
			if math.Abs(got-ms)/ms > erroreMassimo {
				t.Errorf("%g ms, p%g: stimato %g ms, oltre l'errore del %.0f%%",
					ms, p*100, got, erroreMassimo*100)
			}
		}
	}
}

func TestPercentileDistingueLaCoda(t *testing.T) {
	// 98% a 10 ms e 2% a 2 s: il p99 deve cadere nella coda, il p50 no.
	var h Histogram
	for i := 0; i < 980; i++ {
		h.Add(10 * 1e6)
	}
	for i := 0; i < 20; i++ {
		h.Add(2000 * 1e6)
	}

	p50 := h.Percentile(0.50)
	p99 := h.Percentile(0.99)

	if math.Abs(p50-10)/10 > erroreMassimo {
		t.Errorf("p50 = %g ms, atteso circa 10", p50)
	}
	if p99 < 1000 {
		t.Errorf("p99 = %g ms: la coda a 2 s non e' stata vista", p99)
	}
}

func TestPercentileIstogrammaVuoto(t *testing.T) {
	var h Histogram
	if got := h.Percentile(0.95); got != 0 {
		t.Errorf("istogramma vuoto: p95 = %g, atteso 0", got)
	}
}

func TestCodificaAndataERitorno(t *testing.T) {
	var h Histogram
	h.Add(1e6)
	h.Add(50 * 1e6)
	h.Add(3000 * 1e6)

	got := Decode(h.Encode())
	if got != h {
		t.Error("l'istogramma non sopravvive al giro di codifica")
	}
}

func TestDecodeBlobCorrottoNonVaInPanico(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {1, 2, 3}, make([]byte, Buckets*4-1)} {
		var vuoto Histogram
		if got := Decode(raw); got != vuoto {
			t.Errorf("blob di %d byte: atteso istogramma vuoto", len(raw))
		}
	}
}

func TestMerge(t *testing.T) {
	var a, b Histogram
	a.Add(10 * 1e6)
	b.Add(10 * 1e6)
	b.Add(20 * 1e6)

	a.Merge(&b)

	var totale uint64
	for _, c := range a {
		totale += uint64(c)
	}
	if totale != 3 {
		t.Errorf("dopo la fusione: %d campioni, attesi 3", totale)
	}
}
