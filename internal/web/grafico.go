package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/ostap-mykhaylyak/orma/internal/store"
)

// Geometria del grafico, in unita' del viewBox. Il disegno e' SVG puro
// generato lato server: nessun JavaScript, nessuna libreria, niente da
// scaricare e niente che possa non caricarsi.
const (
	graficoLarghezza = 1000.0
	graficoAltezza   = 150.0
	graficoMargine   = 4.0
)

// Barra e' una colonna del grafico del traffico.
type Barra struct {
	X, Y, W, H float64
	// HErrori e' la porzione in errore, disegnata sopra la barra.
	HErrori float64
	Titolo  string
}

// Etichetta e' un riferimento temporale sotto l'asse.
type Etichetta struct {
	X    float64
	Testo string
}

// Grafico contiene tutto cio' che serve al template per disegnare.
type Grafico struct {
	Larghezza float64
	Altezza   float64

	// Aree e linee del tempo di risposta.
	AreaMedia string
	LineaP95  string

	Barre     []Barra
	Etichette []Etichetta

	MaxMS      float64
	MaxCount   uint64
	Punti      int
	SenzaDati  bool
	PassoTesto string
}

// costruisciGrafico traduce la serie in geometria.
//
// I punti senza traffico non spezzano la linea del tempo di risposta: la si
// interrompe, perche' unire due punti attraverso un buco disegnerebbe una
// pendenza che non e' mai esistita.
func costruisciGrafico(punti []store.Punto, passo int64) Grafico {
	g := Grafico{
		Larghezza:  graficoLarghezza,
		Altezza:    graficoAltezza,
		Punti:      len(punti),
		PassoTesto: descriviPasso(passo),
	}

	if len(punti) == 0 {
		g.SenzaDati = true
		return g
	}

	for _, p := range punti {
		if p.P95MS > g.MaxMS {
			g.MaxMS = p.P95MS
		}
		if p.AvgMS > g.MaxMS {
			g.MaxMS = p.AvgMS
		}
		if p.Count > g.MaxCount {
			g.MaxCount = p.Count
		}
	}
	if g.MaxCount == 0 {
		g.SenzaDati = true
		return g
	}
	if g.MaxMS <= 0 {
		g.MaxMS = 1
	}

	utile := graficoAltezza - graficoMargine*2
	larghezzaPunto := graficoLarghezza / float64(len(punti))

	x := func(i int) float64 { return float64(i)*larghezzaPunto + larghezzaPunto/2 }
	y := func(ms float64) float64 {
		return graficoAltezza - graficoMargine - (ms/g.MaxMS)*utile
	}

	var area, linea strings.Builder
	inSegmento := false

	for i, p := range punti {
		if p.Vuoto() {
			if inSegmento {
				area.WriteString(fmt.Sprintf(" L %.1f %.1f Z", x(i-1), graficoAltezza-graficoMargine))
				inSegmento = false
			}
			continue
		}
		if !inSegmento {
			area.WriteString(fmt.Sprintf(" M %.1f %.1f L %.1f %.1f",
				x(i), graficoAltezza-graficoMargine, x(i), y(p.AvgMS)))
			linea.WriteString(fmt.Sprintf(" M %.1f %.1f", x(i), y(p.P95MS)))
			inSegmento = true
			continue
		}
		area.WriteString(fmt.Sprintf(" L %.1f %.1f", x(i), y(p.AvgMS)))
		linea.WriteString(fmt.Sprintf(" L %.1f %.1f", x(i), y(p.P95MS)))
	}
	if inSegmento {
		area.WriteString(fmt.Sprintf(" L %.1f %.1f Z", x(len(punti)-1), graficoAltezza-graficoMargine))
	}

	g.AreaMedia = strings.TrimSpace(area.String())
	g.LineaP95 = strings.TrimSpace(linea.String())

	// Barre del traffico.
	larghezzaBarra := larghezzaPunto * 0.7
	if larghezzaBarra < 0.6 {
		larghezzaBarra = 0.6
	}
	for i, p := range punti {
		if p.Count == 0 {
			continue
		}
		h := float64(p.Count) / float64(g.MaxCount) * utile
		hErr := 0.0
		if p.Errors > 0 {
			hErr = float64(p.Errors) / float64(g.MaxCount) * utile
			if hErr < 1 {
				hErr = 1
			}
		}
		g.Barre = append(g.Barre, Barra{
			X:       x(i) - larghezzaBarra/2,
			Y:       graficoAltezza - graficoMargine - h,
			W:       larghezzaBarra,
			H:       h,
			HErrori: hErr,
			Titolo: fmt.Sprintf("%s — %d richieste, %d in errore, media %.0f ms",
				time.Unix(p.TS, 0).Format("15:04"), p.Count, p.Errors, p.AvgMS),
		})
	}

	// Cinque riferimenti temporali, o meno se i punti sono pochi.
	riferimenti := 5
	if len(punti) < riferimenti {
		riferimenti = len(punti)
	}
	for i := 0; i < riferimenti; i++ {
		idx := i * (len(punti) - 1) / max(riferimenti-1, 1)
		g.Etichette = append(g.Etichette, Etichetta{
			X:     x(idx),
			Testo: time.Unix(punti[idx].TS, 0).Format(formatoOra(passo)),
		})
	}

	return g
}

func formatoOra(passo int64) string {
	if passo >= 3600 {
		return "02/01 15h"
	}
	return "15:04"
}

func descriviPasso(passo int64) string {
	switch {
	case passo >= 3600:
		return "un punto per ora"
	case passo >= 300:
		return "un punto ogni 5 minuti"
	default:
		return "un punto al minuto"
	}
}
