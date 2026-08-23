package oci

import "fmt"

type Credential struct{ Username, Secret string }

// String redacts the secret so a credential cannot reach a log or error through %v.
func (c Credential) String() string {
	return "oci.Credential{username: " + c.Username + ", secret: redacted}"
}

// checkedCredentials refuses a credential the client could never safely offer, so a
// misconfiguration surfaces at construction rather than as an anonymous "not found".
func checkedCredentials(values map[string]Credential) (map[string]Credential, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]Credential, len(values))
	for authority, credential := range values {
		if !validAuthority(authority) {
			return nil, fmt.Errorf("invalid OCI credential registry %q", authority)
		}
		if !validCredentialUsername(credential.Username) || credential.Secret == "" {
			return nil, fmt.Errorf("invalid OCI credential for registry %q", authority)
		}
		canonical := canonicalAuthority(authority)
		if _, duplicate := out[canonical]; duplicate {
			return nil, fmt.Errorf("conflicting OCI credentials for registry %q", canonical)
		}
		out[canonical] = credential
	}
	return out, nil
}

func validCredentialUsername(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, r := range value {
		if r < '!' || r > '~' || r == ':' {
			return false
		}
	}
	return true
}
