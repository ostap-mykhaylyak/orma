# orma

APM per PHP, self-hosted, indipendente dal framework.

Si aggancia al motore Zend, non all'applicazione: non sa cosa sia WordPress, Laravel o
Symfony, e proprio per questo funziona con tutti e tre e anche con il codice scritto in
casa. Dice dove se ne va il tempo di una richiesta — PHP, query, chiamate di rete — e
conserva per intero le richieste lente e quelle andate male.

Il documento di progetto, con le scelte e il perché, è in [DESIGN.md](DESIGN.md).

## Com'è fatto

```
[ estensione PHP ] --socket unix--> [ daemon Go ] --> [ SQLite ] --> [ UI ]
```

L'estensione misura e consegna; il daemon aggrega, campiona e serve le pagine. Nessuna
dipendenza esterna: l'estensione linka solo la libc, il daemon è un binario statico che
include il motore SQLite.

## Requisiti

PHP 8.5 su Ubuntu 26.04, amd64 o arm64. Non c'è supporto per altre versioni di PHP: una
sola ABI significa due artefatti invece di sedici.

## Installazione

Scarica gli artefatti dalla [pagina delle release](https://github.com/ostap-mykhaylyak/orma/releases),
poi:

```bash
sudo install -m 0755 orma-<versione>-linux-amd64 /usr/local/bin/orma
sudo cp orma-<versione>-php8.5-nts-amd64.so ./orma.so
sudo orma --init
```

`orma --init` scrive la configurazione, copia l'estensione nella `extension_dir` di PHP,
genera l'INI e **verifica che PHP la carichi davvero** prima di lasciarla installata. Se
l'artefatto non corrisponde a quella build di PHP, viene rimosso e te lo dice.

Su Ubuntu l'INI finisce in `mods-available`, quindi:

```bash
sudo phpenmod orma
sudo systemctl restart php8.5-fpm
sudo orma start
```

L'interfaccia è su `127.0.0.1:8737`. **Non ha autenticazione**: tienila dietro a un
reverse proxy o su una rete privata.

## Configurazione

Il daemon si configura in `/etc/orma/orma.yaml`, che `--init` genera commentato per
intero. La cosa da mettere a posto subito:

```yaml
socket_group: www-data
```

Senza, il socket resta accessibile a qualunque utente locale, che potrebbe iniettare
telemetria falsa. Il daemon lo segnala all'avvio.

L'estensione si configura nel suo INI:

```ini
orma.app_name=nome-del-sito
orma.detail=1        ; 0 nessuna instrumentazione delle funzioni utente, 1 sopra soglia, 2 tutto
orma.function_ms=5   ; soglia per detail=1
orma.max_depth=5
;orma.ignored_exceptions=DomainException,Miaapp\FlussoInterrotto
```

## Cosa costa

Misurato con `test/overhead.sh`, su una pagina con molte query: **circa il 5% con
`detail = 0`, il 10% con `detail = 1`**. Su codice fatto di sole chiamate di funzione
`detail = 1` arriva al 70–80%: è il caso peggiore possibile, ma esiste. La tabella
completa e cosa significa sono in [DESIGN.md](DESIGN.md#costo-misurato).

Il 5% di `detail = 0` è l'instrumentazione delle query: è il prezzo di sapere quali
girano. Se ti serve il minimo assoluto, `orma.enabled = 0` costa zero misurabile.

## API per l'applicazione

Utili dove l'instrumentazione automatica non arriva. Nessuna fallisce in modo visibile:
con la raccolta disattivata restituiscono un valore neutro, quindi non serve
proteggerle con `function_exists()`.

```php
orma_name_transaction('checkout/pagamento');   // front controller con un solo URL
orma_background_transaction('coda/ordini');    // consumer, cron, worker
orma_ignore();                                 // health check, endpoint di rumore
orma_add_attribute('cliente', 'ACME');
$s = orma_start_span('calcolo spedizione'); /* ... */ orma_end_span($s);
orma_notice_error('pagamento rifiutato', $eccezione);
orma_get_trace_id();
orma_get_traceparent();                        // header W3C per propagare a valle
```

## Privacy

Quello che **non** lascia mai il processo PHP:

- i valori dei parametri SQL, sostituiti con `?` dall'estensione prima della
  serializzazione, non dal daemon;
- percorso, query string e credenziali delle chiamate uscenti: si registra il solo host;
- la query string degli URL, che non entra nel nome della transazione.

## Distributed tracing

L'header `traceparent` in ingresso viene adottato: la richiesta continua la traccia del
servizio che l'ha chiamata. In uscita **non** c'è iniezione automatica, perché
sovrascrivere gli header di una chiamata curl altrui è un modo eccellente per rompere
un'applicazione. Usa `orma_get_traceparent()` dove ti serve propagare.

## Sviluppo

L'estensione si compila sempre in container: il glibc dell'immagine di build non deve
essere più recente di quello di produzione.

```bash
make ext          # compila l'estensione in ubuntu:26.04
make ext-test     # i test .phpt
make test         # vet e test del daemon
make smoke        # prova end-to-end: PHP vero, socket, daemon, SQLite, pagine
make daemon       # binario del daemon
```

## Licenza

Non ancora decisa.
