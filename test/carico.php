<?php
// Carico sintetico per esercitare gli hook e l'observer: funzioni annidate,
// query PDO e chiamate HTTP uscenti. Usato da test/smoke.sh, non fa parte del
// prodotto.

function preparaSchema(PDO $pdo): void
{
    $pdo->exec('CREATE TABLE utenti (id INTEGER PRIMARY KEY, email TEXT, eta INTEGER)');
}

function inserisciUno(PDO $pdo, string $email, int $eta): void
{
    $st = $pdo->prepare('INSERT INTO utenti (email, eta) VALUES (?, ?)');
    $st->execute([$email, $eta]);
}

function popolaUtenti(PDO $pdo): void
{
    foreach ([['mario@esempio.it', 41], ['lucia@esempio.it', 33]] as [$email, $eta]) {
        inserisciUno($pdo, $email, $eta);
    }
}

final class Rapporto
{
    public function __construct(private PDO $pdo)
    {
    }

    public function genera(): array
    {
        // Letterali e numeri devono sparire dallo statement riportato.
        $adulti = $this->pdo->query(
            "SELECT * FROM utenti WHERE email = 'mario@esempio.it' AND eta > 30"
        )->fetchAll();

        $totale = $this->pdo->query(
            "SELECT COUNT(*) FROM utenti /* commento da rimuovere */ WHERE eta < 99"
        )->fetchColumn();

        return [$adulti, $totale];
    }
}

function generaAvvisi(): void
{
    trigger_error('listino non aggiornato da tre giorni', E_USER_WARNING);

    $riga = ['codice' => 'AB-1'];
    echo $riga['prezzo'] ?? '';  // niente avviso: l'accesso e' protetto

    $mancante = [];
    echo @$mancante[0];          // E_WARNING: chiave non definita

    try {
        throw new RuntimeException('listino remoto irraggiungibile');
    } catch (RuntimeException $e) {
        // Catturata: viene comunque registrata al lancio, ma non fa fallire
        // la transazione.
    }
}

function chiamaEsterni(): void
{
    $ch = curl_init('http://127.0.0.1:8080/router.php?token=segreto');
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 2);
    curl_exec($ch);
    curl_close($ch);

    @file_get_contents('http://127.0.0.1:8080/router.php');
}

$pdo = new PDO('sqlite::memory:');
preparaSchema($pdo);
popolaUtenti($pdo);

$rapporto = new Rapporto($pdo);
$rapporto->genera();

$st = $pdo->prepare('SELECT email FROM utenti WHERE id = ?');
$st->execute([1]);

if (isset($_GET['avvisi'])) {
    generaAvvisi();
}

if (isset($_GET['esterna'])) {
    chiamaEsterni();
}

echo "fatto\n";
