# orma — build dell'estensione (in container) e del daemon (nativo).
#
# L'estensione si compila sempre dentro l'immagine ubuntu:26.04 di build/, mai
# sull'host: il glibc dell'immagine non deve essere piu' recente di quello del
# target di produzione. Vedi DESIGN.md §8.

IMAGE   ?= orma-build
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo sviluppo)

MODULE  := github.com/ostap-mykhaylyak/orma
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) \
           -X $(MODULE)/internal/version.Commit=$(COMMIT)

DOCKER_RUN = docker run --rm -v "$(CURDIR)":/src -w /src/ext $(IMAGE) sh -c

.PHONY: all image ext ext-test ext-clean daemon test smoke asan overhead wordpress systemd clean

all: ext daemon

image:
	docker build -t $(IMAGE) build

ext: image
	docker run --rm -v "$(CURDIR)":/src $(IMAGE) sh /src/build/compile.sh

ext-test: ext
	$(DOCKER_RUN) 'NO_INTERACTION=1 REPORT_EXIT_STATUS=1 make test'

ext-clean:
	-$(DOCKER_RUN) 'test -f Makefile && make distclean; phpize --clean'

daemon:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/orma ./cmd/orma

test:
	go vet ./...
	go test ./...

# Prova end-to-end: avvia il daemon nel container, gli manda un frame sul socket
# e verifica che lo conti e si fermi pulito.
smoke: image daemon
	docker run --rm -v "$(CURDIR)":/src $(IMAGE) sh /src/test/smoke.sh

# L'estensione gira dentro ogni processo PHP: un accesso fuori dai limiti non
# e' un dato sbagliato, e' il sito giu'.
asan: image
	docker run --rm -v "$(CURDIR)":/src $(IMAGE) sh /src/build/asan.sh

overhead: image daemon
	docker run --rm -v "$(CURDIR)":/src $(IMAGE) sh /src/test/overhead.sh

# php-fpm vero, WordPress vero, con giro di controllo a estensione spenta.
# E' l'unico test che esercita worker vivi migliaia di richieste.
wordpress: ext daemon
	cd test/fpm && docker compose down -v
	cd test/fpm && docker compose up --build --abort-on-container-exit --exit-code-from web

# systemd come PID 1: senza init, systemctl non ha con chi parlare e l'unit
# resterebbe verificata solo sulla carta.
systemd: ext daemon
	docker build -t orma-systemd test/systemd
	-docker rm -f orma-sd
	docker run -d --name orma-sd --privileged --cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$(CURDIR)":/src:ro orma-systemd
	sleep 8
	docker exec orma-sd sh /src/test/systemd/prova.sh
	docker rm -f orma-sd

clean: ext-clean
	rm -rf dist
