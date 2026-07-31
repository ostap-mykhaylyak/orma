<?php
// Input patologico per l'offuscatore SQL.
//
// L'offuscatore gira su ogni query di ogni richiesta, dentro il processo PHP.
// Un accesso fuori dai limiti li' non produce una metrica sbagliata: fa
// cadere il sito. Questo script gli passa davanti tutto cio' che potrebbe
// mandarlo oltre la fine del buffer, e va eseguito sotto AddressSanitizer.

$pdo = new PDO('sqlite::memory:');

$pezzi = [
    "SELECT",
    " * FROM t WHERE ",
    "'",                      // apice non chiuso
    '"',                      // virgoletta non chiusa
    "''",                     // apice raddoppiato
    "\\'",                    // apice con escape
    "\\",                     // backslash finale
    "--",                     // commento di linea senza fine riga
    "-- x\n",
    "/*",                     // commento a blocco non chiuso
    "/* x */",
    "/*/",                    // quasi chiusura
    "0",
    "12345678901234567890",
    "1.2e",                   // esponente troncato
    "1.2e+",
    "1.2e+3",
    ".5",
    "col2",                   // cifra dentro un identificatore
    "\x00",                   // byte nullo
    "\xff\xfe",               // byte non validi in UTF-8
    "\n\t\r  ",
    "(",
    ")",
    str_repeat("a", 300),
    str_repeat("'", 50),
    str_repeat("/*", 40),
    str_repeat("1", 200),
    str_repeat(" ", 100),
];

// Deterministico: un fallimento deve essere riproducibile senza salvare il seme.
mt_srand(20260731);

$casi = 0;
for ($i = 0; $i < 4000; $i++) {
    $sql = '';
    $quanti = mt_rand(1, 12);
    for ($p = 0; $p < $quanti; $p++) {
        $sql .= $pezzi[mt_rand(0, count($pezzi) - 1)];
    }

    try {
        // La query fallira' quasi sempre: non importa. L'offuscamento avviene
        // prima che l'handler originale venga chiamato, quindi il codice sotto
        // esame gira comunque.
        @$pdo->query($sql);
    } catch (Throwable $e) {
        // Attesa.
    }
    $casi++;
}

// Anche il limite di lunghezza va superato: lo statement viene troncato a
// ORMA_MAX_STATEMENT, e il troncamento e' un punto classico di errore.
foreach ([1999, 2000, 2001, 4000, 20000] as $lunghezza) {
    try {
        @$pdo->query("SELECT " . str_repeat("'x',", intdiv($lunghezza, 4)) . "1");
    } catch (Throwable $e) {
    }
    $casi++;
}

echo "casi provati: $casi\n";
echo "nessun crash\n";
