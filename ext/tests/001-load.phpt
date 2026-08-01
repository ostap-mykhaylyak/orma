--TEST--
orma: l'estensione si carica e registra le sue direttive INI
--EXTENSIONS--
orma
--INI--
orma.app_name=prova
--FILE--
<?php
var_dump(extension_loaded('orma'));
// La versione non si verifica alla lettera: cambierebbe a ogni release e il
// test fallirebbe per il motivo sbagliato. Interessa che sia dichiarata.
var_dump((bool) preg_match('/^\d+\.\d+\.\d+$/', phpversion('orma')));
var_dump(ini_get('orma.enabled'));
var_dump(ini_get('orma.app_name'));
?>
--EXPECT--
bool(true)
bool(true)
string(1) "1"
string(5) "prova"
