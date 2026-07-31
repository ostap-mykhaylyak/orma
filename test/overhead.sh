#!/bin/sh
# Misura il costo dell'instrumentazione su due carichi.
#
#   chiamate  fatto di sole chiamate di funzione: e' il caso peggiore, misura
#             il costo per chiamata senza nulla che lo diluisca;
#   misto     query e formattazione, come una pagina vera: e' il numero che
#             conta per decidere il default.
#
# Il daemon viene avviato davvero: con un socket morto il percorso di consegna
# fallirebbe subito e il numero sarebbe piu' bello del vero.
set -e

SO=/src/ext/modules/orma.so
SOCKET=/run/orma/orma.sock

/src/dist/orma --init --senza-estensione >/dev/null 2>&1 || true
/src/dist/orma start 2>/dev/null &
sleep 1

# Tre processi per configurazione, si tiene il migliore: la varianza fra
# processi e' maggiore di quella fra giri dentro lo stesso processo.
misura() {
	script="$1"
	shift
	migliore=""
	for _ in 1 2 3; do
		valore=$(php "$@" "$script")
		if [ -z "$migliore" ] || [ "$(awk -v a="$valore" -v b="$migliore" 'BEGIN{print (a<b)}')" = "1" ]; then
			migliore="$valore"
		fi
	done
	echo "$migliore"
}

tabella() {
	script="$1"
	etichetta="$2"

	base=$(misura "$script")
	spento=$(misura "$script" -d extension=$SO -d orma.enabled=0)
	d0=$(misura "$script" -d extension=$SO -d orma.enabled=1 -d orma.socket=$SOCKET -d orma.detail=0)
	d1=$(misura "$script" -d extension=$SO -d orma.enabled=1 -d orma.socket=$SOCKET -d orma.detail=1)
	d2=$(misura "$script" -d extension=$SO -d orma.enabled=1 -d orma.socket=$SOCKET -d orma.detail=2)

	echo
	echo "carico: $etichetta"
	printf '%-32s %8s %13s\n' "configurazione" "ms" "sovraccarico"
	printf '%-32s %8s %13s\n' "--------------------------------" "--------" "-------------"
	awk -v base="$base" -v spento="$spento" -v d0="$d0" -v d1="$d1" -v d2="$d2" '
		BEGIN {
			printf "%-32s %8.1f %13s\n", "senza estensione", base, "-"
			printf "%-32s %8.1f %12.1f%%\n", "caricata, orma.enabled=0", spento, (spento-base)/base*100
			printf "%-32s %8.1f %12.1f%%\n", "detail=0 (nessun observer)", d0, (d0-base)/base*100
			printf "%-32s %8.1f %12.1f%%\n", "detail=1 (solo sopra soglia)", d1, (d1-base)/base*100
			printf "%-32s %8.1f %12.1f%%\n", "detail=2 (ogni chiamata)", d2, (d2-base)/base*100
		}'
}

tabella /src/test/overhead.php "sole chiamate di funzione (caso peggiore)"
tabella /src/test/overhead-misto.php "query e formattazione (pagina tipica)"

/src/dist/orma stop >/dev/null 2>&1 || true
