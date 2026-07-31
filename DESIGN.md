# orma — APM per PHP, indipendente dal framework

APM self-hosted per applicazioni PHP, modellato su New Relic: un'estensione C che si
aggancia al runtime Zend, un daemon Go che aggrega, SQLite come storage, UI web nello
stesso binario.

Non sa nulla di WordPress, Laravel o Symfony. Si aggancia sotto, al motore.

---

## 1. Architettura

```
┌─────────────────────┐
│  php-fpm worker     │
│  ┌───────────────┐  │
│  │   orma.so     │  │  buffer span in-process
│  └───────┬───────┘  │
└──────────┼──────────┘
           │ unix socket, write non bloccante, drop se lento
           ▼
┌─────────────────────┐
│   orma (daemon Go)  │  ring buffer → aggregazione 60s → campionamento trace
└──────────┬──────────┘
           ▼
┌─────────────────────┐
│   SQLite (WAL)      │  metriche rollup + trace + slow SQL + errori
└──────────┬──────────┘
           ▼
┌─────────────────────┐
│   UI HTTP           │  template server-side
└─────────────────────┘
```

Il principio non negoziabile: **l'estensione non deve mai poter rallentare o rompere
una richiesta**. Niente I/O di rete sincrono verso l'esterno, niente allocazioni non
limitate, niente errori propagati a userland. Se qualcosa non va, si droppa e si conta.

---

## 2. Estensione (`ext/`)

### Target

**PHP 8.5, NTS e ZTS. Ubuntu amd64 e arm64. Nient'altro.**

Un solo minore di PHP significa un solo `ZEND_MODULE_API_NO`, quindi una sola ABI: la
matrice di build collassa a 4 artefatti (2 arch × NTS/ZTS), e in pratica a 2, perché
`php8.5-fpm` su Ubuntu è NTS.

La `zend_observer` API (8.0+, matura in 8.5) è il motivo per cui non serve sostituire
`zend_execute_ex`: si registra un observer per `op_array` una volta sola, e l'overhead
a regime resta basso.

L'estensione non deve linkare **nulla oltre alla libc**. Niente OpenSSL, niente
libcurl, niente protobuf: la serializzazione è scritta a mano proprio per questo. Un
artefatto che dipende solo da libc è un artefatto che si copia e funziona.

### Confini della transazione

| Fase | Azione |
|---|---|
| `MINIT` | lettura INI, registrazione observer, patch degli handler interni |
| `RINIT` | apertura transazione, clock monotonico (`CLOCK_MONOTONIC`), arena span |
| `RSHUTDOWN` | chiusura root span, serializzazione, flush su socket |
| `MSHUTDOWN` | chiusura socket |

Le esecuzioni CLI e cron diventano **background transaction**, nominate dal path dello
script invece che dall'URI.

### Naming delle transazioni

È il punto critico quando non fai framework detection: se nomini per URI grezzo, la
cardinalità esplode e le metriche diventano inutili.

Normalizzazione a template, in ordine:

1. segmento tutto numerico → `{id}`
2. UUID / ULID → `{uuid}`
3. hash esadecimale ≥ 16 char → `{hash}`
4. segmento con estensione nota (`.php`, `.html`) → mantenuto
5. cap a N segmenti, il resto collassa in `/*`

Poi un **limite globale di cardinalità** nel daemon: oltre X nomi distinti per app in
una finestra, i nuovi confluiscono in `OtherTransaction/*`. Senza questa valvola prima
o poi ti si riempie lo storage.

Override esplicito da userland con `orma_name_transaction()`.

### Instrumentazione delle funzioni interne

Il valore vero dell'APM sta qui, e non richiede l'observer: si sostituisce il puntatore
`internal_function.handler` nella function table (globale, o della classe per i metodi).

| Categoria | Target |
|---|---|
| Database | `PDO::query`, `PDO::exec`, `PDOStatement::execute`, `mysqli_query`, `mysqli_real_query`, `mysqli_stmt_execute`, `pg_query`, `pg_query_params` |
| External | `curl_exec`, `curl_multi_exec`, `fsockopen`, `stream_socket_client`, `file_get_contents` (solo su wrapper http/https) |
| Cache | `Memcached::get/set`, `Redis::*` (se le classi esistono) |
| Altro | `mail` |

