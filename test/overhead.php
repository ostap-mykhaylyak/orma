<?php
// Carico di riferimento per misurare il costo dell'instrumentazione.
//
// Volutamente pesante di chiamate di funzione: e' li' che l'observer costa,
// e un carico fatto di sole operazioni native darebbe un numero lusinghiero
// e inutile.

function fib(int $n): int
{
    return $n < 2 ? $n : fib($n - 1) + fib($n - 2);
}

// Il carico deve durare abbastanza da rendere il rumore di misura
// trascurabile: sotto la decina di millisecondi la varianza fra processi vale
// piu' della differenza che si vuole misurare.
function lavoro(): void
{
    fib(25);

    $s = '';
    for ($i = 0; $i < 200000; $i++) {
        $s .= 'x';
    }

    $numeri = range(1, 200000);
    array_sum(array_map(static fn (int $v): int => $v * 2, $numeri));
}

// Si tiene il giro migliore: la mediana di misure rumorose in container e'
// meno stabile del minimo, e il minimo e' comunque il piu' vicino al costo
// vero senza interferenze.
$migliore = INF;
for ($giro = 0; $giro < 3; $giro++) {
    $inizio = hrtime(true);
    lavoro();
    $migliore = min($migliore, (hrtime(true) - $inizio) / 1e6);
}

printf("%.1f\n", $migliore);
