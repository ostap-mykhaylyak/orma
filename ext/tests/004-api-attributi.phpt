--TEST--
orma: attributi personalizzati ed errori applicativi
--EXTENSIONS--
orma
--INI--
orma.enabled=1
orma.socket=/percorso/che/non/esiste.sock
--FILE--
<?php
var_dump(orma_add_attribute('cliente', 'ACME'));
var_dump(orma_add_attribute('articoli', 7));
var_dump(orma_add_attribute('totale', 19.90));
var_dump(orma_add_attribute('primo_ordine', true));

// Chiave vuota rifiutata.
var_dump(orma_add_attribute('', 'x'));

// Oltre il tetto di otto attributi si smette di accettarne.
for ($i = 0; $i < 8; $i++) {
    orma_add_attribute("riempitivo$i", $i);
}
var_dump(orma_add_attribute('uno_di_troppo', 1));

var_dump(orma_notice_error('pagamento rifiutato dal circuito'));
var_dump(orma_notice_error('ordine incoerente', new RuntimeException('quantita negativa')));

// Il secondo argomento accetta solo Throwable o null.
var_dump(orma_notice_error('senza eccezione', null));
?>
--EXPECT--
bool(true)
bool(true)
bool(true)
bool(true)
bool(false)
bool(false)
bool(true)
bool(true)
bool(true)