Per ognuna: si apre uno span CLIENT, si chiama l'handler originale, si chiude lo span
con durata ed esito. L'handler originale va salvato **prima** di sostituirlo e chiamato
sempre, anche se la nostra logica fallisce.

### Instrumentazione delle funzioni utente

Via `zend_observer_fcall_register`, che registra gli handler **una volta per `op_array`**
e non a ogni chiamata: è per questo che si può osservare tutto il codice utente senza
sostituire `zend_execute_ex`.

Il costo resta però reale — due letture di orologio per chiamata di funzione — da cui
tre livelli invece dei due previsti inizialmente:

| `orma.detail` | Comportamento |
|---|---|
| `0` | nessun observer registrato, costo zero |
| `1` (default) | si cronometra solo entro `orma.max_depth`, e si emette uno span solo sopra `orma.function_ms` |
| `2` | si emette ogni chiamata, fino al tetto degli span |

### Costo misurato

Il progetto iniziale dichiarava «overhead target < 3%». **Non è raggiunto**, e la stima
era basata su nulla. Numeri veri, da `test/overhead.sh` su PHP 8.5 in container:

| | sole chiamate di funzione | pagina con molte query |
|---|---|---|
| `orma.enabled = 0` | ~0% | ~0% |
| `detail = 0` | ~0% | **5%** |
| `detail = 1` | **70–80%** | **10%** |
| `detail = 2` | 160–180% | 13% |

Il primo carico è deliberatamente il caso peggiore: mezzo milione di chiamate di
funzione in trenta millisecondi, dove qualunque lavoro per chiamata pesa enormemente. Il
secondo è la misura che conta per scegliere un default.

Il 5% di `detail = 0` **non** è l'observer, che lì non è nemmeno registrato: è
l'instrumentazione di PDO, cioè il prezzo di sapere quali query girano.

Tre cose imparate misurando, tutte controintuitive abbastanza da non essere state
previste:

1. **Registrare un observer costa anche se non si osserva.** `zend_observer_fcall_register`
   cambia il percorso di chiamata del motore per ogni funzione. Chiamarla in `MINIT`
   incondizionatamente costava l'8% anche con `orma.enabled = 0`. Ora la registrazione è
   condizionata.
2. **Due letture di orologio per chiamata erano una di troppo.** L'istante assoluto di
   inizio si ricava da quello della transazione più lo scarto monotonico: stesso valore,
   una syscall in meno. Vale venti punti percentuali sul caso peggiore.
3. **Un frame non cronometrato non va scritto.** Oltre `max_depth` basta contare le
   chiamate saltate, invece di riempire una struttura che nessuno leggerà.

**Perché la soglia non produce span orfani:** la durata di un genitore è sempre maggiore
o uguale a quella di ogni suo discendente. Se un genitore sta sotto soglia, ci stanno
anche tutti i figli, e nessuno di loro è stato emesso. Gli orfani restano possibili solo
quando è il tetto degli span a troncare, e la UI li riattacca alla radice.

### Errori

- `zend_error_cb` per fatal, warning, deprecation
- `zend_throw_exception_hook` per le eccezioni (registrando anche quelle poi catturate,
  marcandole diversamente da quelle uncaught)

### Buffer e flush

- Arena di span pre-allocata, cap a 2000 span per transazione. Oltre: si tronca e si
  incrementa `spans_dropped`, che viene riportato — un trace troncato dichiarato è utile,
  uno troncato in silenzio è una bugia.
- Il socket si apre **lazy al primo flush del processo**, mai ereditato dal padre in
  prefork. Su `EPIPE`/`ECONNRESET` si richiude e si riprova una volta sola.
- `O_NONBLOCK` + `SO_SNDTIMEO` di pochi ms. Se il daemon non consuma, si droppa.

### Configurazione (`orma.ini`)

