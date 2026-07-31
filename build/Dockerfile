# Immagine di build dell'estensione orma.
#
# Deve essere la stessa release di Ubuntu del target di produzione, mai piu' recente:
# il glibc e' compatibile solo in avanti. Vedi DESIGN.md §8.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        autoconf \
        pkg-config \
        file \
        re2c \
        php8.5-dev \
        php8.5-cli \
        sqlite3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
