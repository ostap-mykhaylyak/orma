#!/bin/sh
# Prova end-to-end: estensione PHP, socket unix, daemon, SQLite, UI.
#
# Verifica, in ordine di importanza:
#   1. con il daemon spento la richiesta PHP funziona lo stesso;
#   2. uno script CLI produce una transazione di background;
#   3. una richiesta HTTP produce una transazione con il nome normalizzato;
#   4. le query PDO diventano span con lo statement offuscato;
#   5. curl e file_get_contents diventano span esterni con l'host;
#   6. un frame spazzatura viene rifiutato senza far cadere il daemon;
#   7. tutto arriva su SQLite e torna indietro nella UI.
set -e

# detail=2 instrumenta ogni funzione utente: in produzione il default e' 1,
# che emette uno span solo sopra soglia. Qui serve vedere il waterfall intero.
EXT="-d extension=/src/ext/modules/orma.so -d orma.app_name=prova -d orma.detail=2"

echo "== preparazione =="
/src/dist/orma --init >/dev/null
sed -i 's|#log_level: info|log_level: debug|' /etc/orma/orma.yaml
# Soglia bassa ma non nulla: nella prova le richieste durano pochi
# millisecondi, e con 8 ms alcune restano sopra e altre sotto. Serve a
# esercitare entrambe le vie di conservazione: sopra soglia, e "la piu' lenta
# del minuto" per quelle che sotto soglia non avrebbero mai un trace.
echo 'trace_threshold_ms: 8' >>/etc/orma/orma.yaml
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
echo "== 3/4/5. transazioni web, query PDO, chiamate esterne =="
# Il server integrato serve una richiesta alla volta: senza worker multipli la
# chiamata curl verso se stesso resterebbe in attesa fino al timeout.
PHP_CLI_SERVER_WORKERS=4 php $EXT -S 127.0.0.1:8080 -t /src/test /src/test/router.php >/dev/null 2>&1 &
sleep 1
php -r '
foreach ([
  "/prodotti/1234/dettaglio?utente=segreto",
  "/ordini/550e8400-e29b-41d4-a716-446655440000",
  "/a/b/c/d/e/f/g/h/i/j/k/l",
  "/carico",
  "/carico?esterna=1",
  "/carico?avvisi=1",
  "/guasto",
] as $p) { @file_get_contents("http://127.0.0.1:8080" . $p); }
'
sleep 1

echo
echo "== 6. frame spazzatura =="
php -r '
$c = stream_socket_client("unix:///run/orma/orma.sock", $e, $s);
$p = "\x01\x00" . str_repeat("x", 62);
fwrite($c, pack("V", strlen($p)) . $p);
fclose($c);
'
sleep 1
/src/dist/orma status >/dev/null && echo "daemon ancora vivo"

echo
echo "== arresto =="
/src/dist/orma stop >/dev/null
sleep 1

echo
echo "== transazioni viste dal daemon =="
grep 'msg=transazione' /tmp/orma.log | sed -E 's/.*nome=([^ ]*).*span=([0-9]*)/  \1  (span: \2)/'

echo
grep -E 'msg=(ingestione|"frame malformato)' /tmp/orma.log

echo
echo "== metriche per categoria =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT txn_name, category, count, errors AS falliti, ROUND(sum_ns/1e6,2) AS tot_ms
	   FROM metrics_1m ORDER BY txn_name, category;" 2>/dev/null || echo "(nessuna)"

echo
echo "== query lente =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT substr(statement,1,58) AS statement, count, ROUND(sum_ns/1e6,2) AS tot_ms
	   FROM slow_sql ORDER BY sum_ns DESC;" 2>/dev/null || echo "(tabella assente)"

echo
echo "== chiamate esterne =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT host, count, ROUND(sum_ns/1e6,2) AS tot_ms
	   FROM externals ORDER BY sum_ns DESC;" 2>/dev/null || echo "(tabella assente)"

echo
echo "== errori raccolti =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT class, substr(message,1,44) AS messaggio, severity AS grave, count
	   FROM errors ORDER BY severity DESC, count DESC;" 2>/dev/null || echo "(tabella assente)"

echo
echo "== trace conservati (soglia 8 ms: sotto restano solo le piu' lente del minuto) =="
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT id, txn_name, ROUND(duration_ns/1e6,2) AS ms,
	        CASE WHEN has_error THEN 'errore'
	             WHEN duration_ns >= 8000000 THEN 'sopra soglia'
	             ELSE 'piu lenta del minuto' END AS motivo
	   FROM traces ORDER BY duration_ns DESC;" 2>/dev/null || echo "(tabella assente)"

echo
echo "== waterfall del trace piu' lento =="
sqlite3 /var/lib/orma/orma.db \
	"SELECT json_extract(value,'\$.n') || '  ' || ROUND(json_extract(value,'\$.d')/1e6,3) || ' ms'
	   FROM traces, json_each(traces.spans)
	  WHERE traces.id = (SELECT id FROM traces ORDER BY duration_ns DESC LIMIT 1)
	  ORDER BY json_extract(value,'\$.o');" 2>/dev/null | head -20

echo
echo "== la UI rilegge dal database =="
/src/dist/orma start 2>>/tmp/orma.log &
sleep 1
php -r 'echo @file_get_contents("http://127.0.0.1:8737/?minuti=60");' >/src/dist/panoramica.html
php -r 'echo @file_get_contents("http://127.0.0.1:8737/database?minuti=60");' >/src/dist/database.html
php -r 'echo @file_get_contents("http://127.0.0.1:8737/esterne?minuti=60");' >/src/dist/esterne.html
php -r 'echo @file_get_contents("http://127.0.0.1:8737/errori?minuti=60");' >/src/dist/errori.html
php -r 'echo @file_get_contents("http://127.0.0.1:8737/tracce?minuti=60");' >/src/dist/tracce.html
id=$(sqlite3 /var/lib/orma/orma.db "SELECT id FROM traces ORDER BY duration_ns DESC LIMIT 1;")
php -r "echo @file_get_contents('http://127.0.0.1:8737/traccia?id=$id');" >/src/dist/traccia.html
echo "panoramica: $(wc -c </src/dist/panoramica.html) byte"
echo "database:   $(wc -c </src/dist/database.html) byte"
echo "esterne:    $(wc -c </src/dist/esterne.html) byte"
echo "errori:     $(wc -c </src/dist/errori.html) byte"
echo "tracce:     $(wc -c </src/dist/tracce.html) byte"
echo "traccia:    $(wc -c </src/dist/traccia.html) byte"
/src/dist/orma stop >/dev/null
