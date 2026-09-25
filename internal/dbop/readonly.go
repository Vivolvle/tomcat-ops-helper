package dbop

import (
	"errors"
	"strings"
)

// ErrForbidCmd is returned by SQLOp.Exec (and any Query whose statement
// is not a clearly read-only query) when the connector was locked with
// config.json "forbid_cmd": true. Callers surface the message verbatim
// so the operator / agent on the other side immediately understands why
// the statement was refused.
var ErrForbidCmd = errors.New("forbid_cmd: this connector is locked to read-only by config.json (forbid_cmd=true); only SELECT / SHOW / DESC / EXPLAIN queries are allowed — INSERT / UPDATE / DELETE / DDL / CALL and everything else is rejected")

// checkReadOnlyQuery validates that stmt is safe to run on a connector
// locked with forbid_cmd=true. It is deliberately stricter than
// RouteStatement: the router lets WITH-led statements go through Query
// because MySQL 8 / SQL Server accept `WITH cte AS (…) DELETE …` — a
// data-changing statement that starts with WITH. In read-only mode we
// only trust a narrow allowlist of unambiguous read verbs, and refuse
// EXPLAIN over DML (MySQL's EXPLAIN ANALYZE executes the statement it
// explains) plus SELECT … INTO OUTFILE/DUMPFILE (server-side file
// writes).
//
// Allowed shapes:
//
//	SELECT …            (incl. a leading paren: "(SELECT …) UNION …")
//	SHOW …
//	DESC … / DESCRIBE …
//	EXPLAIN SELECT …
//	EXPLAIN ANALYZE SELECT …
//
// Everything else — WITH-led statements included — is rejected.
func checkReadOnlyQuery(stmt string) error {
	toks := scanSQLTokens(stmt)
	if len(toks) == 0 {
		return ErrForbidCmd
	}
	// Parentheses and other punctuation never become tokens, so a
	// "(SELECT …) UNION (SELECT …)" compound simply starts with SELECT.
	head := toks[0]
	switch head {
	case "SELECT", "SHOW", "DESC", "DESCRIBE":
		// fall through to the INTO OUTFILE check below
	case "EXPLAIN":
		// Only explaining a plain SELECT is safe. EXPLAIN ANALYZE
		// executes the underlying statement, so ANALYZE is accepted
		// only when what follows is SELECT.
		if len(toks) < 2 {
			return ErrForbidCmd
		}
		if toks[1] == "ANALYZE" {
			if len(toks) < 3 || toks[2] != "SELECT" {
				return ErrForbidCmd
			}
		} else if toks[1] != "SELECT" {
			return ErrForbidCmd
		}
	default:
		return ErrForbidCmd
	}
	for i, tok := range toks {
		if tok == "INTO" && i+1 < len(toks) {
			switch toks[i+1] {
			case "OUTFILE", "DUMPFILE":
				return ErrForbidCmd
			}
		}
	}
	return nil
}

// scanSQLTokens returns the uppercased word tokens of s, skipping
// string literals ('…' with ” escapes), quoted identifiers ("…", `…`),
// -- line comments and /* */ block comments. Word tokens are maximal
// runs of [A-Z0-9_$#] — enough for keyword detection without any
// driver-specific parser. Numbers and punctuation produce no tokens,
// which is what we want: only keyword positions matter here.
func scanSQLTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	isWord := func(c byte) bool {
		return c == '_' || c == '$' || c == '#' ||
			(c >= '0' && c <= '9') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z')
	}
	i := 0
	for i < len(s) {
		c := s[i]
		next := byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case c == '-' && next == '-':
			// line comment
		flushComment:
			for i < len(s) {
				if s[i] == '\n' {
					break flushComment
				}
				i++
			}
		case c == '/' && next == '*':
			// block comment
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'':
			// string literal; '' is an escaped quote
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case c == '"' || c == '`':
			// quoted identifier (either dialect), with "" doubling
			// as an escape inside double quotes
			q := c
			i++
			for i < len(s) {
				if s[i] == q {
					if q == '"' && i+1 < len(s) && s[i+1] == q {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case isWord(c):
			for i < len(s) && isWord(s[i]) {
				if s[i] >= 'a' && s[i] <= 'z' {
					cur.WriteByte(s[i] - 32)
				} else {
					cur.WriteByte(s[i])
				}
				i++
			}
			flush()
		default:
			i++
		}
	}
	flush()
	return toks
}
