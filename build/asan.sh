#!/bin/sh
# Ricompila l'estensione con AddressSanitizer e le fa girare addosso i test.
#
# L'estensione vive dentro ogni processo PHP del server: un accesso fuori dai
# limiti non e' un dato sbagliato, e' il sito giu'. Questo e' l'unico modo
# pratico di cercarli prima che lo facciano gli utenti.
#
# USE_ZEND_ALLOC=0 e' necessario: con l'allocatore di Zend attivo, la memoria
# presa da pemalloc arriva dai suoi blocchi e ASAN non vede nulla.
set -e

cd /src/ext

echo "== compilazione con AddressSanitizer =="
make distclean >/dev/null 2>&1 || true
phpize --clean >/dev/null 2>&1 || true
phpize >/dev/null
./configure --enable-orma \
	CFLAGS="-fsanitize=address,undefined -fno-omit-frame-pointer -g -O1" \
	LDFLAGS="-fsanitize=address,undefined" >/dev/null
make -j"$(nproc)" >/tmp/asan-build.log 2>&1 || {
	echo "BUILD FALLITA"
	tail -30 /tmp/asan-build.log
	exit 1
}
echo "compilata"

PRELOAD=$(gcc -print-file-name=libasan.so)
export LD_PRELOAD="$PRELOAD"
export USE_ZEND_ALLOC=0
export ASAN_OPTIONS="detect_leaks=0:abort_on_error=0:print_summary=1"
export UBSAN_OPTIONS="print_stacktrace=1"

# La suite .phpt gira con il binario php, non con quello preparato da run-tests,
# quindi si eseguono gli script direttamente: interessa il comportamento della
# memoria, non il confronto degli output.
echo
echo "== carico sintetico =="
php -d extension=/src/ext/modules/orma.so -d orma.enabled=1 -d orma.detail=2 \
	-d orma.socket=/percorso/inesistente.sock \
	/src/test/carico.php >/tmp/asan-carico.txt 2>/tmp/asan-carico.err || true
grep -E 'ERROR: AddressSanitizer|runtime error' /tmp/asan-carico.err && esito=1 || esito=0
[ "$esito" = "0" ] && echo "nessun errore di memoria"

echo
echo "== SQL patologico =="
php -d extension=/src/ext/modules/orma.so -d orma.enabled=1 -d orma.detail=1 \
	-d orma.socket=/percorso/inesistente.sock \
	/src/test/fuzz-sql.php >/tmp/asan-fuzz.txt 2>/tmp/asan-fuzz.err || true
tail -2 /tmp/asan-fuzz.txt
if grep -qE 'ERROR: AddressSanitizer|runtime error' /tmp/asan-fuzz.err; then
	echo "TROVATI ERRORI:"
	grep -E 'ERROR: AddressSanitizer|runtime error' /tmp/asan-fuzz.err | head -10
	sed -n '1,40p' /tmp/asan-fuzz.err
	exit 1
fi
echo "nessun errore di memoria"

echo
echo "== uscita anticipata =="
php -d extension=/src/ext/modules/orma.so -d orma.enabled=1 -d orma.detail=2 \
	-d orma.socket=/percorso/inesistente.sock \
	-r 'function a(){ orma_start_span("x"); exit("uscito\n"); } function b(){ orma_start_span("y"); a(); } b();' \
	2>/tmp/asan-exit.err || true
if grep -qE 'ERROR: AddressSanitizer|runtime error' /tmp/asan-exit.err; then
	echo "TROVATI ERRORI:"
	sed -n '1,40p' /tmp/asan-exit.err
	exit 1
fi
echo "nessun errore di memoria"

echo
echo "== ricompilazione normale =="
unset LD_PRELOAD USE_ZEND_ALLOC ASAN_OPTIONS UBSAN_OPTIONS
sh /src/build/compile.sh clean >/dev/null
echo "fatto"