```ini
orma.enabled     = 1
orma.app_name    = "sito-produzione"
orma.socket      = "/run/orma/orma.sock"
orma.detail      = 1     ; 0 nessuna, 1 solo sopra soglia, 2 tutto
orma.function_ms = 5     ; soglia per detail = 1
orma.max_depth   = 5     ; profondita' oltre la quale non si cronometra
```

Soglie di campionamento, conservazione dei trace e tetto di cardinalità stanno invece
nella configurazione del daemon (`orma.yaml`): sono decisioni di raccolta, non di
instrumentazione, e cambiarle non deve richiedere un riavvio di php-fpm.

**L'offuscamento SQL non è configurabile.** Era previsto come direttiva
`orma.obfuscate_sql`; è stato tolto perché una direttiva che si può mettere a zero è una
direttiva che prima o poi qualcuno mette a zero in produzione.

### API userland

```php
orma_name_transaction(string $nome): bool
orma_background_transaction(string $nome): bool
orma_ignore(): bool
orma_add_attribute(string $chiave, string|int|float|bool $valore): bool
orma_start_span(string $nome): int
orma_end_span(int $riferimento): bool
orma_notice_error(string $messaggio, ?Throwable $eccezione = null): bool
orma_get_trace_id(): ?string
```

**Nessuna di queste fallisce in modo visibile.** Con `orma.enabled = 0`, o fuori da una
transazione, restituiscono un valore neutro senza warning né eccezioni: un'applicazione
che le chiama non deve doverle proteggere con `function_exists()`.

`orma_start_span` non prende una categoria, come previsto nella prima stesura. Le
categorie si deducono dagli attributi semantici (`db.statement`, `server.address`), e
lasciare che userland ne dichiari una permetterebbe di creare span che si dichiarano
tempo di database senza avere una query dietro.

---

## 3. Protocollo agent → daemon

Binario, little-endian, frame length-prefixed. Modello dati ricalcato su OpenTelemetry
così che un ingest OTLP possa essere aggiunto in seguito senza toccare lo storage.

```
frame  := u32 len | u8 version | u8 flags | payload
payload:= string_table | transaction | span[]
```

La **string table** è per-payload: nomi di funzione, tabelle e host si ripetono molto,
internarli taglia il payload di parecchio.

**Span**

| Campo | Tipo | Note |
|---|---|---|
| `trace_id` | 16 byte | |
| `span_id` | 8 byte | |
| `parent_span_id` | 8 byte | 0 per il root |
| `name` | idx string table | |
| `kind` | u8 | SERVER / CLIENT / INTERNAL |
| `start_unix_nano` | u64 | |
| `duration_nano` | u64 | da clock monotonico |
| `status` | u8 | ok / error |
| `attributes` | k/v | chiavi OTel semantic conventions |

Chiavi attributo: `db.system`, `db.statement`, `db.operation`, `http.request.method`,
`url.full`, `server.address`, `code.function`, `code.filepath`, `code.lineno`.

**Transaction** = root span di kind SERVER + metadati: app, hostname, pid, status HTTP,
memoria di picco, CPU user/sys, conteggio errori, `spans_dropped`.

---

## 4. Daemon (`internal/`)

### Ingest

Unix socket (TCP opzionale per i container), un goroutine per connessione, decodifica in
un ring buffer con backpressure = drop e contatore esposto.

### Aggregazione

Finestra di **60 secondi**. Per ogni chiave `(app, transazione, categoria)`:
count, sum, sum², min, max, più un istogramma per i percentili (p50/p95/p99).

Da sum e sum² si ricava la deviazione standard senza tenere i campioni. Apdex calcolato
sulla soglia `apdex_t` configurata per app.

### Campionamento dei trace

Il trace completo si salva **solo se** almeno una condizione è vera:

1. durata > `trace_threshold_ms`
2. la transazione ha prodotto un errore
3. è la più lenta del minuto per quel nome di transazione

La terza regola vale solo per le prime `trace_slowest_names` (default 5) transazioni
distinte del minuto: senza un tetto, un'applicazione con molti nomi conserverebbe un
trace per ciascuno a ogni minuto. Serve a non restare completamente ciechi sulle
transazioni veloci, che per definizione non superano mai la soglia.

