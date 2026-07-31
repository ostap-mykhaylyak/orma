<?php
// Router per il server integrato usato da test/smoke.sh: senza, le richieste a
// percorsi inesistenti ricevono un 404 servito dal server e non entrano in PHP.

if (str_contains($_SERVER['REQUEST_URI'], 'carico')) {
    require __DIR__ . '/carico.php';
    return true;
}

echo 'ok';
return true;
