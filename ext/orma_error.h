/* Cattura degli errori PHP e delle eccezioni. */

#ifndef ORMA_ERROR_H
#define ORMA_ERROR_H

/* Installa gli hook su zend_error_cb e zend_throw_exception_hook, salvando i
 * precedenti. Va chiamata in MINIT. */
void orma_error_install(void);

/* Ripristina gli hook precedenti. */
void orma_error_uninstall(void);

#endif /* ORMA_ERROR_H */