Tutto il resto contribuisce solo alle metriche aggregate. È questa regola che rende lo
storage sostenibile: le metriche crescono con il numero di transazioni distinte, non con
il traffico.

### Slow SQL

**L'offuscamento avviene nell'estensione, non nel daemon.** Il design iniziale lo
metteva nel daemon; è stato spostato in `ext/orma_sql.c` perché è strettamente più
sicuro: così i valori dei parametri non lasciano mai il processo PHP che li ha in mano,
e il daemon non può scriverli su disco nemmeno per errore, nemmeno in un log di debug.

L'estensione sostituisce letterali e numeri con `?`, rimuove i commenti e comprime gli
spazi. Il daemon riceve solo la forma già offuscata, la hasha (FNV-1a) e la aggrega.

---

## 5. Storage (SQLite, WAL)

```
apps(id, name, apdex_t, created_at)
metrics_1m(app_id, bucket_ts, txn_name, kind, category,
           count, errors, sum_ns, sumsq_ms, min_ns, max_ns, histogram BLOB)
metrics_5m(...)      -- stessa forma
metrics_1h(...)      -- stessa forma
traces(id, app_id, ts, txn_name, kind, duration_ns, http_status, has_error, spans TEXT)
slow_sql(app_id, bucket_ts, stmt_hash, statement, count, errors, sum_ns, max_ns)
externals(app_id, bucket_ts, host, count, errors, sum_ns, max_ns)
errors(app_id, bucket_ts, fingerprint, class, message, file, line, txn_name, severity, count)
```

`kind` distingue web da background; `category` è la scomposizione (`totale`,
`database`, `esterne`). Sono due dimensioni diverse e tenerle nella stessa colonna,
come nella prima stesura, rendeva impossibile chiedere "il tempo in database delle sole
transazioni web".

### Rollup: ricalcolo, non avanzamento

Il rollup **ricalcola gli ultimi N bucket completi** invece di tenere un segnalibro di
avanzamento: 1m → 5m ogni minuto sugli ultimi 3 bucket, 5m → 1h ogni minuto sugli
ultimi 2. È idempotente, quindi un giro saltato viene rimediato dal successivo e non
esiste uno stato da riparare a mano quando qualcosa va storto.

Il rollup **non consuma** la granularità fine: è la purga a rimuoverla quando scade.
Così una query su 24 ore trova ancora il minuto, e una su 30 giorni trova l'ora.

Le pagine scelgono la tabella in base all'intervallo chiesto. Da qui il vincolo,
verificato da `--check-config`: **ogni livello di conservazione deve coprire almeno fino
a dove comincia il successivo**, altrimenti resta un buco nel quale le pagine non
trovano nulla.

`slow_sql`, `externals` ed `errors` non vengono aggregati: la loro cardinalità è
limitata dal numero di forme distinte, non dal traffico. Hanno solo un TTL. Se un giorno
crescessero, il passo successivo è portarli a bucket orari.

Retention di default: `metrics_1m` 24h, `metrics_5m` 7g, `metrics_1h` 395g,
`traces` 7g, `errors` 30g, `slow_sql` ed `externals` 7g.

---

## 6. UI

| Pagina | Contenuto |
|---|---|
| Panoramica | throughput, response time (p50/p95/p99), error rate, apdex |
| Transazioni | classifica per **tempo totale consumato**, non per durata media |
| Dettaglio transazione | breakdown per categoria nel tempo, lista dei trace campionati |
| Trace | waterfall degli span |
| Database | slow SQL aggregate per hash |
| External | chiamate uscenti per host |
| Errori | error traces raggruppati per classe e messaggio |

La classifica per tempo consumato è la scelta importante: una pagina da 3 secondi
chiamata due volte al giorno conta meno di una da 300 ms chiamata diecimila volte.

---

## 7. CLI

Verbi di servizio senza trattini, tutto il resto con i trattini:

```
orma start | stop | reload | restart | status
orma --init
orma --check-config
orma --version
```

---

## 8. Build e distribuzione

### Regola che governa tutto: il glibc è compatibile solo in avanti

Un binario compilato contro glibc 2.41 **non** si carica su un sistema con glibc 2.39:
il linker cerca versioni di simbolo che lì non esistono. Il contrario invece funziona.

