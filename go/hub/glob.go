package hub

// bashMatch is bash's [[ s == pattern ]] with the pattern unquoted (a topic policy's patterns):
// '*' any string (including '/'), '?' any byte, '[...]' a bracket expression with '!' or '^'
// negation, ranges and [:class:]es, '\' quoting the next byte. Bytes, as under LC_ALL=C.
func bashMatch(pattern, s string) bool {
	px, sx := 0, 0
	nextPx, nextSx := -1, -1
	for px < len(pattern) || sx < len(s) {
		if px < len(pattern) {
			switch c := pattern[px]; c {
			case '*':
				nextPx, nextSx = px, sx+1
				px++
				continue
			case '?':
				if sx < len(s) {
					px++
					sx++
					continue
				}
			case '[':
				if sx < len(s) {
					if ok, n, valid := matchBracket(pattern[px:], s[sx]); valid {
						if ok {
							px += n
							sx++
							continue
						}
					} else if s[sx] == '[' { // an unclosed '[' is an ordinary character
						px++
						sx++
						continue
					}
				}
			case '\\':
				if px+1 < len(pattern) {
					if sx < len(s) && s[sx] == pattern[px+1] {
						px += 2
						sx++
						continue
					}
				} else if sx < len(s) && s[sx] == '\\' {
					px++
					sx++
					continue
				}
			default:
				if sx < len(s) && s[sx] == c {
					px++
					sx++
					continue
				}
			}
		}
		if nextSx > 0 && nextSx <= len(s) {
			px, sx = nextPx, nextSx
			continue
		}
		return false
	}
	return true
}

// matchBracket matches byte c against the bracket expression at the start of p. Returns whether it
// matched, the expression's length, and whether p holds a complete expression at all.
func matchBracket(p string, c byte) (bool, int, bool) {
	i := 1
	neg := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		neg = true
		i++
	}
	matched := false
	first := true
	for i < len(p) {
		if p[i] == ']' && !first {
			return matched != neg, i + 1, true
		}
		first = false
		if p[i] == '[' && i+1 < len(p) && p[i+1] == ':' {
			if end := indexFrom(p, ":]", i+2); end >= 0 {
				if classMatch(p[i+2:end], c) {
					matched = true
				}
				i = end + 2
				continue
			}
		}
		lo := p[i]
		if lo == '\\' && i+1 < len(p) {
			i++
			lo = p[i]
		}
		i++
		hi := lo
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			hi = p[i+1]
			if hi == '\\' && i+2 < len(p) {
				hi = p[i+2]
				i++
			}
			i += 2
		}
		if lo <= c && c <= hi {
			matched = true
		}
	}
	return false, 0, false
}

func indexFrom(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func classMatch(class string, c byte) bool {
	switch class {
	case "alpha":
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	case "digit":
		return c >= '0' && c <= '9'
	case "alnum":
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	case "lower":
		return c >= 'a' && c <= 'z'
	case "upper":
		return c >= 'A' && c <= 'Z'
	case "space":
		return c == ' ' || c >= '\t' && c <= '\r'
	case "blank":
		return c == ' ' || c == '\t'
	case "punct":
		return c > ' ' && c < 0x7f && !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
	case "xdigit":
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	case "cntrl":
		return c < 0x20 || c == 0x7f
	case "print":
		return c >= 0x20 && c < 0x7f
	case "graph":
		return c > 0x20 && c < 0x7f
	}
	return false
}
