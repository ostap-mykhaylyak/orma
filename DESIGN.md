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

**Da dove si legge l'URI.** Non da `SG(request_info).request_uri`: su php-fpm quel campo
contiene lo `SCRIPT_NAME`, non l'URI richiesto. Con un front controller — cioè con
WordPress, Laravel, Symfony e qualunque applicazione moderna — *tutte* le pagine
diventerebbero `/index.php` e le metriche non direbbero più nulla. È esattamente ciò che
succedeva, ed è emerso solo provando su php-fpm vero.

L'URI si legge da `REQUEST_URI` nell'ambiente della richiesta, con `sapi_getenv`, che non
costringe a materializzare `$_SERVER`. Il vecchio campo resta come ripiego per i SAPI che
non espongono l'ambiente.

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
internarli taglia il payload di parecchio. L'**indice 0 è sempre la stringa vuota**: è
così che un campo facoltativo dice "assente", ed è anche il ripiego quando la tabella si
riempie. La casella va occupata a mano in testa al frame — internare `""` restituisce 0
senza inserire nulla, e per tre versioni l'indice 0 è finito alla prima stringa vera,
facendo comparire il nome dell'applicazione al posto dei campi assenti.

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
| `chiamate` | u32 | funzioni utente eseguite dentro lo span |
| `interne_nano` | u64 | tempo nelle funzioni interne profilate |
| `definizione` | idx string + u32 | file e riga dove la funzione è scritta |
| `pila` | u8 n + n×(idx string + u32) | da dove è partita la chiamata, fino a 5 livelli, uno per file |
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
| Panoramica | andamento nel tempo, throughput, p50/p95/p99, error rate, apdex, classifica delle transazioni |
| Transazione | l'andamento di una singola pagina, dove va il suo tempo, i suoi trace |
| Trace | waterfall degli span |
| Database | slow SQL aggregate per forma |
| Esterne | chiamate uscenti per host |
| Errori | errori ed eccezioni raggruppati per forma |
| Stato | contatori del daemon: ricevuto, perso, occupato |

La classifica per tempo consumato è la scelta importante: una pagina da 3 secondi
chiamata due volte al giorno conta meno di una da 300 ms chiamata diecimila volte.

**I grafici sono SVG generato lato server.** Nessuno script, nessuna libreria: la
geometria si calcola in Go e il template emette `path` e `rect`. Una pagina di
diagnostica che dipende da un bundle JavaScript è una pagina che può non caricarsi
proprio quando serve.

Un dettaglio che cambia cosa racconta il grafico: gli intervalli senza traffico
**interrompono** la linea invece di essere disegnati a zero. Unire due punti attraverso
un buco disegnerebbe una pendenza che non è mai esistita.

### Accesso

Token unico in configurazione, generato da `--init`. Si passa come
`Authorization: Bearer` oppure una volta sola come `?token=`, che imposta un cookie: così
il token non resta negli URL, dove finirebbe nei log del reverse proxy e nella cronologia
del browser. Il confronto è a tempo costante.

`/salute` resta fuori dall'autenticazione: serve a un supervisore per sapere se il
processo risponde, e non espone dati raccolti.

### Perché è lento, non solo dove

Il waterfall mostra l'annidamento, e da solo non basta. Un metodo che dura cinque secondi
con sotto un figlio che ne dura cinque lascia esattamente dove si era. Tre aggiunte, in
ordine di costo crescente:

**Il tempo proprio** — durata meno quella dei figli registrati. Si calcola nel daemon
dall'albero che già esiste, quindi costa zero all'agent. Le righe con tempo proprio alto
in proporzione sono marcate: il rallentamento è lì e non più in basso.

**Il conteggio delle chiamate per span** — quante funzioni utente sono state eseguite
dentro, comprese quelle rimaste sotto soglia. Si ottiene in tempo costante con un
contatore globale e uno scarto per frame, senza risalire la pila. Distingue *lento perché
fa moltissimo* da *lento perché aspetta*: due diagnosi opposte che senza questo numero
hanno lo stesso aspetto.

**Il profilo delle funzioni interne** — quante volte e per quanto tempo sono state
chiamate le funzioni che possono davvero costare: `preg_*`, `json_*`, serializzazione,
filesystem, compressione, hash, immagini, attese. L'elenco è curato a mano ed esclude le
funzioni banali chiamate a milioni, dove cronometrare costerebbe più che eseguire.

Non produce span — sarebbero migliaia — ma un totale per funzione, che è la forma in cui
l'informazione si legge. Su una homepage WordPress: 162 `file_exists`, 140 `json_decode`
e 1409 `preg_match` per richiesta. Costa due letture di orologio per chiamata delle sole
funzioni in elenco; misurato su WordPress, l'overhead resta dentro l'intervallo già
osservato senza profilo.

**Le posizioni nel codice** — per ogni span, il file e la riga dove la funzione è
*definita*, e il punto da cui è stata *chiamata*. Sono due domande diverse: la prima dice
di chi è il codice — quale plugin, quale tema — la seconda dice chi lo ha voluto. Le query
e le connessioni non hanno una definizione, esistono nel motore; hanno però un chiamante,
ed è quello che serve.

