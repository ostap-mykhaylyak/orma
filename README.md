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

`--init` genera anche il token di accesso all'interfaccia e te lo stampa con l'URL
pronto. L'interfaccia è su `127.0.0.1:8737`.

Per disinstallare tutto — estensione, INI, configurazione e dati raccolti:

```bash
sudo orma stop
sudo orma --purge
```

## Comandi

| | |
|---|---|
| `orma start\|stop\|reload\|restart\|status` | il servizio |
| `orma --init` | configura, installa l'estensione, genera il token |
| `orma --purge` | disinstalla tutto e cancella i dati |
| `orma --enable` / `orma --disable` | sospende e riattiva la raccolta |
| `orma --export <dir>` | genera le pagine come HTML statico |
| `orma --check-config` | valida la configurazione |

I verbi di servizio si scrivono senza trattini, tutto il resto con i trattini.

## Cosa vedi

| Pagina | Cosa mostra |
|---|---|
| Panoramica | tempo di risposta e traffico nel tempo, apdex, classifica delle transazioni per tempo consumato |
| Transazione | l'andamento di una singola pagina, dove va il suo tempo, i suoi trace |
| Database | le query aggregate per forma, già offuscate |
| Esterne | le chiamate uscenti per host |
| Errori | errori ed eccezioni raggruppati per forma, fatali distinti dagli avvisi |
| Tracce | le richieste conservate per intero, con il waterfall |
| Stato | i contatori del daemon: quanto ha ricevuto, quanto ha perso, quanto occupa |

I grafici sono SVG generato dal server: nessuno script, nessuna libreria, niente che
possa non caricarsi.

## Configurazione

Il daemon si configura in `/etc/orma/orma.yaml`, che `--init` genera commentato per
intero. La cosa da mettere a posto subito:

```yaml
socket_group: www-data
```

Senza, il socket resta accessibile a qualunque utente locale, che potrebbe iniettare
telemetria falsa. Il daemon lo segnala all'avvio.

### Accesso all'interfaccia

