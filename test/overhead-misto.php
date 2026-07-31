<?php
// Secondo carico di riferimento: piu' vicino a una pagina vera, dove il tempo
// se ne va in query e non in chiamate di funzione.
//
// Serve a leggere il primo carico nella giusta prospettiva: quello misura il
// costo per chiamata nel caso peggiore, questo il costo che si paga davvero.

function formatta(array $riga): string
{
    return sprintf('%s: %.2f', $riga['nome'], $riga['prezzo']);
}

function pagina(PDO $pdo): string
{
    $out = '';
    for ($blocco = 0; $blocco < 1500; $blocco++) {
        $st = $pdo->prepare('SELECT id, nome, prezzo FROM prodotti WHERE id > ? LIMIT 10');
        $st->execute([($blocco * 10) % 600]);
        foreach ($st->fetchAll(PDO::FETCH_ASSOC) as $riga) {
            $out .= formatta($riga) . "\n";
        }
    }
    return $out;
}

function prepara(): PDO
{
    $pdo = new PDO('sqlite::memory:');
    $pdo->exec('CREATE TABLE prodotti (id INTEGER PRIMARY KEY, nome TEXT, prezzo REAL)');

    $ins = $pdo->prepare('INSERT INTO prodotti (nome, prezzo) VALUES (?, ?)');
    for ($i = 0; $i < 600; $i++) {
        $ins->execute(["prodotto $i", $i * 1.5]);
    }
    return $pdo;
}

$migliore = INF;
for ($giro = 0; $giro < 3; $giro++) {
    $pdo = prepara();

    $inizio = hrtime(true);
    pagina($pdo);
    $migliore = min($migliore, (hrtime(true) - $inizio) / 1e6);
}

printf("%.1f\n", $migliore);
