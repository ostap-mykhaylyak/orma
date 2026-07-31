#!/bin/sh
# Prova end-to-end: estensione PHP, socket unix, daemon.
#
# Verifica, in ordine di importanza:
#   1. con il daemon spento la richiesta PHP funziona lo stesso;
#   2. uno script CLI produce una transazione di background;
#   3. una richiesta HTTP produce una transazione con il nome normalizzato;
#   4. un frame spazzatura viene rifiutato senza far cadere il daemon;
#   5. il daemon si ferma pulito riportando i contatori.
set -e

EXT="-d extension=/src/ext/modules/orma.so -d orma.app_name=prova"

echo "== preparazione =="
/src/dist/orma --init >/dev/null
sed -i 's|#log_level: info|log_level: debug|' /etc/orma/orma.yaml
/src/dist/orma --check-config >/dev/null && echo "configurazione valida"

cat >/tmp/lavoro.php <<'PHP'
<?php
$x = 0;
for ($i = 0; $i < 200000; $i++) { $x += $i; }
echo "risultato $x\n";
PHP

echo
echo "== 1. daemon spento: la richiesta deve funzionare comunque =="
php $EXT /tmp/lavoro.php
echo "uscita php: $?"

echo
echo "== avvio del daemon =="
/src/dist/orma start 2>/tmp/orma.log &
sleep 1
/src/dist/orma status >/dev/null && echo "daemon attivo"

echo
echo "== 2. transazione di background (CLI) =="
php $EXT /tmp/lavoro.php >/dev/null

echo
echo "== 3. transazioni web con URI ad alta cardinalita' =="
# Serve uno script di routing: senza, il server integrato risponde 404 da solo
# e la richiesta non entra mai in PHP.
cat >/tmp/router.php <<'PHP'
<?php
echo "ok";
PHP
php $EXT -S 127.0.0.1:8080 -t /tmp /tmp/router.php >/dev/null 2>&1 &
sleep 1
php -r '
foreach ([
  "/lavoro.php",
  "/prodotti/1234/dettaglio?utente=segreto",
  "/ordini/550e8400-e29b-41d4-a716-446655440000",
  "/a/b/c/d/e/f/g/h/i/j/k/l",
] as $p) { @file_get_contents("http://127.0.0.1:8080" . $p); }
'
sleep 1

echo
echo "== 4. frame spazzatura =="
php -r '
$c = stream_socket_client("unix:///run/orma/orma.sock", $e, $s);
$p = "\x01\x00" . str_repeat("x", 62);
fwrite($c, pack("V", strlen($p)) . $p);
fclose($c);
'
sleep 1
/src/dist/orma status >/dev/null && echo "daemon ancora vivo"

echo
echo "== 5. arresto: le metriche in memoria devono finire su disco =="
/src/dist/orma stop >/dev/null
sleep 1

echo
echo "== nomi di transazione visti dal daemon =="
grep -o 'nome=[^ ]*' /tmp/orma.log | sort | uniq -c

echo
grep -E 'msg=(ingestione|"frame malformato)' /tmp/orma.log

echo
echo "== 6. cosa e' finito in SQLite =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT txn_name, category, count, errors, ROUND(sum_ns/1e6,2) AS tot_ms
	   FROM metrics_1m ORDER BY sum_ns DESC;"

echo
echo "== 7. la UI rilegge dal database =="
/src/dist/orma start 2>>/tmp/orma.log &
sleep 1
php -r 'echo @file_get_contents("http://127.0.0.1:8737/?minuti=60");' >/tmp/ui.html
cp /tmp/ui.html /src/dist/panoramica.html
echo "byte della pagina: $(wc -c </tmp/ui.html)"
grep -o '<div class="k">[^<]*</div><div class="v[^>]*>[^<]*</div>' /tmp/ui.html \
	| sed -e 's|<div class="k">||' -e 's|</div><div class="v[^>]*>| = |' -e 's|</div>||'
echo "transazioni in pagina:"
grep -o '<td class="name">[^<]*' /tmp/ui.html | sed 's|<td class="name">|  |'
/src/dist/orma stop >/dev/null
