<?php
// Router per il server integrato usato da test/smoke.sh: senza, le richieste a
// percorsi inesistenti ricevono un 404 servito dal server e non entrano in PHP.

if (str_contains($_SERVER['REQUEST_URI'], 'carico')) {
    require __DIR__ . '/carico.php';
    return true;
}

// Eccezione non catturata: 500 e E_ERROR, la transazione deve risultare fallita.
if (str_contains($_SERVER['REQUEST_URI'], 'guasto')) {
    throw new LogicException('configurazione del pagamento assente');
}

echo 'ok';
return true;