Il token generato da `--init` si passa in due modi:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8737/
```

oppure una volta sola nel browser come `?token=<token>`, che imposta un cookie per la
navigazione successiva — così il token non resta negli URL, dove finirebbe nei log del
reverse proxy e nella cronologia. Svuotare `ui_token` disattiva la protezione, e il
daemon lo segnala all'avvio.

`/salute` resta sempre accessibile: serve a un supervisore per sapere se il processo
risponde, e non espone dati raccolti.

### Il pannello dietro nginx

orma ascolta solo su `127.0.0.1`: per raggiungerlo dall'esterno serve un proxy. Il
blocco minimo, da mettere in un `server` già esistente:

```nginx
location /orma/ {
    proxy_pass http://127.0.0.1:8737/;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;

    # Il token viaggia in chiaro: senza TLS davanti, esponi il pannello
    # solo su una rete di cui ti fidi.
    proxy_read_timeout 30s;
}
```

Su un sottodominio dedicato, che è più pulito perché evita di dover riscrivere i
percorsi:

```nginx
server {
    listen 443 ssl;
    server_name orma.esempio.it;

    ssl_certificate     /etc/letsencrypt/live/orma.esempio.it/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/orma.esempio.it/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8737;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Per Apache:

```apache
<Location /orma/>
    ProxyPass        http://127.0.0.1:8737/
    ProxyPassReverse http://127.0.0.1:8737/
</Location>
```

Un accorgimento: il token passato come `?token=` finisce nei log di accesso del proxy.
Usalo una volta sola per farti impostare il cookie, oppure passa l'header `Authorization`.

### Senza proxy: l'HTML statico

Quando il proxy non si può mettere — container chiuso, rete non raggiungibile, accesso
solo via SSH — le pagine si generano come file:

```bash
orma --export /tmp/orma-report --minuti 1440
```

Produce `panoramica.html` e tutte le altre, più il dettaglio delle transazioni più
pesanti e i trace conservati, con i **collegamenti relativi**: la directory si copia
dove serve e i link continuano a funzionare. Nessun server, nessun token, nessuna porta.

```bash
# generare e portarsi via il tutto in un colpo solo
orma --export /tmp/orma-report --minuti 1440
tar czf orma-report.tar.gz -C /tmp orma-report

# oppure direttamente dalla macchina locale
ssh server 'orma --export /tmp/r --minuti 1440 && tar cz -C /tmp r' | tar xz
```

I dati sono congelati al momento della generazione, quindi il selettore di intervallo
sparisce e al suo posto compare la data: un selettore che rimanda a pagine inesistenti
sarebbe peggio che non averlo.

### Sospendere la raccolta

```bash
orma --disable    # sospende, senza disinstallare
orma --enable     # riattiva
```

Agiscono su `orma.enabled` nell'INI, non su `extension=`: spegnere non richiede che PHP
sappia dove sta il `.so`, e riaccendere non può fallire perché il file è stato spostato.
Con la raccolta spenta l'estensione resta caricata ma non registra nemmeno l'observer,
quindi il costo è zero misurabile. Entrambi ti ricordano il comando per ricaricare
php-fpm, che serve perché l'INI venga riletto.

### Allarmi

Vengono scritti nel log del daemon con livello `WARN` quando una soglia viene superata,
e `INFO` quando la situazione rientra. Si segnalano le **transizioni**, non lo stato: un
allarme ripetuto ogni minuto diventa rumore che si impara a ignorare, ed è così che si
perdono quelli veri.

```yaml
alert_error_rate_pct: 5
alert_apdex_min: 0.8
alert_p95_ms: 2000
```

Nessuna notifica viene inviata da orma: chi ha già una raccolta log la intercetta da lì.
Le regole non scattano sotto le 20 richieste nella finestra — due errori su tre richieste
non sono un'emergenza, sono un sito senza traffico.

L'estensione si configura nel suo INI:

```ini
orma.app_name=nome-del-sito
orma.detail=1        ; 0 nessuna instrumentazione delle funzioni utente, 1 sopra soglia, 2 tutto
orma.function_ms=5   ; soglia per detail=1
orma.max_depth=5
;orma.ignored_exceptions=DomainException,Miaapp\FlussoInterrotto
```

## Cosa costa

**Su WordPress vero**, misurato con `test/fpm/` — WordPress 7 su php-fpm, 3000 richieste
per giro, con e senza estensione: **fra il 4% e il 9% di throughput** con `detail = 1`.

La memoria dei worker non cresce. Su 3000 richieste con `pm.max_requests = 0`, cioè
senza riciclo, i worker con orma attiva sono cresciuti *meno* di quelli senza: la
crescita che si osserva è riscaldamento di WordPress e opcache, non nostra.

Sui carichi sintetici di `test/overhead.sh`: circa il 5% con `detail = 0` e il 10% con
`detail = 1` su una pagina con molte query; su codice fatto di sole chiamate di funzione
`detail = 1` arriva al 70–80%, che è il caso peggiore possibile ma esiste. La tabella
completa è in [DESIGN.md](DESIGN.md#costo-misurato).

Il costo con `detail = 0` è l'instrumentazione delle query: è il prezzo di sapere quali
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
make asan         # ricompila sotto AddressSanitizer e cerca errori di memoria
make overhead     # misura il costo dell'instrumentazione
make wordpress    # php-fpm vero + WordPress + carico, con giro di controllo
make daemon       # binario del daemon
```

`make wordpress` è quello che conta prima di una release: è l'unico che esercita worker
che vivono migliaia di richieste, la fork del master e il riuso dei buffer fra richieste.
`make asan` fa girare l'estensione sotto AddressSanitizer su carico sintetico, SQL
patologico e uscita anticipata: quel codice sta dentro ogni processo PHP del server, e
un accesso fuori dai limiti lì non è un dato sbagliato, è il sito giù.

## Licenza

MIT. Vedi [LICENSE](LICENSE).
