package oci

import (
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var dockerHubAliases = map[string]bool{"docker.io": true, "index.docker.io": true, "registry.hub.docker.com": true, dockerHubAuthority: true}

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// Both mirror the OCI distribution spec grammar verbatim. Narrowing either rejects legal
	// artifacts, and the changelog path reports a rejected reference as "no changelog".
	namePartPattern       = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagPattern            = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	authorityLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type reference struct{ authority, name, tag, digest string }

func parseReference(raw string) (reference, error) {
	if strings.ContainsAny(raw, "?#") {
		return reference{}, errors.New("invalid OCI reference")
	}
	authority, rest, hasAuthority := "", raw, false
	if head, tail, ok := strings.Cut(raw, "/"); ok && registryHost(head) {
		authority, rest, hasAuthority = head, tail, true
	}
	dockerHub := !hasAuthority || dockerHubAliases[authority]
	if dockerHub {
		authority = dockerHubAuthority
	}
	if rest == "" {
		return reference{}, errors.New("OCI reference needs registry and name")
	}
	r := reference{authority: authority}
	if !validAuthority(r.authority) {
		return reference{}, errors.New("invalid OCI registry")
	}
	if at := strings.LastIndex(rest, "@sha256:"); at >= 0 {
		r.name, r.digest = rest[:at], strings.ToLower(rest[at+1:])
		// A reference may carry both coordinates; the digest identifies the artifact, so the tag
		// is separated off rather than left to corrupt the repository name.
		if colon := strings.LastIndex(r.name, ":"); colon > strings.LastIndex(r.name, "/") {
			r.name, r.tag = r.name[:colon], r.name[colon+1:]
			if r.tag == "" {
				return reference{}, errors.New("invalid OCI reference")
			}
		}
	} else {
		at := strings.LastIndex(rest, ":")
		if at < 1 {
			return reference{}, errors.New("OCI reference requires tag or digest")
		}
		r.name, r.tag = rest[:at], rest[at+1:]
	}
	if dockerHub && !strings.Contains(r.name, "/") {
		r.name = dockerHubNamespace + "/" + r.name
	}
	// Each coordinate is validated on its own: a well-formed tag must never excuse a malformed
	// digest, which would otherwise reach the request path verbatim.
	if !validName(r.name) ||
		(r.digest != "" && !digestPattern.MatchString(r.digest)) ||
		(r.tag != "" && !tagPattern.MatchString(r.tag)) ||
		(r.digest == "" && r.tag == "") {
		return reference{}, errors.New("invalid OCI reference")
	}
	return r, nil
}

// registryHost applies the distinguisher every OCI client uses: a leading segment is a registry
// only when it carries a dot or a port, or is localhost. Everything else is a Docker Hub namespace.
func registryHost(segment string) bool {
	return strings.ContainsAny(segment, ".:") || segment == "localhost"
}

func canonicalAuthority(authority string) string {
	if dockerHubAliases[authority] {
		return dockerHubAuthority
	}
	return authority
}

func validAuthority(authority string) bool {
	if authority == "" || len(authority) > 253 || strings.ContainsAny(authority, "@/?#") {
		return false
	}
	host := authority
	if h, port, err := net.SplitHostPort(authority); err == nil {
		value, err := strconv.ParseUint(port, 10, 16)
		if h == "" || err != nil || value == 0 {
			return false
		}
		host = h
	} else if strings.Contains(authority, ":") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.String() == host
	}
	for _, label := range strings.Split(host, ".") {
		if !authorityLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if !namePartPattern.MatchString(part) {
			return false
		}
	}
	return true
}
func (r reference) ref() string {
	if r.digest != "" {
		return r.digest
	}
	return r.tag
}
func (r reference) normalized() string { return r.authority + "/" + r.name + "@" + r.digest }
