/* Consegna del frame al daemon sul socket unix. */

#ifndef ORMA_SENDER_H
#define ORMA_SENDER_H

/* Tenta la consegna entro ORMA_SEND_TIMEOUT_MS. Non fallisce mai in modo
 * visibile: se il daemon e' assente, lento o morto, il frame si perde e si
 * incrementa il contatore dei drop. Non emette mai warning PHP. */
void orma_sender_send(const char *data, size_t len);

void orma_sender_close(void);

/* Contabilizza un frame perso. Il conteggio viaggia nel frame successivo. */
void orma_sender_drop(void);

#endif /* ORMA_SENDER_H */
