--TEST--
orma: uscita anticipata e stack unwinding dentro funzioni annidate
--EXTENSIONS--
orma
--INI--
orma.enabled=1
orma.detail=2
orma.socket=/percorso/che/non/esiste.sock
--FILE--
<?php
// L'observer tiene una pila di frame. exit() e le eccezioni saltano fuori
// dalle funzioni senza passare dagli handler di uscita, quindi la pila resta
// disallineata: questo test verifica che il disallineamento non faccia danni.

function profonda(int $n): void
{
    $span = orma_start_span("livello $n");
    if ($n > 0) {
        profonda($n - 1);
    }
    // Volutamente senza orma_end_span: gli span restano aperti.
}

function lanciaEccezione(): void
{
    profonda(5);
    throw new RuntimeException('interrotta');
}

try {
    lanciaEccezione();
} catch (RuntimeException $e) {
    echo "eccezione catturata\n";
}

// La transazione deve essere ancora utilizzabile dopo lo srotolamento.
var_dump(orma_name_transaction('dopo-eccezione'));
var_dump(strlen(orma_get_trace_id()));

function esceQui(): void
{
    orma_start_span('mai chiuso');
    exit("uscito dal fondo\n");
}

function intermedia(): void
{
    orma_start_span('intermedia');
    esceQui();
}

intermedia();
echo "questa riga non deve comparire\n";
?>
--EXPECT--
eccezione catturata
bool(true)
int(32)
uscito dal fondo
