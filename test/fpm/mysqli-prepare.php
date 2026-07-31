<?php
// Statement preparati di mysqli, per via oggetto e per via procedurale.
//
// All'esecuzione lo statement non e' piu' raggiungibile: se la mappa costruita
// alla preparazione non funziona, questi span arrivano senza db.statement e
// nella pagina Database si vede solo "EXECUTE".

$m = new mysqli('db', 'wordpress', 'wordpress', 'wordpress');
$m->query('DROP TABLE IF EXISTS prova_orma');
$m->query('CREATE TABLE prova_orma (id INT PRIMARY KEY, nome VARCHAR(50), eta INT)');

$ins = $m->prepare('INSERT INTO prova_orma (id, nome, eta) VALUES (?, ?, ?)');
for ($i = 1; $i <= 20; $i++) {
    $nome = "utente $i";
    $eta = 20 + $i;
    $ins->bind_param('isi', $i, $nome, $eta);
    $ins->execute();
}
$ins->close();

$sel = $m->prepare('SELECT nome FROM prova_orma WHERE eta > ? ORDER BY eta LIMIT 5');
$soglia = 30;
$sel->bind_param('i', $soglia);
$sel->execute();
$sel->get_result();
$sel->close();

// Via procedurale.
$proc = mysqli_prepare($m, 'SELECT COUNT(*) FROM prova_orma WHERE nome LIKE ?');
$modello = 'utente%';
mysqli_stmt_bind_param($proc, 's', $modello);
mysqli_stmt_execute($proc);
mysqli_stmt_close($proc);

$m->close();
echo "statement preparati eseguiti\n";
