/* Instrumentazione delle funzioni utente tramite zend_observer. */

#ifndef ORMA_OBSERVER_H
#define ORMA_OBSERVER_H

/* Registra l'observer. Va chiamata in MINIT: la registrazione e' di processo e
 * il motore decide una volta per op_array se osservarlo. */
void orma_observer_register(void);

void orma_observer_reset(void);
void orma_observer_free(void);

/* Identificativo dello span a cui appendere i figli generati adesso: la
 * funzione utente tracciata piu' interna, oppure la radice della transazione.
 * Usata anche dagli hook di M2, cosi' una query finisce sotto la funzione che
 * l'ha eseguita invece che appesa alla radice. */
void orma_observer_current_parent(uint8_t out[ORMA_SPAN_ID_LEN]);

#endif /* ORMA_OBSERVER_H */
