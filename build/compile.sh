#!/bin/sh
# Compila l'estensione dentro il container. Il log completo finisce in
# dist/build.log; a schermo restano solo diagnostica e esito.
#
# Uso: sh /src/build/compile.sh [clean]
set -e

cd /src/ext
mkdir -p /src/dist
log=/src/dist/build.log

if [ "$1" = "clean" ]; then
	make distclean >/dev/null 2>&1 || true
	phpize --clean >/dev/null 2>&1 || true
fi

{
	phpize
	./configure --enable-orma
	make -j"$(nproc)"
} >"$log" 2>&1 || {
	echo "BUILD FALLITA"
	tail -40 "$log"
	exit 1
}

echo "--- diagnostica ---"
grep -E "warning:|error:" "$log" || echo "nessun warning, nessun errore"
echo "--- esito ---"
tail -2 "$log"
