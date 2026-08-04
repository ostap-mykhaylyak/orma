package store

import "testing"

func spanConPosizioni(id, parent string, def *Posizione, pila []Posizione) TraceSpan {
	return TraceSpan{ID: id, Parent: parent, Name: id, Def: def, Pila: pila}
}

func TestRadiceComune(t *testing.T) {
	casi := map[string]struct {
		spans  []TraceSpan
		attesa string
	}{
		"prefisso lungo condiviso": {
			spans: []TraceSpan{
				spanConPosizioni("a", "", &Posizione{File: "/home/tess/public_html/wp-content/plugins/uno/x.php", Linea: 4}, nil),
				spanConPosizioni("b", "a", &Posizione{File: "/home/tess/public_html/wp-content/themes/due/y.php", Linea: 9}, nil),
			},
			attesa: "/home/tess/public_html/wp-content/",
		},
		"niente in comune": {
			spans: []TraceSpan{
				spanConPosizioni("a", "", &Posizione{File: "/srv/uno.php", Linea: 1}, nil),
				spanConPosizioni("b", "a", &Posizione{File: "/var/due.php", Linea: 1}, nil),
			},
			attesa: "",
		},
		"anche la pila conta": {
			spans: []TraceSpan{
				spanConPosizioni("a", "", &Posizione{File: "/var/www/html/wp-includes/wp-db.php", Linea: 2},
					[]Posizione{{File: "/var/www/html/wp-content/plugins/tre/z.php", Linea: 77}}),
			},
			attesa: "/var/www/html/",
		},
		"nessuna posizione": {
			spans:  []TraceSpan{spanConPosizioni("a", "", nil, nil)},
			attesa: "",
		},
	}

	for nome, c := range casi {
		t.Run(nome, func(t *testing.T) {
			tr := Trace{Spans: c.spans}
			if got := tr.RadiceComune(); got != c.attesa {
				t.Errorf("radice %q, attesa %q", got, c.attesa)
			}
		})
	}
}

// Il chiamante immediato di una query e' l'astrazione del framework: il rilievo
// deve indicare il livello piu' lontano, quello del plugin.
func TestPosizioneUtileSaltaIlFramework(t *testing.T) {
	pila := []Posizione{
		{File: "/var/www/wp-includes/class-wpdb.php", Linea: 2431},
		{File: "/var/www/wp-includes/meta.php", Linea: 1210},
		{File: "/var/www/wp-content/plugins/negozio/prezzi.php", Linea: 88},
	}
	if got := posizioneUtile(pila); got != "/var/www/wp-content/plugins/negozio/prezzi.php:88" {
		t.Errorf("posizione utile %q", got)
	}
	if got := posizioneUtile(nil); got != "" {
		t.Errorf("pila vuota: %q", got)
	}
}

func TestSpanQuery(t *testing.T) {
	tr := Trace{Spans: []TraceSpan{
		{ID: "a", Name: "SELECT", Attrs: map[string]string{"db.statement": "SELECT 1"},
			Pila: []Posizione{{File: "/app/lista.php", Linea: 12}}},
		{ID: "b", Parent: "a", Name: "SELECT", Attrs: map[string]string{"db.statement": "SELECT 1"},
			Pila: []Posizione{{File: "/app/altro.php", Linea: 99}}},
	}}

	s := tr.spanQuery("SELECT 1")
	if s == nil || s.ID != "a" {
		t.Fatalf("prima esecuzione non trovata: %+v", s)
	}
	if tr.spanQuery("SELECT 2") != nil {
		t.Error("query assente trovata lo stesso")
	}
}

// La domanda davanti a una query lenta e' "quale plugin la esegue": il percorso
// contiene gia' la risposta, e le astrazioni del core non devono coprirla.
func TestComponente(t *testing.T) {
	casi := map[string]struct {
		span   TraceSpan
		attesa string
	}{
		"plugin dalla pila di una query": {
			span: TraceSpan{Pila: []Posizione{
				{File: "/var/www/wp-includes/class-wpdb.php", Linea: 2351},
				{File: "/var/www/wp-includes/meta.php", Linea: 1210},
				{File: "/var/www/wp-content/plugins/complianz-gdpr/cookie.php", Linea: 88},
			}},
			attesa: "complianz-gdpr",
		},
		"tema dalla definizione": {
			span:   TraceSpan{Def: &Posizione{File: "/var/www/wp-content/themes/negozio/loop.php", Linea: 18}},
			attesa: "negozio",
		},
		"pacchetto composer, autore compreso": {
			span:   TraceSpan{Def: &Posizione{File: "/srv/app/vendor/guzzlehttp/guzzle/src/Client.php", Linea: 200}},
			attesa: "guzzlehttp/guzzle",
		},
		"solo core: nessun componente": {
			span: TraceSpan{Pila: []Posizione{
				{File: "/var/www/wp-includes/class-wpdb.php", Linea: 2351},
			}},
			attesa: "",
		},
		"contenitore senza nome dopo": {
			span:   TraceSpan{Def: &Posizione{File: "/var/www/wp-content/plugins", Linea: 1}},
			attesa: "",
		},
	}

	for nome, c := range casi {
		t.Run(nome, func(t *testing.T) {
			if got := componenteSpan(c.span); got != c.attesa {
				t.Errorf("componente %q, atteso %q", got, c.attesa)
			}
		})
	}
}
