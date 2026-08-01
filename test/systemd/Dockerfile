# Container con systemd come PID 1, per provare l'unit davvero.
#
# I test in docker normale non possono verificarlo: senza init, systemctl non
# ha con chi parlare. Qui invece l'unit viene installata, abilitata, avviata e
# fermata come su una macchina vera.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive
ENV container=docker

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv dbus \
        php8.5-cli php8.5-sqlite3 \
        sqlite3 curl ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*

# systemd in container: si tolgono le unit che non hanno senso qui e che
# rallenterebbero o fallirebbero l'avvio.
RUN find /etc/systemd/system /lib/systemd/system \
        -path /lib/systemd/system/systemd-tmpfiles-setup.service -prune -o \
        \( -name '*getty*' -o -name '*udev*' -o -name '*mount*' \) -exec rm -f {} + 2>/dev/null || true

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
