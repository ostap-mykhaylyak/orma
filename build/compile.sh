#!/bin/sh
# Compila l'estensione dentro il container. A schermo restano solo diagnostica
# ed esito; il log completo resta nel container.
#
# Il log non va dentro al repository montato: lo creerebbe come root, e in CI
# il passo successivo non potrebbe piu' scrivere nella stessa directory.
#
# Uso: sh /src/build/compile.sh [clean]
set -e

cd /src/ext
log=/tmp/orma-build.log

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
