package oci

import "strings"

// bearerParams parses one RFC 7235 challenge. A comma only separates parameters outside a quoted
// string: registries routinely quote a scope containing `pull,push`. A parameter repeated within
// the challenge is refused rather than resolved, so a second `realm=` cannot displace the first.
func bearerParams(challenge string) (map[string]string, bool) {
	rest, ok := bearerScheme(challenge)
	if !ok {
		return nil, false
	}
	params, _, ok := authParams(rest)
	if !ok {
		return nil, false
	}
	return params, params["realm"] != ""
}

// authParams parses one challenge's auth-params and returns whatever follows it in the list.
func authParams(s string) (map[string]string, string, bool) {
	params, rest := map[string]string{}, s
	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return params, "", true
		}
		key, after, ok := authToken(rest)
		if !ok {
			return nil, "", false
		}
		after = strings.TrimLeft(after, " \t")
		// A bare token is the scheme of the next challenge in the list; its parameters are not ours.
		if !strings.HasPrefix(after, "=") {
			return params, rest, true
		}
		value, after, ok := authValue(strings.TrimLeft(after[1:], " \t"))
		if !ok {
			return nil, "", false
		}
		key = strings.ToLower(key)
		if _, duplicate := params[key]; duplicate {
			return nil, "", false
		}
		params[key] = value
		rest = strings.TrimLeft(after, " \t")
		if rest != "" && !strings.HasPrefix(rest, ",") {
			return nil, "", false
		}
	}
}

func bearerScheme(challenge string) (string, bool) {
	const scheme = "bearer"
	if len(challenge) <= len(scheme) || !strings.EqualFold(challenge[:len(scheme)], scheme) {
		return "", false
	}
	if rest := challenge[len(scheme):]; rest[0] == ' ' || rest[0] == '\t' {
		return rest, true
	}
	return "", false
}

func authTokenChar(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
}

func authToken(s string) (string, string, bool) {
	end := 0
	for end < len(s) && authTokenChar(s[end]) {
		end++
	}
	if end == 0 {
		return "", s, false
	}
	return s[:end], s[end:], true
}

// An unquoted value runs to the next separator rather than to the end of an RFC 7230 token: a
// realm URL is not a token, and a registry that omits the quotes was parsed before this change.
func authValue(s string) (string, string, bool) {
	if !strings.HasPrefix(s, `"`) {
		end := strings.IndexAny(s, ", \t")
		if end < 0 {
			end = len(s)
		}
		if end == 0 {
			return "", s, false
		}
		return s[:end], s[end:], true
	}
	var value strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", s, false
			}
			i++
			value.WriteByte(s[i])
		case '"':
			return value.String(), s[i+1:], true
		default:
			value.WriteByte(s[i])
		}
	}
	return "", s, false
}

// bearerChallenge finds the Bearer challenge among every WWW-Authenticate value. RFC 7235 fixes no
// order, so a registry may list Basic first, in one header or in several, and the list is walked
// challenge by challenge rather than scanned for the word: a quoted parameter must not forge one.
func bearerChallenge(values []string) string {
	for _, value := range values {
		rest := value
		for {
			rest = strings.TrimLeft(rest, " \t,")
			scheme, after, ok := authToken(rest)
			if !ok {
				break
			}
			if strings.EqualFold(scheme, "bearer") {
				if _, ok := bearerScheme(rest); ok {
					return rest
				}
			}
			_, next, ok := authParams(after)
			if !ok || next == "" {
				break
			}
			rest = next
		}
	}
	return ""
}
