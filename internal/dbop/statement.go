package dbop

import "strings"

// SplitScript breaks a SQL script into individual statements separated
// by ';' that appears outside any string literal or comment. The split
// is intentionally simple — it does NOT understand PL/SQL blocks, so
// `BEGIN ... END;` will be split mid-block. Callers needing that
// should send each PL/SQL block as its own tool call.
//
// String-literal rules honoured:
//
//   - '…' with '' as the SQL-standard escape for one quote
//   - "…"  (double-quoted identifier in standard SQL; some drivers use
//     it for string literals too)
//   - backtick `…` (MySQL identifier quoting)
//   - $tag$ … $tag$ (PostgreSQL dollar quoting; harmless for MySQL /
//     Oracle / SQL Server)
//
// Comment rules:
//
//   - `-- …` until end of line
//   - `/* … */` block comments (non-nested)
//
// Comments are preserved verbatim in the output so that the caller can
// feed them straight to the driver and the audit log can record what
// the operator actually wrote. Trailing-whitespace-only chunks are
// dropped so "SELECT 1; " yields ["SELECT 1"], not ["SELECT 1", ""].
func SplitScript(script string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, inBacktick, inLineComment, inBlockComment := false, false, false, false, false
	i := 0
	for i < len(script) {
		c := script[i]
		// Two-character lookahead.
		next := byte(0)
		if i+1 < len(script) {
			next = script[i+1]
		}
		switch {
		case inLineComment:
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			i++
			continue
		case inBlockComment:
			cur.WriteByte(c)
			if c == '*' && next == '/' {
				cur.WriteByte(next)
				inBlockComment = false
				i += 2
				continue
			}
			i++
			continue
		case inSingle:
			cur.WriteByte(c)
			if c == '\'' && next == '\'' {
				// SQL-standard escaped quote inside string.
				cur.WriteByte(next)
				i += 2
				continue
			}
			if c == '\'' {
				inSingle = false
			}
			i++
			continue
		case inDouble:
			cur.WriteByte(c)
			if c == '"' && next == '"' {
				cur.WriteByte(next)
				i += 2
				continue
			}
			if c == '"' {
				inDouble = false
			}
			i++
			continue
		case inBacktick:
			cur.WriteByte(c)
			if c == '`' {
				inBacktick = false
			}
			i++
			continue
		default:
			switch {
			case c == '-' && next == '-':
				inLineComment = true
				cur.WriteByte(c)
				cur.WriteByte(next)
				i += 2
				continue
			case c == '/' && next == '*':
				inBlockComment = true
				cur.WriteByte(c)
				cur.WriteByte(next)
				i += 2
				continue
			case c == '\'':
				inSingle = true
				cur.WriteByte(c)
				i++
				continue
			case c == '"':
				inDouble = true
				cur.WriteByte(c)
				i++
				continue
			case c == '`':
				inBacktick = true
				cur.WriteByte(c)
				i++
				continue
			case c == '$' && next == '$':
				// Dollar-quoted string (Postgres-style). We do not need
				// to support it for the three drivers we ship, but
				// being tolerant costs almost nothing: find the
				// matching $$ and copy verbatim.
				j := i + 2
				for j+1 < len(script) && !(script[j] == '$' && script[j+1] == '$') {
					j++
				}
				if j+1 >= len(script) {
					// Unterminated; treat the rest as a single string.
					cur.WriteString(script[i:])
					i = len(script)
					continue
				}
				cur.WriteString(script[i : j+2])
				i = j + 2
				continue
			case c == ';':
				out = append(out, cur.String())
				cur.Reset()
				i++
				continue
			}
			cur.WriteByte(c)
			i++
		}
	}
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		out = append(out, cur.String())
	}
	return out
}

// RouteStatement decides whether a single statement should be sent
// through Query or Exec based on its leading keyword. We deliberately
// keep the rule list narrow and treat anything ambiguous as Exec —
// running a non-query as Query is harmless (returns zero rows), but
// running a query as Exec returns 0 affected rows and silently hides
// the data, which is the worse failure mode.
//
// Leading comments are stripped first so `-- my select\nSELECT …` is
// recognised as a query.
func RouteStatement(stmt string) StatementKind {
	s := trimStatement(stmt)
	if s == "" {
		return KindExec
	}
	upper := strings.ToUpper(s)
	// Use first whitespace token, case-insensitively.
	head := upper
	for i := 0; i < len(head); i++ {
		if head[i] == ' ' || head[i] == '\t' || head[i] == '\n' || head[i] == '\r' {
			head = head[:i]
			break
		}
	}
	// WITH (CTE) that starts a SELECT goes to Query; bare WITH is rare
	// in our supported drivers but we still treat it as a query.
	switch head {
	case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "VALUES", "TABLE":
		return KindQuery
	}
	// Oracle / SQL Server occasionally allow `(` immediately after the
	// keyword (e.g. `SELECT ...`, `(SELECT ...)`). Skip past whitespace
	// and a leading paren too.
	stripped := s
	for len(stripped) > 0 && (stripped[0] == ' ' || stripped[0] == '\t' || stripped[0] == '\n' || stripped[0] == '\r') {
		stripped = stripped[1:]
	}
	if len(stripped) > 0 && stripped[0] == '(' {
		head2 := strings.ToUpper(stripped[1:])
		// Skip whitespace after the paren too.
		for len(head2) > 0 && (head2[0] == ' ' || head2[0] == '\t' || head2[0] == '\n' || head2[0] == '\r') {
			head2 = head2[1:]
		}
		// Take first token.
		for i := 0; i < len(head2); i++ {
			if head2[i] == ' ' || head2[i] == '\t' || head2[i] == '\n' || head2[i] == '\r' {
				head2 = head2[:i]
				break
			}
		}
		switch head2 {
		case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "VALUES", "TABLE":
			return KindQuery
		}
	}
	return KindExec
}
