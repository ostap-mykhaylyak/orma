--TEST--
orma: con la raccolta disattivata le funzioni non fanno nulla e non falliscono
--EXTENSIONS--
orma
--INI--
orma.enabled=0
--FILE--
<?php
// Un'applicazione che chiama queste funzioni non deve doverle proteggere:
// con orma.enabled=0 tornano un valore neutro senza warning ne' eccezioni.
var_dump(orma_name_transaction('checkout'));
var_dump(orma_background_transaction('coda'));
var_dump(orma_ignore());
var_dump(orma_add_attribute('cliente', 'ACME'));
var_dump(orma_start_span('elaborazione'));
var_dump(orma_end_span(0));
var_dump(orma_notice_error('qualcosa'));
var_dump(orma_get_trace_id());
echo "nessun avviso\n";
?>
--EXPECT--
bool(false)
bool(false)
bool(false)
bool(false)
int(-1)
bool(false)
bool(false)
NULL
nessun avviso
