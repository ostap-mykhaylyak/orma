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

// Chiesta esplicitamente di non essere osservata: non deve arrivare al daemon.
if (str_contains($_SERVER['REQUEST_URI'], 'silenzio')) {
    orma_ignore();
    echo 'non osservata';
    return true;
}

// API userland: nome esplicito, attributi, span manuale, errore applicativo.
if (str_contains($_SERVER['REQUEST_URI'], 'api')) {
    orma_name_transaction('checkout/pagamento');
    orma_add_attribute('cliente', 'ACME');
    orma_add_attribute('articoli', 3);
    orma_add_attribute('totale', 19.90);
    orma_add_attribute('primo_ordine', true);

    $span = orma_start_span('calcolo spedizione');
    usleep(2000);
    orma_end_span($span);

    orma_notice_error('circuito di pagamento non raggiungibile');

    echo orma_get_trace_id();
    return true;
}

echo 'ok';
return true;
