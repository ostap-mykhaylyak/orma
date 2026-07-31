--TEST--
orma: span aperti da userland
--EXTENSIONS--
orma
--INI--
orma.enabled=1
orma.socket=/percorso/che/non/esiste.sock
--FILE--
<?php
$primo = orma_start_span('elaborazione');
var_dump(is_int($primo), $primo >= 0);

$annidato = orma_start_span('sotto-elaborazione');
var_dump($annidato > $primo);

var_dump(orma_end_span($annidato));
var_dump(orma_end_span($primo));

// Chiudere due volte non e' un errore: e' una richiesta senza effetto.
var_dump(orma_end_span($primo));

// Un riferimento mai emesso viene ignorato.
var_dump(orma_end_span(99999));
var_dump(orma_end_span(-1));

// Un nome vuoto non produce span.
var_dump(orma_start_span(''));
?>
--EXPECT--
bool(true)
bool(true)
bool(true)
bool(true)
bool(true)
bool(true)
bool(true)
bool(false)
int(-1)
