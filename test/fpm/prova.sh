#!/bin/sh
# Prova su php-fpm con WordPress vero.
#
# Verifica le cose che CLI e server integrato non toccano: worker che vivono
# migliaia di richieste, fork del master, riuso di arena e buffer.
#
# Il carico gira due volte, con l'estensione caricata ma spenta e poi accesa.
# Senza il giro di controllo, la crescita di memoria dei worker e il calo di
# throughput non sarebbero attribuibili: WordPress cresce da solo mentre
# opcache e cache interne si riempiono.
set -e

REGISTRO=/risultati
mkdir -p $REGISTRO
RICHIESTE=${RICHIESTE:-3000}

echo "== installazione di WordPress =="
mkdir -p /run/php /var/www/html
chown www-data:www-data /run/php

wp core download --allow-root --quiet
wp config create --allow-root --quiet \
	--dbname=wordpress --dbuser=wordpress --dbpass=wordpress --dbhost=db
wp core install --allow-root --quiet \
	--url=http://localhost --title="Prova orma" \
	--admin_user=admin --admin_password=segreta --admin_email=prova@esempio.it
wp post generate --allow-root --count=30 --quiet
chown -R www-data:www-data /var/www/html
echo "WordPress $(wp core version --allow-root) installato"

echo
echo "== installazione di orma =="
cp /src/ext/modules/orma.so /tmp/orma.so
/src/dist/orma --init --extension /tmp/orma.so >/dev/null
phpenmod orma

sed -i 's|^#socket_group:.*|socket_group: www-data|' /etc/orma/orma.yaml
grep -q '^socket_group:' /etc/orma/orma.yaml || echo 'socket_group: www-data' >>/etc/orma/orma.yaml
echo 'trace_threshold_ms: 200' >>/etc/orma/orma.yaml

/src/dist/orma start 2>$REGISTRO/orma.log &
sleep 1
nginx

php -m | grep -qx orma || { echo "FALLITO: php non carica l'estensione"; exit 1; }
echo "estensione installata e caricata"

rss() {
	# RSS dei soli worker: il master non serve richieste.
	ps -o rss=,args= -C php-fpm8.5 | grep 'pool prova' | awk '{s+=$1} END {print s+0}'
}

giro() {
	etichetta="$1"
	attiva="$2"

	sed -i "s/^orma.enabled=.*/orma.enabled=$attiva/" /etc/php/8.5/mods-available/orma.ini
	grep -q '^orma.enabled=' /etc/php/8.5/mods-available/orma.ini \
		|| echo "orma.enabled=$attiva" >>/etc/php/8.5/mods-available/orma.ini

	pkill -f 'php-fpm.*master' 2>/dev/null || true
	sleep 1
	php-fpm8.5 --daemonize
	sleep 2

	# Qualche richiesta a vuoto perche' opcache e le cache di WordPress si
	# riempiano: altrimenti la crescita del riscaldamento verrebbe attribuita
	# all'estensione.
	for _ in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null http://127.0.0.1/; done

	prima=$(rss)
	ab -q -n $RICHIESTE -c 8 http://127.0.0.1/ >$REGISTRO/ab-$attiva.txt 2>&1 || true
	sleep 1
	dopo=$(rss)

	rps=$(awk '/Requests per second/ {print $4}' $REGISTRO/ab-$attiva.txt)
	falliti=$(awk '/Failed requests/ {print $3}' $REGISTRO/ab-$attiva.txt)

	echo "$etichetta|$rps|$falliti|$prima|$dopo"
}

echo
echo "== carico: $RICHIESTE richieste per giro, 8 in parallelo =="
spento=$(giro "orma spenta" 0)
acceso=$(giro "orma accesa" 1)

echo
printf '%-14s %10s %9s %12s %12s %10s\n' "" "req/s" "falliti" "RSS prima" "RSS dopo" "crescita"
printf '%-14s %10s %9s %12s %12s %10s\n' "-------------" "----------" "---------" "------------" "------------" "----------"
for riga in "$spento" "$acceso"; do
	echo "$riga" | awk -F'|' '{
		printf "%-14s %10s %9s %10s kB %10s kB %8s kB\n", $1, $2, $3, $4, $5, $5 - $4
	}'
done

awk -v spento="$(echo "$spento" | cut -d'|' -f2)" \
    -v acceso="$(echo "$acceso" | cut -d'|' -f2)" \
    -v cresce_spento="$(echo "$spento" | awk -F'|' '{print $5-$4}')" \
    -v cresce_acceso="$(echo "$acceso" | awk -F'|' '{print $5-$4}')" '
	BEGIN {
		if (spento > 0 && acceso > 0) {
			printf "\nsovraccarico sul throughput: %.1f%%\n", (spento/acceso - 1) * 100
		}
		printf "crescita di memoria attribuibile a orma: %d kB\n", cresce_acceso - cresce_spento
	}'

