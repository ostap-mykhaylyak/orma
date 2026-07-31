/* Offuscamento delle query SQL.
 *
 * Regola: i valori non escono mai dal processo. Una query come
 *
 *   SELECT * FROM utenti WHERE email = 'mario@esempio.it' AND eta > 30
 *
 * diventa
 *
 *   SELECT * FROM utenti WHERE email = ? AND eta > ?
 *
 * prima ancora di essere serializzata. Il daemon non vede mai i letterali,
 * quindi non puo' scriverli su disco nemmeno per sbaglio.
 */

#ifdef HAVE_CONFIG_H
# include "config.h"
#endif

#include "php.h"
#include "php_orma.h"
#include "orma_sql.h"

#include <string.h>
#include <ctype.h>

static inline bool orma_is_ident(char c)
{
	return isalnum((unsigned char)c) || c == '_' || c == '$';
}

size_t orma_sql_obfuscate(const char *sql, size_t len, char *out, size_t out_cap)
{
	size_t w = 0;
	size_t i = 0;
	bool last_was_space = true; /* evita lo spazio iniziale */

	if (out_cap == 0) {
		return 0;
	}

	while (i < len && w + 1 < out_cap) {
		char c = sql[i];

		/* Commento di linea: -- ... fino a fine riga. */
		if (c == '-' && i + 1 < len && sql[i + 1] == '-') {
			while (i < len && sql[i] != '\n') {
				i++;
			}
			continue;
		}

		/* Commento a blocco. */
		if (c == '/' && i + 1 < len && sql[i + 1] == '*') {
			i += 2;
			while (i + 1 < len && !(sql[i] == '*' && sql[i + 1] == '/')) {
				i++;
			}
			i = (i + 1 < len) ? i + 2 : len;
			continue;
		}

		/* Letterale fra apici singoli o doppi, con raddoppio ed escape. */
		if (c == '\'' || c == '"') {
			char quote = c;
			i++;
			while (i < len) {
				if (sql[i] == '\\' && i + 1 < len) {
					i += 2;
					continue;
				}
				if (sql[i] == quote) {
					if (i + 1 < len && sql[i + 1] == quote) {
						i += 2;
						continue;
					}
					i++;
					break;
				}
				i++;
			}
			out[w++] = '?';
			last_was_space = false;
			continue;
		}

		/* Numero: solo se non e' parte di un identificatore come "col2". */
		if (isdigit((unsigned char)c) && (i == 0 || !orma_is_ident(sql[i - 1]))) {
			while (i < len && (isdigit((unsigned char)sql[i]) || sql[i] == '.'
			                   || sql[i] == 'e' || sql[i] == 'E'
			                   || ((sql[i] == '+' || sql[i] == '-') && i > 0
			                       && (sql[i - 1] == 'e' || sql[i - 1] == 'E')))) {
				i++;
			}
			out[w++] = '?';
			last_was_space = false;
			continue;
		}

		/* Spazi compressi. */
		if (isspace((unsigned char)c)) {
			if (!last_was_space) {
				out[w++] = ' ';
				last_was_space = true;
			}
			i++;
			continue;
		}

		out[w++] = c;
		last_was_space = false;
		i++;
	}

	/* Niente spazio in coda. */
	while (w > 0 && out[w - 1] == ' ') {
		w--;
	}

	out[w] = '\0';
	return w;
}

const char *orma_sql_operation(const char *sql, size_t len)
{
	static const struct {
		const char *word;
		size_t      len;
	} ops[] = {
		{ "SELECT", 6 }, { "INSERT", 6 }, { "UPDATE", 6 }, { "DELETE", 6 },
		{ "REPLACE", 7 }, { "CREATE", 6 }, { "ALTER", 5 }, { "DROP", 4 },
		{ "TRUNCATE", 8 }, { "SHOW", 4 }, { "SET", 3 }, { "BEGIN", 5 },
		{ "COMMIT", 6 }, { "ROLLBACK", 8 }, { "CALL", 4 }, { "EXPLAIN", 7 },
	};

	size_t i = 0;
	while (i < len && (isspace((unsigned char)sql[i]) || sql[i] == '(')) {
		i++;
	}

	for (size_t k = 0; k < sizeof(ops) / sizeof(ops[0]); k++) {
		size_t n = ops[k].len;
		if (len - i >= n && strncasecmp(sql + i, ops[k].word, n) == 0) {
			/* Deve essere una parola intera. */
			if (len - i == n || !orma_is_ident(sql[i + n])) {
				return ops[k].word;
			}
		}
	}
	return "SQL";
}
