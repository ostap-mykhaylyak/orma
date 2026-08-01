#!/bin/sh
# Verifica il servizio systemd: installazione, avvio al boot, riavvio,
# delega dei verbi e disinstallazione.
set -e

echo "== systemd e' vivo =="
systemctl is-system-running --wait >/dev/null 2>&1 || true
systemctl --version | head -1

echo
echo "== installazione dal binario in /root, come farebbe un utente =="
mkdir -p /root
cp /src/dist/orma /root/orma
chmod +x /root/orma
cp /src/ext/modules/orma.so /tmp/orma.so

# --init deve accorgersi che /root non e' un posto da cui far partire un
# servizio, e copiare il binario dove lo e'.
/root/orma --init --extension /tmp/orma.so

echo
echo "== unit generata =="
cat /etc/systemd/system/orma.service | grep -E 'ExecStart|Restart=|RuntimeDirectory|StateDirectory|ReadWritePaths|WantedBy'

echo
echo "== abilitata all'avvio =="
systemctl is-enabled orma

echo
echo "== avvio =="
systemctl start orma
sleep 2
systemctl is-active orma
systemctl show orma --property=MainPID --value | sed 's/^/  pid: /'

echo
echo "== la directory del socket e' stata creata da systemd =="
ls -ld /run/orma
ls -l /run/orma/orma.sock

echo
echo "== il daemon raccoglie sotto systemd =="
php -d extension=/tmp/orma.so -d orma.app_name=servizio -d orma.socket=/run/orma/orma.sock -r '
$pdo = new PDO("sqlite::memory:");
$pdo->exec("CREATE TABLE t (id INTEGER PRIMARY KEY)");
$pdo->query("SELECT * FROM t WHERE id = 42");
echo "richiesta eseguita\n";
'
sleep 1

echo
echo "== i verbi passano da systemd =="
/usr/local/bin/orma status | head -3
echo "-- restart --"
/usr/local/bin/orma restart
sleep 2
systemctl is-active orma

echo
echo "== stop volontario: systemd non deve rimetterlo su =="
/usr/local/bin/orma stop
sleep 3
stato=$(systemctl is-active orma || true)
echo "stato dopo lo stop: $stato"
if [ "$stato" = "active" ]; then
	echo "FALLITO: il servizio e' ripartito da solo dopo uno stop voluto"
	exit 1
fi

echo
echo "== riavvio della macchina simulato: il servizio riparte =="
systemctl start orma
sleep 2
systemctl is-active orma

echo
echo "== cosa e' arrivato =="
systemctl stop orma
sleep 1
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT txn_name, category, count FROM metrics_1m ORDER BY category;" || echo "(nessun dato)"

echo
echo "== disinstallazione =="
/usr/local/bin/orma --purge | grep -E 'rimoss|Fatto'
if [ -f /etc/systemd/system/orma.service ]; then
	echo "FALLITO: l'unit e' rimasta"
	exit 1
fi
systemctl is-enabled orma 2>&1 | head -1

echo
echo "== VERIFICA SUPERATA =="