echo
echo "== stato dei processi =="
echo "worker vivi: $(pgrep -f 'pool prova' | wc -l)"
if grep -qiE 'segfault|SIGSEGV|SIGBUS|exited on signal' /var/log/php8.5-fpm.log 2>/dev/null; then
	echo "FALLITO: php-fpm riporta processi caduti"
	grep -iE 'segfault|SIGSEGV|SIGBUS|exited on signal' /var/log/php8.5-fpm.log | head -5
	exit 1
fi
echo "nessun crash nel log di php-fpm"

echo
echo "== statement preparati di mysqli =="
php /src/test/fpm/mysqli-prepare.php

echo
echo "== qualche pagina diversa, per i nomi di transazione =="
for percorso in "/?p=1" "/wp-login.php" "/?s=prova" "/feed/" "/wp-json/wp/v2/posts"; do
	curl -s -o /dev/null "http://127.0.0.1$percorso" || true
done
sleep 2

echo
echo "== cosa ha raccolto orma =="
/src/dist/orma stop >/dev/null
sleep 1

sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT txn_name, SUM(count) AS richieste, SUM(errors) AS falliti,
	        ROUND(SUM(sum_ns)/1e6/SUM(count),2) AS media_ms
	   FROM metrics_1m WHERE category='totale'
	  GROUP BY txn_name ORDER BY SUM(sum_ns) DESC LIMIT 10;"

echo
echo "-- scomposizione della homepage --"
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT category, SUM(count) AS n, ROUND(SUM(sum_ns)/1e6,1) AS tot_ms
	   FROM metrics_1m WHERE txn_name='/' GROUP BY category;"

echo
echo "-- query piu' costose --"
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT substr(statement,1,62) AS statement, SUM(count) AS n,
	        ROUND(SUM(sum_ns)/1e6,1) AS tot_ms
	   FROM slow_sql GROUP BY stmt_hash ORDER BY SUM(sum_ns) DESC LIMIT 8;"

echo
echo "-- statement preparati riconosciuti --"
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT substr(statement,1,58) AS statement, SUM(count) AS n
	   FROM slow_sql WHERE statement LIKE '%prova_orma%'
	  GROUP BY stmt_hash ORDER BY statement;"

echo
echo "-- da dove vengono le query: un livello per file, altrimenti si resta dentro wpdb --"
sqlite3 /var/lib/orma/orma.db \
	"SELECT json_extract(value,'\$.n') || '  ' ||
	        COALESCE(json_extract(value,'\$.pl[0].f') || ':' || json_extract(value,'\$.pl[0].l'), '-') ||
	        '  <- ' || COALESCE(json_extract(value,'\$.pl[1].f') || ':' || json_extract(value,'\$.pl[1].l'), '-') ||
	        '  <- ' || COALESCE(json_extract(value,'\$.pl[2].f') || ':' || json_extract(value,'\$.pl[2].l'), '-')
	   FROM traces, json_each(traces.spans)
	  WHERE json_extract(value,'\$.a') LIKE '%db.statement%'
	  ORDER BY json_extract(value,'\$.d') DESC LIMIT 6;" 2>/dev/null

# Se i primi tre livelli stanno tutti nello stesso file, la pila non e' uscita
# dall'astrazione del database e non dice chi ha voluto la query.
dentro=$(sqlite3 /var/lib/orma/orma.db \
	"SELECT COUNT(*) FROM traces, json_each(traces.spans)
	  WHERE json_extract(value,'\$.a') LIKE '%db.statement%'
	    AND json_extract(value,'\$.pl[1].f') = json_extract(value,'\$.pl[0].f');" 2>/dev/null)
echo "query con due livelli nello stesso file: ${dentro:-0} (devono essere 0)"
if [ "${dentro:-0}" -gt 0 ]; then
	echo "FALLITO: la pila non esce dal file del chiamante"
	exit 1
fi

echo
echo "-- errori --"
sqlite3 -header -column /var/lib/orma/orma.db \
	"SELECT class, substr(message,1,46) AS messaggio, severity AS grave, SUM(count) AS n
	   FROM errors GROUP BY fingerprint ORDER BY SUM(count) DESC LIMIT 6;"

echo
echo "-- ingestione --"
grep 'msg=ingestione' $REGISTRO/orma.log || true

cp /var/lib/orma/orma.db $REGISTRO/orma.db 2>/dev/null || true

echo
echo "== pagine con i dati di WordPress =="
/src/dist/orma start 2>>$REGISTRO/orma.log &
sleep 1
TOKEN=$(awk '/^ui_token:/ {print $2}' /etc/orma/orma.yaml)

scarica() {
	curl -s -o "$REGISTRO/$2" "http://127.0.0.1:8737/$1"
	echo "  $2: $(wc -c <"$REGISTRO/$2") byte"
}
scarica "?minuti=60&token=$TOKEN"                          panoramica.html
scarica "transazione?nome=%2F&minuti=60&token=$TOKEN"       transazione.html
scarica "database?minuti=60&token=$TOKEN"                   database.html
scarica "stato?minuti=60&token=$TOKEN"                      stato.html
/src/dist/orma stop >/dev/null

echo
echo "== fine =="
