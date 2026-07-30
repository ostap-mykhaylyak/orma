#!/bin/sh
# Prova end-to-end del M0: il daemon si avvia, accetta un frame sul socket unix,
# lo valida e lo conta, poi si ferma pulito su SIGTERM.
set -e

echo "== --version =="
/src/dist/orma --version

echo "== --init =="
/src/dist/orma --init

echo "== --check-config =="
/src/dist/orma --check-config

echo "== start =="
/src/dist/orma start &
sleep 1

echo "== status =="
/src/dist/orma status

echo "== invio di un frame valido e di uno con versione errata =="
php -r '
$c = stream_socket_client("unix:///run/orma/orma.sock", $e, $s);
$p = "\x01\x00" . str_repeat("x", 62);          // versione 1, flags 0, payload
fwrite($c, pack("V", strlen($p)) . $p);
fclose($c);

$c = stream_socket_client("unix:///run/orma/orma.sock", $e, $s);
$p = "\x09\x00" . str_repeat("x", 10);          // versione 9: deve essere rifiutato
fwrite($c, pack("V", strlen($p)) . $p);
fclose($c);
'
sleep 1

echo "== stop =="
/src/dist/orma stop
wait
echo "== fine =="
