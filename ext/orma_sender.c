/* Consegna del frame al daemon.
 *
 * Regola che governa questo file: l'estensione non deve mai poter rallentare
 * o rompere una richiesta. Da cui, in ordine di importanza:
 *
 *   - socket non bloccante con un budget di pochi millisecondi, poi si droppa;
 *   - MSG_NOSIGNAL, perche' scrivere su un socket chiuso alzerebbe SIGPIPE e
 *     ucciderebbe il worker php-fpm;
 *   - FD_CLOEXEC, perche' il descrittore non deve sopravvivere a un exec();
 *   - riapertura se il pid e' cambiato, perche' un fd ereditato da una fork
 *     verrebbe condiviso da due processi e i frame si intreccerebbero;
 *   - nessun warning PHP, mai.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_orma.h"
#include "orma_sender.h"
#include "orma_txn.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

void orma_sender_close(void)
{
	int fd = ORMA_G(sock_fd);
	if (fd >= 0) {
		close(fd);
		ORMA_G(sock_fd) = -1;
	}
	ORMA_G(sock_pid) = 0;
}

static int orma_sender_open(void)
{
	const char *path = ORMA_G(socket_path);
	if (path == NULL || *path == '\0') {
		return -1;
	}

	struct sockaddr_un addr;
	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;

	size_t path_len = strlen(path);
	if (path_len >= sizeof(addr.sun_path)) {
		return -1;
	}
	memcpy(addr.sun_path, path, path_len);

	int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
	if (fd < 0) {
		return -1;
	}

	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
		if (errno != EINPROGRESS) {
			close(fd);
			return -1;
		}
		struct pollfd p = { .fd = fd, .events = POLLOUT, .revents = 0 };
		if (poll(&p, 1, ORMA_SEND_TIMEOUT_MS) <= 0 || !(p.revents & POLLOUT)) {
			close(fd);
			return -1;
		}
		int err = 0;
		socklen_t err_len = sizeof(err);
		if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &err_len) < 0 || err != 0) {
			close(fd);
			return -1;
		}
	}

	ORMA_G(sock_fd) = fd;
	ORMA_G(sock_pid) = getpid();
	return fd;
}

static bool orma_write_all(int fd, const char *data, size_t len)
{
	uint64_t deadline = orma_now_monotonic_nano()
	                  + (uint64_t)ORMA_SEND_TIMEOUT_MS * 1000000ULL;
	size_t off = 0;

	while (off < len) {
		ssize_t n = send(fd, data + off, len - off, MSG_NOSIGNAL);
		if (n > 0) {
			off += (size_t)n;
			continue;
		}
		if (n < 0 && errno == EINTR) {
			continue;
		}
		if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
			uint64_t now = orma_now_monotonic_nano();
			if (now >= deadline) {
				return false;
			}
			int remaining_ms = (int)((deadline - now) / 1000000ULL);
			struct pollfd p = { .fd = fd, .events = POLLOUT, .revents = 0 };
			if (poll(&p, 1, remaining_ms > 0 ? remaining_ms : 1) <= 0) {
				return false;
			}
			continue;
		}
		return false;
	}
	return true;
}

void orma_sender_send(const char *data, size_t len)
{
	if (len == 0) {
		return;
	}

	/* Un fd ereditato da una fork non e' nostro. */
	if (ORMA_G(sock_fd) >= 0 && ORMA_G(sock_pid) != getpid()) {
		ORMA_G(sock_fd) = -1;
		ORMA_G(sock_pid) = 0;
	}

	/* Un tentativo, piu' un solo ritentativo dopo riconnessione: il daemon
	 * puo' essere stato riavviato sotto di noi, ma non vale la pena insistere. */
	for (int attempt = 0; attempt < 2; attempt++) {
		int fd = ORMA_G(sock_fd);
		if (fd < 0) {
			fd = orma_sender_open();
			if (fd < 0) {
				orma_sender_drop();
				return;
			}
		}

		if (orma_write_all(fd, data, len)) {
			ORMA_G(sent_frames)++;
			/* Il frame appena consegnato dichiarava quanti se ne erano persi:
			 * ora che il daemon lo sa, si riparte da zero. */
			ORMA_G(dropped_frames) = 0;
			return;
		}

		orma_sender_close();
	}

	orma_sender_drop();
}

void orma_sender_drop(void)
{
	if (ORMA_G(dropped_frames) < UINT32_MAX) {
		ORMA_G(dropped_frames)++;
	}
	ORMA_G(dropped_total)++;
}