Quindi l'immagine di build deve essere **la stessa release di Ubuntu del target di
produzione, o più vecchia**. Non le immagini ufficiali `php:8.5-*`, che sono basate su
Debian e hanno un glibc diverso da quello di Ubuntu.

Il target è **Ubuntu 26.04 (Resolute)**, che ha PHP 8.5 nell'archivio ufficiale: niente
PPA di terze parti, e l'archivio Ubuntu è nativamente multi-arch, quindi `php8.5-dev`
esiste sia per amd64 sia per arm64.

Immagine di build: `ubuntu:26.04` + `php8.5-dev` + `build-essential`. Nient'altro.

### Loop interno (locale, Docker Desktop)

```
docker run --rm -v ~/orma:/src -w /src/ext orma-build \
  sh -c 'phpize && ./configure && make -j && make test'
```

Secondi, non minuti. Questo è dove si sviluppa.

### Loop esterno (GitHub Actions)

Repo pubblico → runner arm64 nativi gratuiti, nessuna emulazione QEMU.

| Job | Runner | Quando |
|---|---|---|
| test | `ubuntu-24.04` | ogni push: `make test` sui `.phpt`, più i test Go |
| build | `ubuntu-24.04` + `ubuntu-24.04-arm` | su tag |
| release | — | allega gli `.so` e il binario del daemon alla GitHub Release |

Artefatti: `orma-<versione>-php8.5-<nts|zts>-<amd64|arm64>.so` più il binario Go
`orma-<versione>-linux-<arch>`.

L'installazione sul target è copiare l'`.so` in `extension_dir` e l'`orma.ini` in
`conf.d`. Se ne occupa `orma --init`, che legge `extension_dir` e la versione API
direttamente da `php -i` e rifiuta di procedere se non corrispondono.

---

## 9. Milestone

| | Obiettivo | Fatto quando |
|---|---|---|
| **M0** | Scheletro | `config.m4` compila una `.so` vuota che appare in `php -m`; CLI Go con `--init`/`--check-config` e verbi di servizio; il daemon accetta sul socket e scarta |
| **M1** | Transazione base | RINIT/RSHUTDOWN, naming a template, memoria e status, flush non bloccante; il daemon decodifica e scrive `metrics_1m`; pagina Panoramica viva |
| **M2** | Span I/O | patch degli handler interni per curl/PDO/mysqli/pg; categorie Database ed External; `sqlnorm`; pagine Database ed External |
| **M3** | Span funzioni utente | `zend_observer` con policy detail; waterfall del trace nella UI |
| **M4** | Errori | `zend_error_cb` + throw hook; error traces; error rate e apdex |
| **M5** | Campionamento e retention | soglie, slowest-of-minute, rollup a cascata, purge, TTL |
| **M6** | API userland | le funzioni `orma_*`, background transaction, CLI/cron |
| **M7** | Distribuzione | release su tag con i 4 artefatti, `orma --init` che installa e verifica l'ABI, distributed tracing `traceparent`, hardening |

---

## 10. Trappole note

- **glibc**: vedi §8. È la trappola numero uno di questo progetto, perché si manifesta
  solo al deploy e non in CI.
- **ABI**: la `.so` è legata a `ZEND_MODULE_API_NO` e ai flag ZTS/debug. Con il solo
  PHP 8.5 il problema è contenuto, ma `orma --init` deve comunque verificarlo invece di
  lasciare che sia `php` a fallire al caricamento.
- **Fatal error**: `RSHUTDOWN` viene comunque eseguito, quindi il flush avviene. Su
  segfault no: si perde la transazione, ed è accettabile.
- **opcache**: convive con l'observer, ma la registrazione va fatta in `MINIT`.
- **php-fpm prefork**: il socket va aperto nel worker, mai ereditato. Vedi flush lazy.
- **Cardinalità**: senza il cap globale nel daemon, un'app con URL generati porta lo
  storage a saturazione in giorni.
- **Privacy**: l'offuscamento SQL è attivo di default e i parametri delle query non
  vengono mai serializzati. Stesso discorso per le query string negli `url.full`.