Della pila si registrano **cinque livelli, uno per file**. Il chiamante immediato di una
query è quasi sempre l'astrazione del framework, e contare i frame non basta: dentro
`class-wpdb.php` la catena `query` → `_do_query` → … occupa da sola tre livelli, e il
risultato dice tre volte che la query passa da `wpdb`. Saltando i frame che stanno nello
stesso file del livello precedente — del file si tiene il frame più basso, quello con la
riga esatta — si esce dall'astrazione e si arriva al plugin. La risalita è limitata a 64
frame, perché una ricorsione dentro un solo file non deve costare più della chiamata.

La raccolta non alloca: i percorsi vanno nell'arena già usata per i nomi, deduplicati per
hash, e nel frame diventano indici della string table.

Da queste posizioni il daemon deduce il **componente**: il segmento che segue
`plugins/`, `mu-plugins/`, `themes/`, `modules/`, `extensions/`, oppure i due che seguono
`vendor/`. È la risposta alla domanda che ci si fa davvero davanti a una query lenta —
quale plugin la esegue — e sta già dentro il percorso. Il core del framework non somiglia
a nessuno di questi schemi e viene scavalcato, che è esattamente il comportamento voluto.

Nella UI i percorsi si mostrano relativi alla radice comune del trace: su
un'installazione tipica ogni riga comincerebbe con gli stessi cinquanta caratteri, che si
leggono una volta sola.

### L'analisi la fa il programma

Le tre aggiunte sopra danno i dati; leggerli resta lavoro meccanico. Guardare quale
funzione interna domina, contare le query ripetute, cercare gli span con molto tempo
proprio: sono tutte cose che si fanno con una regola, e le regole le esegue il computer.

Da qui i **rilievi**: la pagina di un trace comincia con le osservazioni già fatte,
ordinate per tempo recuperabile, ognuna con il perché e cosa farci. Le soglie sono
relative alla durata della richiesta e non assolute — su una da dodici secondi cento
millisecondi sono rumore, su una da duecento sono un terzo del problema.

Un trace da mille righe però non si guarda comunque. Tre accorgimenti:

- le query identiche ripetute sotto lo stesso genitore si raccolgono in una riga con
  `×n`, perché sono la firma di un N+1 e da sole riempiono il waterfall;
- il filtro per durata nasconde le righe brevi **tenendo quelle che portano a una
  visibile**: nascondere un genitore lascerebbe i figli senza contesto;
- il riepilogo delle query raggruppa per forma.

E se il tetto degli span ha troncato la raccolta, la pagina lo dichiara. È lo stesso
principio dell'auto-osservazione: un waterfall incompleto che non dice di esserlo fa
cercare a lungo qualcosa che non c'è.

### Auto-osservazione

Un APM che non sa dire se sta perdendo dati **mente per omissione**: se il socket satura
o il disco si riempie, tutte le altre pagine continuano a mostrare numeri plausibili ma
incompleti, e niente lo segnala.

Da qui il campo che l'agent aggiunge a ogni frame: **quante transazioni non è riuscito a
consegnare** dall'ultima consegna riuscita. È l'unica informazione che il daemon non
potrebbe dedurre da solo — ciò che non gli è arrivato, per definizione, non gli è
arrivato. Il contatore si azzera a ogni consegna riuscita, quindi il daemon somma delta
e non deve inseguire pid che si riciclano.

La pagina Stato mostra quel numero insieme ai frame rifiutati e alle finestre perse in
scrittura, e dichiara la raccolta «con perdite» invece di «completa» appena uno dei tre
è diverso da zero.

**Le perdite sono distinte per causa**, perché hanno rimedi diversi: connessione fallita
significa daemon fermo o permessi sbagliati sul socket; timeout significa macchina carica
o budget troppo stretto, e allora si alza `orma.send_timeout_ms`; errore di scrittura
significa socket caduto sotto l'agent, tipicamente un riavvio del daemon. La pagina
traduce la causa prevalente in una frase operativa: un contatore che sale senza dire cosa
farci è un contatore che si impara a ignorare.

### Allarmi

Valutati ogni minuto sulle metriche recenti e scritti nel **log del daemon**: `WARN` al
superamento, `INFO` al rientro. Nessuna notifica viene inviata da orma — chi ha già una
raccolta log la intercetta da lì, e costruirsi una propria catena di notifiche
significherebbe duplicare male qualcosa che esiste già meglio altrove.

Si segnalano le **transizioni**, non lo stato: un allarme ripetuto ogni minuto diventa
rumore che si impara a ignorare, ed è così che si perdono quelli veri. Sotto le 20
richieste nella finestra le regole non scattano: due errori su tre richieste non sono
un'emergenza, sono un sito senza traffico.

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
- **String table, indice 0**: deve contenere la stringa vuota, e va inserita
  esplicitamente. Il test del protocollo costruisce i frame a mano e la metteva al posto
  giusto, quindi passava; il difetto è uscito solo dalla prova end-to-end, dove i campi
  assenti mostravano il nome dell'applicazione. È il motivo per cui `test/smoke.sh` resta
  necessario anche avendo i test unitari.
