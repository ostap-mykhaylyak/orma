# Ambiente di prova con php-fpm vero, nginx e WordPress.
#
# Serve a esercitare cio' che CLI e server integrato non toccano: worker che
# vivono migliaia di richieste, fork del master, riuso di arena e buffer.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        nginx \
        php8.5-fpm \
        php8.5-mysqli \
        php8.5-curl \
        php8.5-xml \
        php8.5-mbstring \
        php8.5-zip \
        php8.5-gd \
        mariadb-client \
        curl \
        ca-certificates \
        less \
        apache2-utils \
        procps \
        sqlite3 \
    && rm -rf /var/lib/apt/lists/*

# wp-cli per installare WordPress senza passare dal browser.
RUN curl -sSL -o /usr/local/bin/wp \
        https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar \
    && chmod +x /usr/local/bin/wp

COPY nginx.conf /etc/nginx/sites-available/default

# Il pool di prova sostituisce quello di default, che ascolta sullo stesso
# socket: due pool sullo stesso indirizzo fanno fallire l'avvio di fpm.
RUN rm -f /etc/php/8.5/fpm/pool.d/www.conf
COPY pool.conf /etc/php/8.5/fpm/pool.d/prova.conf

WORKDIR /var/www/html
