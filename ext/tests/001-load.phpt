--TEST--
orma: l'estensione si carica e registra le sue direttive INI
--EXTENSIONS--
orma
--INI--
orma.app_name=prova
--FILE--
<?php
var_dump(extension_loaded('orma'));
var_dump(phpversion('orma'));
var_dump(ini_get('orma.enabled'));
var_dump(ini_get('orma.app_name'));
?>
--EXPECT--
bool(true)
string(5) "0.1.0"
string(1) "1"
string(5) "prova"
