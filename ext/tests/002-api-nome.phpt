--TEST--
orma: denominazione esplicita della transazione
--EXTENSIONS--
orma
--INI--
orma.enabled=1
orma.socket=/percorso/che/non/esiste.sock
--FILE--
<?php
var_dump(orma_name_transaction('checkout'));

// Un nome vuoto non sostituisce quello automatico.
var_dump(orma_name_transaction(''));

var_dump(orma_background_transaction('coda/ordini'));

$traccia = orma_get_trace_id();
var_dump(strlen($traccia));
var_dump((bool) preg_match('/^[0-9a-f]{32}$/', $traccia));

// Lo stesso identificativo per tutta la richiesta.
var_dump($traccia === orma_get_trace_id());
?>
--EXPECT--
bool(true)
bool(false)
bool(true)
int(32)
bool(true)
bool(true)
