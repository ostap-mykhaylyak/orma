package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// nomeCookie e' dove finisce il token dopo il primo accesso con ?token=, cosi'
// che la navigazione successiva non debba portarselo dietro nell'URL — dove
// finirebbe nei log del reverse proxy e nella cronologia del browser.
const nomeCookie = "orma_token"

// autentica avvolge un handler richiedendo il token.
//
// Con token vuoto non protegge nulla: la scelta e' dell'operatore, e il daemon
// gliela ricorda all'avvio invece di decidere al posto suo.
func (s *Server) autentica(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}

		if fornito, ok := s.tokenDaRichiesta(r); ok {
			// Il confronto e' a tempo costante: su una rete locale la
			// differenza e' teorica, ma costa una riga.
			if subtle.ConstantTimeCompare([]byte(fornito), []byte(s.token)) == 1 {
				if r.URL.Query().Get("token") != "" {
					http.SetCookie(w, &http.Cookie{
						Name:     nomeCookie,
						Value:    fornito,
						Path:     "/",
						HttpOnly: true,
						SameSite: http.SameSiteLaxMode,
					})
				}
				next(w, r)
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="orma"`)
		http.Error(w, "token mancante o errato", http.StatusUnauthorized)
	}
}

func (s *Server) tokenDaRichiesta(r *http.Request) (string, bool) {
	if intestazione := r.Header.Get("Authorization"); intestazione != "" {
		if dopo, trovato := strings.CutPrefix(intestazione, "Bearer "); trovato {
			return strings.TrimSpace(dopo), true
		}
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q, true
	}
	if c, err := r.Cookie(nomeCookie); err == nil {
		return c.Value, true
	}
	return "", false
}
