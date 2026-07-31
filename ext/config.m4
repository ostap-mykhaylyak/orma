dnl orma — configurazione di build dell'estensione.
dnl Nessuna dipendenza esterna: solo libc. Vedi DESIGN.md §2.

PHP_ARG_ENABLE([orma],
  [se abilitare orma],
  [AS_HELP_STRING([--enable-orma], [Abilita l'estensione APM orma])],
  [no])

if test "$PHP_ORMA" != "no"; then
  AC_DEFINE([HAVE_ORMA], [1], [Definito a 1 se orma e' abilitata])

  PHP_NEW_EXTENSION([orma],
    [orma.c orma_txn.c orma_span.c orma_sql.c orma_hooks.c orma_proto.c orma_sender.c],
    [$ext_shared],
    ,
    [-DZEND_ENABLE_STATIC_TSRMLS_CACHE=1])
fi
