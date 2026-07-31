<?php
// Carico sintetico per esercitare gli hook: query PDO e chiamate HTTP uscenti.
// Usato da test/smoke.sh, non fa parte del prodotto.

$pdo = new PDO('sqlite::memory:');
$pdo->exec('CREATE TABLE utenti (id INTEGER PRIMARY KEY, email TEXT, eta INTEGER)');

$ins = $pdo->prepare('INSERT INTO utenti (email, eta) VALUES (?, ?)');
foreach ([['mario@esempio.it', 41], ['lucia@esempio.it', 33]] as [$email, $eta]) {
    $ins->execute([$email, $eta]);
}

// Letterali e numeri devono sparire dallo statement riportato.
$pdo->query("SELECT * FROM utenti WHERE email = 'mario@esempio.it' AND eta > 30");
$pdo->query("SELECT COUNT(*) FROM utenti /* commento da rimuovere */ WHERE eta < 99");

$st = $pdo->prepare('SELECT email FROM utenti WHERE id = ?');
$st->execute([1]);

if (isset($_GET['esterna'])) {
    $ch = curl_init('http://127.0.0.1:8080/router.php?token=segreto');
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 2);
    curl_exec($ch);
    curl_close($ch);

    @file_get_contents('http://127.0.0.1:8080/router.php');
}

echo "fatto\n";
