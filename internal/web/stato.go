package web

import (
	"fmt"
	"net/http"
	"time"
)

// Stato sono i contatori interni del daemon.
//
// Un APM che non sa dire se sta perdendo dati mente per omissione: se il
// socket satura o il disco si riempie, tutte le altre pagine continuano a
// mostrare numeri plausibili ma incompleti. Questa pagina esiste per rendere
// visibile quel caso.
type Stato struct {
	Versione     string
	Avvio        time.Time
	Socket       string
	SocketGruppo string
	Database     string
	DimensioneDB int64

	FrameRicevuti  uint64
	ByteRicevuti   uint64
	FrameRifiutati uint64

	AgentPerse       uint64
	PerseConnessione uint64
	PerseTimeout     uint64
	PerseScrittura   uint64
	FinestreScritte  uint64
	FinestrePerse   uint64
	FinestreAperte  int

	UltimoAllarme string
}

// Attivo e' da quanto gira il daemon.
func (s Stato) Attivo() string {
	d := time.Since(s.Avvio).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// DimensioneLeggibile formatta i byte del database.
func (s Stato) DimensioneLeggibile() string {
	return byteLeggibili(s.DimensioneDB)
}

// ByteLeggibili formatta i byte ricevuti dal socket.
func (s Stato) ByteLeggibili() string {
	return byteLeggibili(int64(s.ByteRicevuti))
}

// InSalute e' falso quando c'e' qualcosa che l'operatore deve guardare.
func (s Stato) InSalute() bool {
	return s.AgentPerse == 0 && s.FinestrePerse == 0 && s.FrameRifiutati == 0
}

// Rimedio traduce la causa prevalente delle perdite in cosa fare.
//
// Un contatore che sale senza dire cosa farci e' un contatore che si impara a
// ignorare: qui la causa piu' frequente diventa una frase operativa.
func (s Stato) Rimedio() string {
	switch {
	case s.AgentPerse == 0:
		return ""
	case s.PerseConnessione >= s.PerseTimeout && s.PerseConnessione >= s.PerseScrittura:
		return "Connessione al socket fallita: il daemon era fermo, oppure i worker PHP " +
			"non hanno il permesso di scrivere sul socket. Controlla socket_group."
	case s.PerseTimeout >= s.PerseScrittura:
		return "Budget di consegna scaduto: la macchina era carica e cinque millisecondi " +
			"non sono bastati. Alza orma.send_timeout_ms nell'INI dell'estensione."
	default:
		return "Socket caduto durante la scrittura: succede quando il daemon viene " +
			"riavviato mentre PHP sta servendo richieste."
	}
}

func byteLeggibili(n int64) string {
	const unita = 1024
	if n < unita {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unita), 0
	for v := n / unita; v >= unita; v /= unita {
		div *= unita
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

type datiStato struct {
	comune
	Stato Stato
}

func (s *Server) handleStato(w http.ResponseWriter, r *http.Request) {
	minuti, _ := intervallo(r)

	var stato Stato
	if s.stato != nil {
		stato = s.stato()
	}

	s.render(w, "stato.html", datiStato{
		comune: newComune("Stato", "stato", minuti),
		Stato:  stato,
	})
}
