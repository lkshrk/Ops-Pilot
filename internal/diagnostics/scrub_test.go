package diagnostics_test

import (
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

func TestCredentialShapedSpansAreScrubbedFromUntrustedText(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		secret string
	}{
		{
			name:   "service account JWT",
			text:   "mounted eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlLWJ5dGVzMDAw at /var/run",
			secret: "eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ",
		},
		{
			name:   "database URL password",
			text:   "connecting to postgres://appuser:hunter2correcthorse@db:5432/app",
			secret: "hunter2correcthorse",
		},
		{
			name:   "amqp URL password",
			text:   "amqps://svc:Tr0ub4dor-3xample@broker.prod:5671/vhost",
			secret: "Tr0ub4dor-3xample",
		},
		{
			name:   "redis URL with no username",
			text:   "dial redis://:hunter2correcthorse@redis-master.prod.svc:6379/0 failed",
			secret: "hunter2correcthorse",
		},
		{
			name:   "git error printing the token as an empty-username URL",
			text:   "fatal: unable to access 'https://:0123456789abcdef0123456789abcdef01234567@github.com/org/gitops.git/': 403",
			secret: "0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:   "password flag",
			text:   "exec: mysqldump --user app --password hunter2correcthorse --host db",
			secret: "hunter2correcthorse",
		},
		{
			name:   "short pass key",
			text:   "config loaded: db_pass=hunter2correcthorse",
			secret: "hunter2correcthorse",
		},
		{
			name:   "cookie header",
			text:   "upstream rejected request, Cookie: session=8f14e45fceea167a5a36dedd4bea2543; theme=dark",
			secret: "8f14e45fceea167a5a36dedd4bea2543",
		},
		{
			name:   "tls key in a secret payload",
			text:   `{"data":{"tls.crt":"MIIC","tls.key":"c29tZS1wcml2YXRlLWtleQ=="}}`,
			secret: "c29tZS1wcml2YXRlLWtleQ",
		},
		{
			name:   "base64 encoded private key block",
			text:   "decoded value LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFb3dJQkFBS0NBUUVB",
			secret: "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQ",
		},
		{
			name:   "environment assignment",
			text:   "DB_PASSWORD=s3cr3t-p4ssw0rd-value",
			secret: "s3cr3t-p4ssw0rd-value",
		},
		{
			name:   "json field",
			text:   `{"level":"debug","client_secret":"abcd1234efgh5678"}`,
			secret: "abcd1234efgh5678",
		},
		{
			name:   "yaml field",
			text:   "apiKey: 9f8e7d6c5b4a3210",
			secret: "9f8e7d6c5b4a3210",
		},
		{
			name:   "authorization header",
			text:   "GET /v1/charges Authorization: Basic dXNlcjpwYXNzd29yZDEyMzQ1Ng==",
			secret: "dXNlcjpwYXNzd29yZDEyMzQ1Ng",
		},
		{
			name:   "github token",
			text:   "push failed using ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
			secret: "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		},
		{
			name:   "aws access key id",
			text:   "using credentials AKIAIOSFODNN7EXAMPLE in eu-central-1",
			secret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:   "private key block",
			text:   "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
			secret: "MIIEowIBAAKCAQEA",
		},
		{
			name:   "a bracket inside the password",
			text:   "password: p[ssw0rd123",
			secret: "p[ssw0rd123",
		},
		{
			name:   "brackets in the middle of the value",
			text:   "password=abc[def]ghi",
			secret: "abc[def",
		},
		{
			name:   "a bracket inside an api key",
			text:   "api_key: k[y1234567890",
			secret: "k[y1234567890",
		},
		{
			name:   "a bracket inside a token",
			text:   "token: t[kenvalue1234",
			secret: "t[kenvalue1234",
		},
		{
			name:   "a value that opens with a bracket",
			text:   "password: [my-real-password]",
			secret: "my-real-password",
		},
		{
			name:   "aws secret access key under its usual key name",
			text:   "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		{
			name:   "aws secret access key in a spaced assignment",
			text:   "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scrubbed := diagnostics.ScrubSecrets(test.text)
			if strings.Contains(scrubbed, test.secret) {
				t.Fatalf("%q survived scrubbing: %s", test.secret, scrubbed)
			}
		})
	}
}

func TestScrubbingLeavesTheEvidenceADiagnosisNeeds(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a missing secret is named",
			text: `MountVolume.SetUp failed for volume "certs" : secret "app-tls" not found`,
		},
		{
			name: "a secret reference in a manifest error",
			text: `Error: couldn't find key tls.crt in Secret prod/app-tls (secretName: app-tls)`,
		},
		{
			name: "an image pull failure",
			text: `Failed to pull image "registry.example.com/app:4.2.0": unauthorized`,
		},
		{
			name: "a connection error",
			text: "dial tcp 10.0.4.9:5432: connect: connection refused",
		},
		{
			name: "an empty credential is still visible as empty",
			text: "DB_PASSWORD= is unset, refusing to start",
		},
		{
			name: "a changelog naming a setting",
			text: "The api_key setting was renamed; see the migration guide.",
		},
		{
			name: "a timestamped log line",
			text: "2026-08-02T03:14:07Z INFO starting app version 4.2.0",
		},
		{
			name: "a word merely ending in pass",
			text: "bypass=true, compass: 4200 degrees",
		},
		{
			name: "a flag that only starts like a password flag",
			text: "--password-file /etc/app/creds.yaml was not readable",
		},
		{
			// A Flux revision is a 40-hex string, which is why no bare-hex rule exists.
			name: "the deployed revision",
			text: "HelmRelease/prod/app revision=main@sha1:8f14e45fceea167a5a36dedd4bea25438f14e45f",
		},
		{
			name: "an image digest",
			text: "pulling app@sha256:9f8e7d6c5b4a32109f8e7d6c5b4a32109f8e7d6c5b4a32109f8e7d6c5b4a3210",
		},
		{
			name: "a rejected manifest naming the offending field",
			text: `error validating data: unknown field "tls.key" in io.k8s.api.core.v1.Secret`,
		},
		{
			name: "a path that happens to contain pass",
			text: "accumulating resources: open /tmp/x/pass.yaml: no such file",
		},
		{
			name: "a word merely ending in cookie",
			text: "chocolate-cookie: 3 restarts, container not ready, image pull failed",
		},
		{
			name: "a sentence about a cookie",
			text: "cookie: invalid session, user must re-authenticate",
		},
		{
			name: "a key file that cannot be opened",
			text: "open /etc/ssl/tls.key: permission denied",
		},
		{
			name: "a hyphenated word ending in pass",
			text: "first-pass: complete, moving to second stage",
		},
		{
			name: "a lowercase hex run is never an aws key",
			text: "revision=0123456789abcdef0123456789abcdef01234567 applied",
		},
		{
			name: "an image pulled by digest",
			text: "ghcr.io/org/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			// The password half cannot cross a "/", so a digest "@" is unreachable.
			name: "an oci chart reference",
			text: "oci://ghcr.io/org/charts/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "a terraform style module source",
			text: "source: git::https://github.com/org/mod.git?ref=v1.2.3",
		},
		{
			name: "a commit trailer carrying an address",
			text: "Signed-off-by: renovate[bot] <29139614+renovate[bot]@users.noreply.github.com>",
		},
		{
			name: "base64 configmap data that is not a credential",
			text: "configmap data: Q29uZmlnTWFwRGF0YVRoYXRJc0xvbmdCdXROb3RTZWNyZXRBdEFsbA==",
		},
		{
			name: "a mixed case digest",
			text: "chart digest sha512:AbCdEf0123456789AbCdEf0123456789AbCdEf0123456789AbCdEf0123456789",
		},
		{
			// The identity of the broken object is what FluxStatus exists to
			// deliver. No fixed-width rule may be added that can redact it.
			name: "a flux object reference",
			text: "monitoring/HelmRelease/kubeprometheusstack healthy=false reconciling=false reason=UpgradeFailed",
		},
		{
			name: "a flux object reference of exactly forty characters",
			text: "monitoring/HelmChart/kubeprometheusstack healthy=false reconciling=false reason=UpgradeFailed",
		},
		{
			name: "a collector object reference",
			text: "observability/HelmRelease/opentelemetrycollector healthy=false reconciling=false reason=InstallFailed",
		},
		{
			name: "an ingress object reference",
			text: "ingress/Kustomization/ingressnginxcontroller healthy=false reconciling=false reason=ReconciliationFailed",
		},
		{
			name: "an operator object reference",
			text: "security/HelmRelease/externalsecretsoperator healthy=false reconciling=false reason=UpgradeFailed",
		},
		{
			name: "a camel case registry path in a pull failure",
			text: `Failed to pull image "registry.example.com/teamAlpha/serviceBravo/componentCharlie:v1"`,
		},
		{
			name: "a camel case registry path on a cloud registry",
			text: "myacr.azurecr.io/PaymentPlatform/OrderService/apiGateway:1.2.3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("scrubbing changed diagnostic text\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

func TestOneSecretIsReplacedExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a jwt under a credential-named json key",
			text: `{"token":"eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiJhcHAifQ.c2lnbmF0dXJlLWJ5dGVzMDAw"}`,
			want: `{"token":"[REDACTED]"}`,
		},
		{
			name: "a github token in an authorization header",
			text: "Authorization: Bearer ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
			want: "Authorization: [REDACTED]",
		},
		{
			name: "a password in a url under a credential-named key",
			text: "DATABASE_PASSWORD=postgres://app:hunter2correcthorse@db:5432/app",
			want: "DATABASE_PASSWORD=[REDACTED]",
		},
		{
			// Whichever rule runs first, the other must not see what it wrote:
			// the key-name rule alone stops at the space and leaks the body.
			name: "a private key block under a credential-named key",
			text: "password: -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
			want: "password: [REDACTED]",
		},
		{
			name: "a bracket inside the value",
			text: "password: p[ssw0rd123",
			want: "password: [REDACTED]",
		},
		{
			name: "a closing bracket mid value leaves no tail behind",
			text: "password=abc[def]ghi",
			want: "password=[REDACTED]",
		},
		{
			name: "several brackets leave no tail behind",
			text: "password: p[ss]w0rd]end",
			want: "password: [REDACTED]",
		},
		{
			name: "a value wrapped in brackets",
			text: "password: [my-real-password]",
			want: "password: [REDACTED]",
		},
		{
			name: "a cookie header keeps its non-secret attributes",
			text: "Set-Cookie: sid=abcdef123456; HttpOnly",
			want: "Set-Cookie: [REDACTED]; HttpOnly",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a rule re-matched what another rule wrote\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A key name indented under a credential-named key labels the value beside it.
// Eating the label costs the diagnosis a name and leaves the value exposed,
// because the rule that would have matched the inner key has been consumed.
func TestANestedKeyNameSurvivesAndItsOwnValueIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "an aws credentials file",
			text: "credentials:\n  aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
			want: "credentials:\n  aws_access_key_id = [REDACTED]",
		},
		{
			name: "a shapeless password under an outer key",
			text: "credentials:\n  password = hunter2correcthorse",
			want: "credentials:\n  password = [REDACTED]",
		},
		{
			name: "a yaml mapping indented under a credential-named key",
			text: "token:\n  clientSecret: hunter2correcthorse",
			want: "token:\n  clientSecret: [REDACTED]",
		},
		{
			name: "an error whose reason begins the next line",
			text: "error: could not read credentials:\n  Kustomization/flux-system/apps not found",
			want: "error: could not read credentials:\n  Kustomization/flux-system/apps not found",
		},
		{
			name: "a labelled password under a short pass key",
			text: "db_pass:\n  password: hunter2correcthorse",
			want: "db_pass:\n  password: [REDACTED]",
		},
		{
			name: "an error whose reason begins the line after a short pass key",
			text: "checks: db_pass:\n  Kustomization/flux-system/apps not found",
			want: "checks: db_pass:\n  Kustomization/flux-system/apps not found",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a key name was eaten as a value\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Gating a 40-character run of base64 characters on nearby AWS context was
// modelled and declined: 23% of such messages lost the object reference
// instead. A reference containing a hyphen never forms a run that long, so it
// would survive that rule whatever the rule did, and pins nothing - each
// fixture here is checked to be within reach before it is checked to survive.
func TestAnObjectReferenceBesideAnAWSTermIsInReachOfTheDeclinedRuleAndSurvives(t *testing.T) {
	const shortestAWSKey = 40
	tests := []struct {
		name      string
		text      string
		reference string
	}{
		{
			name:      "a bucket source quoting the object it could not fetch",
			text:      "failed to fetch artifact from s3.eu-central-1.amazonaws.com for monitoring/HelmChart/kubeprometheusstack",
			reference: "monitoring/HelmChart/kubeprometheusstack",
		},
		{
			name:      "an aws setting beside the object being reconciled",
			text:      "aws_region=eu-central-1 reconciling monitoring/Kustomization/victoriametrics",
			reference: "monitoring/Kustomization/victoriametrics",
		},
		{
			name:      "a flux bucket that cannot reach s3",
			text:      "fluxsystem/Bucket/homeopsmanifestsbackup: failed to confirm if bucket exists: s3.amazonaws.com",
			reference: "fluxsystem/Bucket/homeopsmanifestsbackup",
		},
		{
			name:      "a velero backup naming its storage location",
			text:      "velero/Backup/dailyclustersnapshotschedule: BackupStorageLocation aws-default is unavailable",
			reference: "velero/Backup/dailyclustersnapshotschedule",
		},
		{
			name:      "an s3 backed log store naming its statefulset",
			text:      "observability/StatefulSet/lokidistributedwriter: s3://loki-chunks unreachable, aws_sdk_load_config unset",
			reference: "observability/StatefulSet/lokidistributedwriter",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(test.text, test.reference) {
				t.Fatalf("the fixture does not carry the reference it names: %s", test.text)
			}
			if run := longestBase64Run(test.reference); run < shortestAWSKey {
				t.Fatalf(
					"%q has a longest run of %d and is out of the declined rule's reach: it would survive a rule that does nothing",
					test.reference, run,
				)
			}
			if !mentionsAWS(test.text) {
				t.Fatalf("the fixture carries no AWS term for the proximity rule to key on: %s", test.text)
			}
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("an object reference was lost\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// longestBase64Run is the longest span an AWS secret key could be mistaken for,
// which is the only span a bare-token rule could key on.
func longestBase64Run(text string) int {
	longest, current := 0, 0
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
			current++
			longest = max(longest, current)
		default:
			current = 0
		}
	}
	return longest
}

func mentionsAWS(text string) bool {
	lower := strings.ToLower(text)
	for _, term := range []string{"aws", "amazonaws", "s3"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

// A secret name reached through a path is a reference to a secret, not an
// assignment of one, and the token after the colon is prose. This is the same
// reading the tls.key rule already applies to "open /etc/ssl/tls.key: denied".
func TestAPathQualifiedSecretNameIsAReferenceNotAnAssignment(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a quoted secret reference in a controller message",
			text: `failed to get secret 'media/registry-credentials': secrets "registry-credentials" not found`,
		},
		{
			name: "an object reference whose name ends in a secret word",
			text: "Secret/media/api-key: dry-run failed: Unauthorized",
		},
		{
			name: "an image whose repository ends in a secret word",
			text: "ghcr.io/home-operations/secret-key:v4.5.6",
		},
		{
			name: "an image whose repository ends in token",
			text: "ghcr.io/onedr0p/plex/api-token:1.2.3",
		},
		{
			name: "a mounted path naming the file it could not read",
			text: "open /var/run/secrets/kubernetes.io/serviceaccount/token: permission denied",
		},
		{
			name: "a kustomization path naming a credential file",
			text: "accumulating resources: apps/media/credentials: no such file or directory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a secret reference lost its reason\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// The path exemption is a reading of ":" only. An "=" after a path-qualified
// credential name is an assignment, because the shapes the exemption protects -
// image refs, mount paths, kustomization paths - all separate with ":".
func TestAPathQualifiedCredentialAssignedWithAnEqualsSignIsScrubbed(t *testing.T) {
	const secret = "hunter2correcthorse"
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a vault read with spaces around the equals",
			text: "vault read secret/data/app/api_key = " + secret,
			want: "vault read secret/data/app/api_key = [REDACTED]",
		},
		{
			name: "an env var named by an absolute path",
			text: "env var /app/password=" + secret,
			want: "env var /app/password=[REDACTED]",
		},
		{
			name: "a helm set flag with a path-qualified key",
			text: "--set global/client_secret=" + secret,
			want: "--set global/client_secret=[REDACTED]",
		},
		{
			name: "a path-qualified access token in a query string",
			text: "GET https://vault/v1/app/access_token=" + secret,
			want: "GET https://vault/v1/app/access_token=[REDACTED]",
		},
		{
			name: "a quoted path-qualified assignment",
			text: `secret/data/db/password="` + secret + `"`,
			want: `secret/data/db/password="[REDACTED]"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a path-qualified assignment leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The "=" reading must not leak back into ":": a colon after a path-qualified
// name still means a reference, and an "=" that is really a comparison is not
// an assignment either.
func TestAnEqualsReadingDoesNotReachAColonReferenceOrAComparison(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "an image tag is still not an assignment",
			text: "ghcr.io/home-operations/secret-key:v4.5.6",
		},
		{
			name: "a mount path naming the file it could not read",
			text: "open /var/run/secrets/kubernetes.io/serviceaccount/token: permission denied",
		},
		{
			name: "a kustomization path naming a credential file",
			text: "accumulating resources: apps/media/credentials: no such file or directory",
		},
		{
			name: "an inequality against a path-qualified name",
			text: "app/api_key != expected-value-here",
		},
		{
			name: "an equality comparison against a path-qualified name",
			text: "app/api_key == expected",
		},
		{
			name: "a no-space equality comparison against a path-qualified name",
			text: "app/api_key ==expected",
		},
		{
			name: "a no-space inequality against a path-qualified name",
			text: "app/api_key !=expected-value",
		},
		{
			name: "a no-space at-least comparison against a path-qualified name",
			text: "app/token_count >=expected-value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a path-qualified reference lost its reason\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// The path exemption must not become a way to write a credential past the rule.
func TestAKeyNameAfterEveryOtherBoundaryStillRedacts(t *testing.T) {
	const secret = "hunter2correcthorse"
	tests := []struct {
		name string
		text string
	}{
		{name: "at the start of the text", text: "api_key=" + secret},
		{name: "after a space", text: "config loaded: api_key=" + secret},
		{name: "after a quote", text: `{"api_key":"` + secret + `"}`},
		{name: "after a brace", text: "{api_key=" + secret + "}"},
		{name: "after a query separator", text: "https://host/v1/sync?api_key=" + secret},
		{name: "after a query conjunction", text: "https://host/v1/sync?page=2&api_key=" + secret},
		{name: "after a comma", text: "flags: dry-run,api_key=" + secret},
		{name: "after an equals sign", text: "ARGS=--api_key=" + secret},
		{name: "after a newline", text: "settings\napi_key=" + secret},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scrubbed := diagnostics.ScrubSecrets(test.text)
			if strings.Contains(scrubbed, secret) {
				t.Fatalf("%q survived scrubbing: %s", secret, scrubbed)
			}
		})
	}
}

// A ${...} or $(...) placeholder opens its own brace or paren, so redacting the
// value must carry the matching close with it rather than leaving a dangling
// "[REDACTED]}" behind. A closing delimiter the value did not open is left in
// place, so a JSON "key":"value"} closes cleanly.
func TestAPlaceholderIsRedactedWithoutLeavingADanglingDelimiter(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a brace placeholder in double quotes",
			text: `token: "${VAULT_TOKEN}"`,
			want: `token: "[REDACTED]"`,
		},
		{
			name: "a paren command substitution",
			text: "clientSecret: $(CLIENT_SECRET)",
			want: "clientSecret: [REDACTED]",
		},
		{
			name: "a brace placeholder under a short pass key",
			text: "password: ${DB_PASSWORD}",
			want: "password: [REDACTED]",
		},
		{
			name: "a brace placeholder in single quotes",
			text: "api_key='${API_KEY}'",
			want: "api_key='[REDACTED]'",
		},
		{
			name: "a json value does not swallow the object's closing brace",
			text: `{"client_secret":"abcd1234efgh5678"}`,
			want: `{"client_secret":"[REDACTED]"}`,
		},
		{
			name: "a stray close the value did not open is left in place",
			text: "(password: hunter2correcthorse)",
			want: "(password: [REDACTED])",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a placeholder was mangled\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The private-key rule spans newlines, so an unbalanced BEGIN/END pair - a BEGIN
// whose END is a separate block, reached across a hunk header or a file header -
// would otherwise collapse every diff line between them, hiding the approved
// change from the operator. The span must stop at those structural boundaries. A
// blank line is deliberately not a boundary: an encrypted PEM contains one.
func TestThePrivateKeyRuleDoesNotCollapseUnrelatedDiffContentAcrossABoundary(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a hunk header between an unbalanced begin and a later end",
			text: "+tls.key: -----BEGIN RSA PRIVATE KEY-----\n" +
				"@@ -20,3 +20,4 @@\n" +
				" replicas: 3\n" +
				"+image: registry.example.com/app:4.2.0\n" +
				"+-----END RSA PRIVATE KEY-----\n",
		},
		{
			name: "a file header between an unbalanced begin and a later end",
			text: "+key: -----BEGIN RSA PRIVATE KEY-----\n" +
				"--- a/apps/other/deployment.yaml\n" +
				"+++ b/apps/other/deployment.yaml\n" +
				"+-----END RSA PRIVATE KEY-----\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scrubbed := diagnostics.ScrubSecrets(test.text)
			for _, want := range []string{"replicas", "image", "deployment.yaml"} {
				if strings.Contains(test.text, want) && !strings.Contains(scrubbed, want) {
					t.Fatalf("approved diff content %q was collapsed by the private-key rule\n got: %s", want, scrubbed)
				}
			}
		})
	}
}

// An RFC 1421 encrypted private key carries a Proc-Type/DEK-Info header block
// separated from its base64 body by a mandatory blank line. That blank line must
// not abort the span, or the whole key leaks - a leak is worse than the diff
// content an over-eager bound would hide.
func TestAnEncryptedPrivateKeyIsRedactedWholeAcrossItsMandatoryBlankLine(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\n" +
		"Proc-Type: 4,ENCRYPTED\n" +
		"DEK-Info: DES-EDE3-CBC,84E01D31C0A59D1F\n" +
		"\n" +
		"9mNspeahSECRETBODYbase64line0000\n" +
		"-----END RSA PRIVATE KEY-----"
	want := "[REDACTED]"
	if scrubbed := diagnostics.ScrubSecrets(in); scrubbed != want {
		t.Fatalf("an encrypted private key survived scrubbing\n want: %s\n  got: %s", want, scrubbed)
	}
}

// With no @@/---/+++ structural boundary between an unbalanced BEGIN and a stray
// END in the same hunk, the span collapses rather than risk leaking a key. The
// swallowed diff lines are the accepted price: a malformed key is pathological,
// and a hidden change is recoverable from the raw diff while a leaked key is not.
func TestAMalformedKeyWithNoStructuralBoundaryCollapsesRatherThanLeak(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\n" +
		" replicas: 3\n" +
		" image: nginx:1.5.0\n" +
		" resources: {}\n" +
		"-----END RSA PRIVATE KEY-----\n"
	want := "[REDACTED]\n"
	if scrubbed := diagnostics.ScrubSecrets(in); scrubbed != want {
		t.Fatalf("a malformed key span was not collapsed\n want: %s\n  got: %s", want, scrubbed)
	}
}

// A real added private key sits as an adjacent BEGIN/END pair inside one hunk,
// and must still be redacted whole.
func TestAnAdjacentPrivateKeyPairInADiffIsStillRedactedWhole(t *testing.T) {
	text := "+tls.key: -----BEGIN RSA PRIVATE KEY-----\n" +
		"+MIIEowIBAAKCAQEAsecretbodyline\n" +
		"+-----END RSA PRIVATE KEY-----\n"
	scrubbed := diagnostics.ScrubSecrets(text)
	if strings.Contains(scrubbed, "MIIEowIBAAKCAQEAsecretbodyline") {
		t.Fatalf("an adjacent private key survived scrubbing: %s", scrubbed)
	}
}

// A unified diff prefixes added and removed lines with "+"/"-" before their
// indentation, which sits between the key's newline and the value and disarms
// the next-line rules. PR-comment and terminal sinks carry diffs, so the value
// must still be reached under a single diff prefix.
func TestANextLineCredentialUnderADiffPrefixIsStillScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "an added next-line password",
			text: "+  password:\n+    hunter2correcthorse\n",
			want: "+  password:\n+    [REDACTED]\n",
		},
		{
			name: "a removed next-line api key",
			text: "-  apiKey:\n-    Ai8fkq2LmZx0Rt7Yb3Nc\n",
			want: "-  apiKey:\n-    [REDACTED]\n",
		},
		{
			name: "an added short pass key value on the next line",
			text: "+db_pass:\n+  hunter2correcthorse\n",
			want: "+db_pass:\n+  [REDACTED]\n",
		},
		{
			name: "an added block-scalar credential in a diff",
			text: "+  password: |\n+    hunter2correcthorse\n",
			want: "+  password: |\n+    [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a diff-prefixed next-line value escaped scrubbing\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A next-line value need not end its line: a quote closes it, a comma closes it
// in JSON, and a YAML comment follows it. None of those three can be prose, so
// each is a terminator the rule may accept without reaching into a sentence.
func TestANextLineCredentialClosedByAQuoteCommaOrCommentIsStillScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a quoted json value with a trailing comma",
			text: "\"password\":\n  \"hunter2correcthorse\",",
			want: "\"password\":\n  \"[REDACTED]\",",
		},
		{
			name: "a quoted json value with no trailing comma",
			text: "\"api_key\":\n  \"Ai8fkq2LmZx0Rt7Yb3Nc\"\n",
			want: "\"api_key\":\n  \"[REDACTED]\"\n",
		},
		{
			name: "a single-quoted next-line value",
			text: "client_secret:\n  'hunter2correcthorse'\n",
			want: "client_secret:\n  '[REDACTED]'\n",
		},
		{
			name: "a plain scalar followed by a yaml comment",
			text: "password:\n  hunter2correcthorse # from vault",
			want: "password:\n  [REDACTED] # from vault",
		},
		{
			name: "a short pass key whose next-line value carries a comment",
			text: "db_pass:\n  hunter2correcthorse # rotated 2026-01-01\n",
			want: "db_pass:\n  [REDACTED] # rotated 2026-01-01\n",
		},
		{
			name: "a diff-prefixed quoted next-line value",
			text: "+  \"token\":\n+    \"Ai8fkq2LmZx0Rt7Yb3Nc\",\n",
			want: "+  \"token\":\n+    \"[REDACTED]\",\n",
		},
		{
			name: "a bare value with a trailing comma ending the line",
			text: "access_key:\n  Ai8fkq2LmZx0Rt7Yb3Nc,\n",
			want: "access_key:\n  [REDACTED],\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a next-line value closed by a delimiter escaped scrubbing\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The terminators above stop at a delimiter on purpose. A next-line token
// followed by ordinary prose is byte-shape-identical to the broken object's own
// identity, so widening to reach it would eat the diagnosis instead.
func TestANextLineTokenFollowedByProseIsLeftAloneSoTheDiagnosisSurvives(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "an object reference under a credentials key",
			text: "error: could not read credentials:\n  Kustomization/flux-system/apps not found",
		},
		{
			name: "an object reference under a short pass key",
			text: "checks: db_pass:\n  Kustomization/flux-system/apps not found",
		},
		{
			name: "a wrapped parse error under a token key",
			text: "token:\n  expected 3 fields but got 2",
		},
		{
			name: "a wrapped connection error under a secret key",
			text: "secret:\n  unable to connect to vault",
		},
		{
			name: "a nested mapping key is not a value",
			text: "credentials:\n  name: vault",
		},
		{
			name: "an external secret reference followed by prose",
			text: "api_key:\n  cluster-secret-store unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("the next-line rule ate a diagnosis\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// A block scalar carries the value on the indented lines after a "|" or ">"
// header, so the same-line rule sees only the indicator and the next-line rule
// never fires. The redaction must cover the indented body and stop at the first
// line indented no deeper than the key, or it eats the following non-secret key.
func TestABlockScalarCredentialIsScrubbedAndAFollowingKeySurvives(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a literal block password",
			text: "  password: |\n    hunter2correcthorse\n",
			want: "  password: |\n    [REDACTED]\n",
		},
		{
			name: "a folded block secret",
			text: "clientSecret: >\n  hunter2correcthorse\n",
			want: "clientSecret: >\n  [REDACTED]\n",
		},
		{
			name: "a chomping indicator on the header",
			text: "apiKey: |-\n  Ai8fkq2LmZx0Rt7Yb3Nc\n",
			want: "apiKey: |-\n  [REDACTED]\n",
		},
		{
			name: "a multi-line literal block",
			text: "stringData:\n  password: |\n    firstlineofsecret\n    secondlineofsecret\n  username: appuser\n",
			want: "stringData:\n  password: |\n    [REDACTED]\n    [REDACTED]\n  username: appuser\n",
		},
		{
			name: "a short pass key as a block scalar",
			text: "db_pass: |\n  hunter2correcthorse\n",
			want: "db_pass: |\n  [REDACTED]\n",
		},
		{
			name: "a following sibling key at the same indent survives",
			text: "token: |\n    hunter2correcthorse\nimage: registry.example.com/app:4.2.0\n",
			want: "token: |\n    [REDACTED]\nimage: registry.example.com/app:4.2.0\n",
		},
		{
			name: "windows line endings in a block scalar",
			text: "password: |\r\n  hunter2correcthorse\r\n",
			want: "password: |\n  [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a block scalar credential was mishandled\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A block-scalar header is recognised anywhere on its line, so prose before the
// key, a quoted key, or a trailing comment on the header no longer lets the body
// leak on a world-readable sink.
func TestABlockScalarHeaderIsFoundAfterProseAQuoteOrACommentSoTheBodyNeverLeaks(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "prose before a literal block password",
			text: "reconcile failed: password: |\n  hunter2correcthorse\n",
			want: "reconcile failed: password: |\n  [REDACTED]\n",
		},
		{
			name: "prose before a folded block secret",
			text: "reconcile failed: password: >\n  hunter2correcthorse\n",
			want: "reconcile failed: password: >\n  [REDACTED]\n",
		},
		{
			name: "a quoted block-scalar key",
			text: "\"password\": |\n  hunter2correcthorse\n",
			want: "\"password\": |\n  [REDACTED]\n",
		},
		{
			name: "a header with a trailing comment",
			text: "password: | # inline\n  hunter2correcthorse\n",
			want: "password: | # inline\n  [REDACTED]\n",
		},
		{
			name: "prose before a short pass block scalar",
			text: "error: db_pass: |\n  hunter2correcthorse\n",
			want: "error: db_pass: |\n  [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a block-scalar body leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A word merely ending in a "|" or containing ">" is not a block-scalar header,
// and a credential-named key must not turn a following diagnostic line into a
// redaction just because it is indented.
func TestABlockScalarRuleDoesNotEatOrdinaryDiagnosticText(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a pipe in the middle of a message",
			text: "password validation failed | retry limit reached",
		},
		{
			name: "a pipe followed by same-line text is not a block header",
			text: "credentials: | source unreachable\n  Kustomization/flux-system/apps not found\n",
		},
		{
			name: "a greater-than in a comparison",
			text: "token count 5 > 3, throttling",
		},
		{
			name: "a non-credential block scalar after prose is not a credential header",
			text: "reconcile note: config: |\n  some ordinary data\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a block-scalar rule changed diagnostic text\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// The comment terminator lets a next-line secret be closed by a trailing "#"
// note, but an object reference (Namespace/Kind/name, a path with "/") followed
// by one is an identity, not a secret, so the terminator must not eat it.
func TestANextLineObjectReferenceWithATrailingCommentKeepsItsIdentity(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a kustomization reference annotated with a runbook link",
			text: "credentials:\n  Kustomization/flux-system/apps # see runbook",
		},
		{
			name: "a namespaced object reference under a short pass key",
			text: "db_pass:\n  monitoring/HelmRelease/kubeprometheusstack # broken",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("an object reference lost its identity to the comment terminator\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// A YAML plain scalar may begin on the line after its key, so narrowing the
// separator to spaces and tabs must not stop the value being reached.
func TestAValueOnTheLineAfterItsKeyIsStillScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a password as a multi-line plain scalar",
			text: "password:\n  hunter2correcthorse",
			want: "password:\n  [REDACTED]",
		},
		{
			name: "an api key as a multi-line plain scalar",
			text: "apiKey:\n  Ai8fkq2LmZx0Rt7Yb3Nc",
			want: "apiKey:\n  [REDACTED]",
		},
		{
			name: "windows line endings",
			text: "password:\r\n  hunter2correcthorse\r\n",
			want: "password:\n  [REDACTED]\n",
		},
		{
			name: "a short pass key with the value on the next line",
			text: "db_pass:\n  hunter2correcthorse",
			want: "db_pass:\n  [REDACTED]",
		},
		{
			name: "a nested key with its own value one line further down",
			text: "credentials:\n  api_key:\n    hunter2correcthorse",
			want: "credentials:\n  api_key:\n    [REDACTED]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a next-line value escaped scrubbing\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The diff-prefix tolerance that lets "+"/"-" open a value line also eats a
// column-0 YAML block-sequence dash, so a credential key whose value is a plain
// list — an ExternalSecret/SealedSecret reference shape, never a raw scalar — is
// redacted where it should be left alone. A key at true column 0 is not inside a
// diff hunk, so a "- " below it is a YAML dash and not a removed line.
func TestAnUnindentedBlockSequenceUnderACredentialKeyIsNotOverRedacted(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "an api key mapping to a single-item list",
			text: "apikey:\n- sometokenvaluehere\n",
		},
		{
			name: "a password key mapping to a list reference",
			text: "password:\n- vault-backend-reference\n",
		},
		{
			name: "a client secret mapping to a list",
			text: "client_secret:\n- externalsecretref\n",
		},
		{
			name: "a block sequence under a key that follows earlier content",
			text: "spec:\napikey:\n- externalsecretref\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a block-sequence reference was over-redacted\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// An INDENTED block-sequence item under a credential key is a real nested value,
// not the column-0 reference structure of C-L61/C-L85, so its scalar is redacted
// while the dash and indentation survive.
func TestAnIndentedBlockSequenceValueUnderACredentialKeyIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "an api key mapping to an indented single-item list",
			text: "apikey:\n  - sometokenvaluehere\n",
			want: "apikey:\n  - [REDACTED]\n",
		},
		{
			name: "a password mapping to an indented list",
			text: "password:\n  - hunter2correcthorse\n",
			want: "password:\n  - [REDACTED]\n",
		},
		{
			name: "a short pass key mapping to an indented list",
			text: "db_pass:\n  - hunter2correcthorse\n",
			want: "db_pass:\n  - [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("an indented block-sequence value leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The indented block-sequence redaction must not reach the exemption family: an
// object reference (a "/"-bearing item) or a column-0 dash under a credential key
// keeps its identity.
func TestAnIndentedBlockSequenceObjectReferenceKeepsItsIdentity(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "an indented object reference under a credential key",
			text: "credentials:\n  - Kustomization/flux-system/apps\n",
		},
		{
			name: "an indented external secret reference under an api key",
			text: "apikey:\n  - cluster-secret-store/backend\n",
		},
		{
			name: "a column-0 dash is still a reference structure",
			text: "apikey:\n- sometokenvaluehere\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a block-sequence reference was over-redacted\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}
}

// A credential key may map to a list of several secrets, so the redaction runs
// the whole sequence: stopping at item 1 published items 2+ to PR comments, the
// JSONL stream and the model.
func TestEveryItemOfAnIndentedBlockSequenceUnderACredentialKeyIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "three items under an api key",
			text: "apikey:\n  - sometokenvaluehere\n  - anothertokenvalue\n  - thirdtokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "two items under a short pass key",
			text: "db_pass:\n  - hunter2correcthorse\n  - staplebatteryhorse\n",
			want: "db_pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "an indented key whose list is indented further",
			text: "spec:\n  apikey:\n    - sometokenvaluehere\n    - anothertokenvalue\n",
			want: "spec:\n  apikey:\n    - [REDACTED]\n    - [REDACTED]\n",
		},
		{
			name: "a sequence at its own key's indent",
			text: "spec:\n  apikey:\n  - sometokenvaluehere\n  - anothertokenvalue\n",
			want: "spec:\n  apikey:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "a quoted and a comment-closed item after the first",
			text: "apikey:\n  - sometokenvaluehere\n  - \"anothertokenvalue\"\n  - thirdtokenvalue # rotated\n",
			want: "apikey:\n  - [REDACTED]\n  - \"[REDACTED]\"\n  - [REDACTED] # rotated\n",
		},
		{
			name: "an object reference between two secrets keeps its identity",
			text: "apikey:\n  - sometokenvaluehere\n  - cluster-secret-store/backend\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n  - cluster-secret-store/backend\n  - [REDACTED]\n",
		},
		{
			name: "a blank line does not end the sequence",
			text: "apikey:\n  - sometokenvaluehere\n\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n\n  - [REDACTED]\n",
		},
		{
			name: "a sequence with no trailing newline",
			text: "apikey:\n  - sometokenvaluehere\n  - anothertokenvalue",
			want: "apikey:\n  - [REDACTED]\n  - [REDACTED]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a block-sequence item leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Walking the whole sequence must stop where the sequence does: a key at or
// below the credential key's indent closes it, and the list under an ordinary
// key beside it is diagnostic evidence that must survive.
func TestWalkingACredentialSequenceStopsAtItsOwnBoundary(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a following key at the same indent ends the sequence",
			text: "apikey:\n  - sometokenvaluehere\nimage:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\nimage:\n  - nginxlatestimage\n",
		},
		{
			name: "a dedented key ends the sequence",
			text: "spec:\n  apikey:\n    - sometokenvaluehere\n  images:\n    - nginxlatestimage\n",
			want: "spec:\n  apikey:\n    - [REDACTED]\n  images:\n    - nginxlatestimage\n",
		},
		{
			name: "a column-0 sequence stays the reference structure it is",
			text: "apikey:\n- sometokenvaluehere\n- anothertokenvalue\n",
			want: "apikey:\n- sometokenvaluehere\n- anothertokenvalue\n",
		},
		{
			name: "a nested sequence at a deeper indent is not part of the list",
			text: "apikey:\n  - name: primary\n    aliases:\n      - nginxlatestimage\n",
			want: "apikey:\n  - name: primary\n    aliases:\n      - nginxlatestimage\n",
		},
		{
			name: "an ordinary key's sequence is untouched",
			text: "images:\n  - nginxlatestimage\n  - redislatestimage\n",
			want: "images:\n  - nginxlatestimage\n  - redislatestimage\n",
		},
		{
			name: "a scalar line after the sequence is not an item",
			text: "apikey:\n  - sometokenvaluehere\n  owner: platformteam\n",
			want: "apikey:\n  - [REDACTED]\n  owner: platformteam\n",
		},
		{
			name: "a dash at a deeper indent is not a sibling item",
			text: "apikey:\n  - sometokenvaluehere\n      - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n      - nginxlatestimage\n",
		},
		{
			name: "a mapping key inside the sequence closes it",
			text: "apikey:\n  - sometokenvaluehere\n  extra:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n  extra:\n  - nginxlatestimage\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("the sequence walk crossed its boundary\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The column-0 exemption must not disarm the diff and prose forms: a value line
// carrying a real diff marker under a diff-prefixed or prose key still hides a
// credential, and dropping it would trade the over-redaction fix for a leak.
func TestACredentialWhoseKeyLineIsADiffHunkOrProseStillScrubsItsNextLineValue(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a diff-added list item whose key is a credential",
			text: "+  - password:\n+      hunter2correcthorse\n",
			want: "+  - password:\n+      [REDACTED]\n",
		},
		{
			name: "a diff-added value under a prose key",
			text: "error: password:\n+ hunter2correcthorse\n",
			want: "error: password:\n+ [REDACTED]\n",
		},
		{
			name: "a diff-removed value under a prose key",
			text: "failed to load password:\n- hunter2correcthorse\n",
			want: "failed to load password:\n- [REDACTED]\n",
		},
		{
			name: "an indented key whose removed value opens with a dash",
			text: " apikey:\n-  hunter2correcthorse\n",
			want: " apikey:\n-  [REDACTED]\n",
		},
		{
			name: "a diff-removed zero-indent key and its removed scalar value",
			text: "-apikey:\n-  hunter2correcthorse\n",
			want: "-apikey:\n-  [REDACTED]\n",
		},
		{
			name: "a diff-added zero-indent key and its added scalar value",
			text: "+apikey:\n+  hunter2correcthorse\n",
			want: "+apikey:\n+  [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a diff or prose next-line credential escaped scrubbing\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// C-L61's accepted trade, pinned: a column-0 block-sequence item under a
// credential key is a reference structure — no real schema (K8s Secret,
// SealedSecret, ExternalSecret, SOPS) stores an opaque secret this way — so an
// opaque value there is deliberately left alone rather than reopening C-L61's
// over-redaction. A value that matches a value-shape rule is still redacted,
// because those rules key on the shape, not on the surrounding structure.
func TestAColumn0ListItemUnderACredentialKeyIsAReferenceButShapedSecretsStillRedact(t *testing.T) {
	left := []struct {
		name string
		text string
	}{
		{name: "an opaque token", text: "apikey:\n- sometokenvaluehere\n"},
		{name: "an opaque password", text: "password:\n- correcthorsebatterystaple\n"},
		{name: "an opaque client secret", text: "client_secret:\n- someopaquereference\n"},
	}
	for _, test := range left {
		t.Run("left alone/"+test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a column-0 reference was over-redacted\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}

	redacted := []struct {
		name   string
		text   string
		secret string
	}{
		{
			name:   "a github token by shape",
			text:   "apikey:\n- ghp_0123456789abcdefghijklmnopqrstuvwx\n",
			secret: "ghp_0123456789abcdefghijklmnopqrstuvwx",
		},
		{
			name:   "a jwt by shape",
			text:   "apikey:\n- eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U\n",
			secret: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name:   "an aws access key by shape",
			text:   "apikey:\n- AKIAIOSFODNN7EXAMPLE\n",
			secret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:   "a base64 private key block by shape",
			text:   "apikey:\n- LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFb3dJQkFBS0NBUUVB\n",
			secret: "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQ",
		},
	}
	for _, test := range redacted {
		t.Run("still redacted/"+test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); strings.Contains(scrubbed, test.secret) {
				t.Fatalf("a shape-matching secret survived under a column-0 list: %s", scrubbed)
			}
		})
	}
}

// A YAML comment never terminates a block sequence, so the items after one are
// still the credential key's values.
func TestACommentInsideACredentialSequenceDoesNotEndIt(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a comment between two items",
			text: "apikey:\n  - sometokenvaluehere\n  # rotated quarterly\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # rotated quarterly\n  - [REDACTED]\n",
		},
		{
			name: "a column-0 comment between two items",
			text: "apikey:\n  - sometokenvaluehere\n# rotated quarterly\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n# rotated quarterly\n  - [REDACTED]\n",
		},
		{
			name: "a comment opening the sequence",
			text: "apikey:\n  # two live keys\n  - sometokenvaluehere\n  - anothertokenvalue\n",
			want: "apikey:\n  # two live keys\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "several comments in a row",
			text: "apikey:\n  - sometokenvaluehere\n  # rotated\n  # by platform\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # rotated\n  # by platform\n  - [REDACTED]\n",
		},
		{
			name: "a comment then a blank line then an item",
			text: "apikey:\n  - sometokenvaluehere\n  # rotated\n\n  - anothertokenvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # rotated\n\n  - [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("an item after a comment leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Skipping comments must not carry the sequence into whatever follows it: a
// comment is transparent, not an extension of the credential key's scope.
func TestACommentDoesNotCarryACredentialSequenceIntoTheNextKey(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a comment separating a credential list from a sibling list",
			text: "apikey:\n  - sometokenvaluehere\n  # unrelated\nimages:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n  # unrelated\nimages:\n  - nginxlatestimage\n",
		},
		{
			name: "a comment before a document separator",
			text: "apikey:\n  - sometokenvaluehere\n# next doc\n---\nimages:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n# next doc\n---\nimages:\n  - nginxlatestimage\n",
		},
		{
			name: "a comment before a dedented key and its list",
			text: "spec:\n  apikey:\n    - sometokenvaluehere\n  # images follow\n  images:\n    - nginxlatestimage\n",
			want: "spec:\n  apikey:\n    - [REDACTED]\n  # images follow\n  images:\n    - nginxlatestimage\n",
		},
		{
			name: "a comment before a column-0 list stays out of C-L61's exemption",
			text: "apikey:\n  - sometokenvaluehere\n  # then a reference list\n- someopaquereference\n",
			want: "apikey:\n  - [REDACTED]\n  # then a reference list\n- someopaquereference\n",
		},
		{
			name: "a comment before a deeper nested list",
			text: "apikey:\n  - sometokenvaluehere\n  # nested\n  extra:\n    - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n  # nested\n  extra:\n    - nginxlatestimage\n",
		},
		{
			name: "a commented credential key opens its own sequence at its own indent",
			text: "apikey:\n  - secretone\n# db_pass:\n    - secrettwovalue\n",
			want: "apikey:\n  - [REDACTED]\n# db_pass:\n    - [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a comment widened the sequence past its key\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A rotated-out secret left behind as a commented item is still a live secret.
func TestACommentedOutItemOfACredentialSequenceIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a commented item after a live one",
			text: "apikey:\n  - sometokenvaluehere\n  #- oldsecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  #- [REDACTED]\n",
		},
		{
			name: "a commented item with a space after the hash",
			text: "apikey:\n  - sometokenvaluehere\n  # - oldsecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # - [REDACTED]\n",
		},
		{
			name: "a commented item before a live one",
			text: "apikey:\n  #- oldsecretvalue\n  - sometokenvaluehere\n",
			want: "apikey:\n  #- [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "only commented items",
			text: "db_pass:\n  #- oldsecretvalue\n  #- oldersecretvalue\n",
			want: "db_pass:\n  #- [REDACTED]\n  #- [REDACTED]\n",
		},
		{
			name: "a commented item at a different indent than the live ones",
			text: "apikey:\n  - sometokenvaluehere\n    #- oldsecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n    #- [REDACTED]\n",
		},
		{
			name: "a commented item carrying its own trailing comment",
			text: "apikey:\n  #- oldsecretvalue # rotated 2026-01\n",
			want: "apikey:\n  #- [REDACTED] # rotated 2026-01\n",
		},
		{
			name: "a quoted commented item",
			text: "apikey:\n  #- \"oldsecretvalue\"\n",
			want: "apikey:\n  #- \"[REDACTED]\"\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a commented-out credential leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Redacting a commented item must not reach prose: a comment is where an
// operator writes down what broke, and it is only eaten where it is a
// commented-out item of a credential key's own list.
func TestACommentIsOnlyEatenWhereItIsACommentedOutCredentialItem(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "a prose comment inside a credential sequence",
			text: "apikey:\n  - sometokenvaluehere\n  # rotated quarterly by platform\n",
		},
		{
			name: "a commented item under an ordinary key",
			text: "images:\n  - nginxlatestimage\n  #- oldnginximage\n",
		},
		{
			name: "a commented item after the sequence has closed",
			text: "apikey:\n  - sometokenvaluehere\nimages:\n  - nginxlatestimage\n  #- oldnginximage\n",
		},
		{
			name: "a commented object reference keeps its identity",
			text: "apikey:\n  - sometokenvaluehere\n  #- cluster-secret-store/backend\n",
		},
		{
			name: "a column-0 commented item stays out of C-L61's exemption",
			text: "apikey:\n  - sometokenvaluehere\n#- someopaquereference\n",
		},
		{
			name: "a commented mapping is not an item",
			text: "apikey:\n  - sometokenvaluehere\n  #- name: primary\n",
		},
		{
			name: "a bare commented word is not an item",
			text: "apikey:\n  - sometokenvaluehere\n  # oldsecretvalue\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := strings.ReplaceAll(test.text, "sometokenvaluehere", "[REDACTED]")
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != want {
				t.Fatalf("a comment was over-redacted\n want: %s\n  got: %s", want, scrubbed)
			}
		})
	}
}

// A hunk that adds or removes a credential list is the shape a Renovate diff
// actually carries, and its items are secrets in both directions.
func TestACredentialSequenceInsideADiffHunkIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "an added key and its added items",
			text: "+apikey:\n+  - sometokenvaluehere\n+  - anothertokenvalue\n",
			want: "+apikey:\n+  - [REDACTED]\n+  - [REDACTED]\n",
		},
		{
			name: "a removed key and its removed items",
			text: "-apikey:\n-  - sometokenvaluehere\n-  - anothertokenvalue\n",
			want: "-apikey:\n-  - [REDACTED]\n-  - [REDACTED]\n",
		},
		{
			name: "an added nested key and its added items",
			text: "+spec:\n+  apikey:\n+    - sometokenvaluehere\n+    - anothertokenvalue\n",
			want: "+spec:\n+  apikey:\n+    - [REDACTED]\n+    - [REDACTED]\n",
		},
		{
			name: "an added short pass key",
			text: "+db_pass:\n+  - hunter2correcthorse\n",
			want: "+db_pass:\n+  - [REDACTED]\n",
		},
		{
			name: "a comment inside an added sequence",
			text: "+apikey:\n+  - sometokenvaluehere\n+  # rotated\n+  - anothertokenvalue\n",
			want: "+apikey:\n+  - [REDACTED]\n+  # rotated\n+  - [REDACTED]\n",
		},
		{
			name: "a commented-out item inside an added sequence",
			text: "+apikey:\n+  - sometokenvaluehere\n+  #- oldsecretvalue\n",
			want: "+apikey:\n+  - [REDACTED]\n+  #- [REDACTED]\n",
		},
		{
			name: "a context item under an added key",
			text: "+apikey:\n   - sometokenvaluehere\n",
			want: "+apikey:\n   - [REDACTED]\n",
		},
		{
			name: "an added item under a removed key",
			text: "-apikey:\n+  - sometokenvaluehere\n",
			want: "-apikey:\n+  - [REDACTED]\n",
		},
		{
			name: "the shape git diff actually emits: a context key and added items",
			text: "@@ -4,6 +4,9 @@ metadata:\n spec:\n   images:\n-    - nginxoldimage\n+    - nginxnewimage\n   apikey:\n     - firsttokenvaluehere\n+    - secondtokenvaluehere\n+    # rotated\n+    #- oldtokenvaluehere\n   remoteRef:\n     - cluster-secret-store/backend\n",
			want: "@@ -4,6 +4,9 @@ metadata:\n spec:\n   images:\n-    - nginxoldimage\n+    - nginxnewimage\n   apikey:\n     - [REDACTED]\n+    - [REDACTED]\n+    # rotated\n+    #- [REDACTED]\n   remoteRef:\n     - cluster-secret-store/backend\n",
		},
		{
			name: "a removed item under a context key",
			text: " apikey:\n-  - sometokenvaluehere\n",
			want: " apikey:\n-  - [REDACTED]\n",
		},
		{
			name: "a context column-0 item under an added pass key, which redacts unprefixed",
			text: "+db_pass:\n - s3cr3topaquevalue\n - secondopaquevalue\n",
			want: "+db_pass:\n - [REDACTED]\n - [REDACTED]\n",
		},
		{
			name: "an added column-0 item under an added pass key",
			text: "+db_pass:\n+- s3cr3topaquevalue\n",
			want: "+db_pass:\n+- [REDACTED]\n",
		},
		{
			name: "a key renamed into the pass family keeps its context items redacted",
			text: "-dbpass:\n+db_pass:\n - s3cr3topaquevalue\n",
			want: "-dbpass:\n+db_pass:\n - [REDACTED]\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a credential sequence in a hunk leaked\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Tolerating a diff marker must not reopen C-L61: the marker is stripped before
// the indent is read, so a dash at column 0 of the hunk's content is the same
// reference structure C-L61/C-L85 leave alone, and the walk still ends where the
// key's own list does.
func TestADiffMarkerIsStrippedBeforeTheColumn0ExemptionIsApplied(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a column-0 item under an added key stays a reference",
			text: "+apikey:\n+- someopaquereference\n",
			want: "+apikey:\n+- someopaquereference\n",
		},
		{
			name: "a column-0 item under a removed key stays a reference",
			text: "-apikey:\n-- someopaquereference\n",
			want: "-apikey:\n-- someopaquereference\n",
		},
		{
			name: "a column-0 context item under an added key stays a reference",
			text: "+apikey:\n - someopaquereference\n",
			want: "+apikey:\n - someopaquereference\n",
		},
		{
			name: "an added ordinary key's list is untouched",
			text: "+images:\n+  - nginxlatestimage\n+  - redislatestimage\n",
			want: "+images:\n+  - nginxlatestimage\n+  - redislatestimage\n",
		},
		{
			name: "an added object reference keeps its identity",
			text: "+apikey:\n+  - cluster-secret-store/backend\n",
			want: "+apikey:\n+  - cluster-secret-store/backend\n",
		},
		{
			name: "a following added key ends the sequence",
			text: "+apikey:\n+  - sometokenvaluehere\n+images:\n+  - nginxlatestimage\n",
			want: "+apikey:\n+  - [REDACTED]\n+images:\n+  - nginxlatestimage\n",
		},
		{
			name: "a line with no marker ends the sequence",
			text: "+apikey:\n+  - sometokenvaluehere\nimages:\n  - nginxlatestimage\n",
			want: "+apikey:\n+  - [REDACTED]\nimages:\n  - nginxlatestimage\n",
		},
		{
			name: "an unprefixed key does not gain a marker tolerance",
			text: "apikey:\n+  - someopaquereference\n",
			want: "apikey:\n+  - someopaquereference\n",
		},
		{
			name: "an item dedented below an indented key keeps its raw reading",
			text: "  apikey:\n - sometokenvaluehere\n",
			want: "  apikey:\n - [REDACTED]\n",
		},
		{
			name: "a context sibling list beside a context credential list survives",
			text: " apikey:\n+  - sometokenvaluehere\n images:\n+  - nginxlatestimage\n",
			want: " apikey:\n+  - [REDACTED]\n images:\n+  - nginxlatestimage\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("the diff tolerance crossed a boundary\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A commented-out item follows its family's reading of a column-0 dash, the same
// as a live one: no exemption under a pass key, C-L61/C-L85's reference structure
// under a named key.
func TestAColumn0CommentedItemFollowsItsKeyFamily(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a column-0 commented item under a pass key is a rotated secret",
			text: "db_pass:\n- livesecretvalue\n#- oldsecretvalue\n",
			want: "db_pass:\n- [REDACTED]\n#- [REDACTED]\n",
		},
		{
			name: "a column-0 commented item under an indented pass list too",
			text: "db_pass:\n  - livesecretvalue\n#- oldsecretvalue\n",
			want: "db_pass:\n  - [REDACTED]\n#- [REDACTED]\n",
		},
		{
			name: "a column-0 commented item inside a pass hunk",
			text: "+db_pass:\n+- livesecretvalue\n+#- oldsecretvalue\n",
			want: "+db_pass:\n+- [REDACTED]\n+#- [REDACTED]\n",
		},
		{
			name: "a column-0 commented item under a named key stays a reference",
			text: "apikey:\n  - livesecretvalue\n#- someopaquereference\n",
			want: "apikey:\n  - [REDACTED]\n#- someopaquereference\n",
		},
		{
			name: "an indented commented item under a named key still redacts",
			text: "apikey:\n  - livesecretvalue\n  #- oldsecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  #- [REDACTED]\n",
		},
		{
			name: "an ordinary key's commented item is untouched",
			text: "images:\n  - nginxlatestimage\n#- redislatestimage\n",
			want: "images:\n  - nginxlatestimage\n#- redislatestimage\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a column-0 commented item was read wrong\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The comment rule reaches the item shape and nothing else. A comment is where
// an operator records what broke, so a bare note, a "/"-bearing reference and a
// mapping line all keep their text — and a mapping line ends the sequence,
// because a list and a mapping cannot share a level and guessing which one won
// would carry the walk into whatever follows.
func TestOnlyACommentedItemShapeIsEatenInsideACredentialSequence(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a dashless commented scalar is a note, not an item",
			text: "apikey:\n  - livesecretvalue\n  #  oldsecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  #  oldsecretvalue\n",
		},
		{
			name: "a one-word note inside the list survives",
			text: "apikey:\n  - livesecretvalue\n  # rotated\n  - othersecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # rotated\n  - [REDACTED]\n",
		},
		{
			name: "a commented object reference keeps its identity",
			text: "apikey:\n  - livesecretvalue\n  #- clustersecretstore/backend\n",
			want: "apikey:\n  - [REDACTED]\n  #- clustersecretstore/backend\n",
		},
		{
			name: "a mapping line with a value ends the sequence",
			text: "apikey:\n  - livesecretvalue\n  extra: x\n  - othersecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  extra: x\n  - othersecretvalue\n",
		},
		{
			name: "a mapping line ends a pass key's sequence too",
			text: "db_pass:\n  - livesecretvalue\n  extra: x\n  - othersecretvalue\n",
			want: "db_pass:\n  - [REDACTED]\n  extra: x\n  - othersecretvalue\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("the comment rule crossed its own shape\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// Commenting a key out does not move the items under it: they still belong to
// the credential key above, so the walk keeps them. The price is an accepted
// over-redaction — an image tag below a commented-out `# images:` is eaten —
// and it is the cheaper side of the trade, because a rule that let a commented
// key close the sequence would publish every live item under any comment that
// happens to end in a colon.
func TestACommentedOutKeyDoesNotCloseACredentialSequence(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "the accepted price: an image tag under a commented-out key",
			text: "apikey:\n  - livesecretvalue\n# images:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\n# images:\n  - [REDACTED]\n",
		},
		{
			name: "the leak it buys: a live secret under a commented-out key",
			text: "apikey:\n  - livesecretvalue\n# rotated:\n  - stillasecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n# rotated:\n  - [REDACTED]\n",
		},
		{
			name: "an indented commented-out key does not close it either",
			text: "apikey:\n  - livesecretvalue\n  # images:\n  - stillasecretvalue\n",
			want: "apikey:\n  - [REDACTED]\n  # images:\n  - [REDACTED]\n",
		},
		{
			name: "a commented-out key under a pass key does not close it",
			text: "db_pass:\n  - livesecretvalue\n# images:\n  - stillasecretvalue\n",
			want: "db_pass:\n  - [REDACTED]\n# images:\n  - [REDACTED]\n",
		},
		{
			name: "an uncommented sibling key does close it",
			text: "apikey:\n  - livesecretvalue\nimages:\n  - nginxlatestimage\n",
			want: "apikey:\n  - [REDACTED]\nimages:\n  - nginxlatestimage\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a commented-out key changed where the sequence ends\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// The column-0 exemption belongs to the named family alone: the "pass" rule
// tolerates a leading marker everywhere, so a column-0 dash under a pass key is
// a value line and every item of the list is one, not just the first.
func TestEveryItemOfAColumn0PassKeyListIsScrubbed(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a raw three-item list under a prefixed pass key",
			text: "db_pass:\n- firstsecretvalue\n- secondsecretvalue\n- thirdsecretvalue\n",
			want: "db_pass:\n- [REDACTED]\n- [REDACTED]\n- [REDACTED]\n",
		},
		{
			name: "a raw list under a bare pass key",
			text: "pass:\n- firstsecretvalue\n- secondsecretvalue\n",
			want: "pass:\n- [REDACTED]\n- [REDACTED]\n",
		},
		{
			name: "a diff-added list still redacts every item",
			text: "+db_pass:\n+- firstsecretvalue\n+- secondsecretvalue\n",
			want: "+db_pass:\n+- [REDACTED]\n+- [REDACTED]\n",
		},
		{
			name: "an object reference keeps its identity",
			text: "db_pass:\n- clustersecretstore/backend\n- otherstore/backend\n",
			want: "db_pass:\n- [REDACTED]\n- otherstore/backend\n",
		},
		{
			name: "a following key ends the list",
			text: "db_pass:\n- firstsecretvalue\nimages:\n- nginxlatestimage\n",
			want: "db_pass:\n- [REDACTED]\nimages:\n- nginxlatestimage\n",
		},
		{
			name: "an item at another indent is not a sibling",
			text: "db_pass:\n- firstsecretvalue\n  - nginxlatestimage\n",
			want: "db_pass:\n- [REDACTED]\n  - nginxlatestimage\n",
		},
		{
			name: "the named family keeps C-L61's column-0 exemption",
			text: "apikey:\n- someopaquereference\n- anotheropaquereference\n",
			want: "apikey:\n- someopaquereference\n- anotheropaquereference\n",
		},
		{
			name: "an ordinary key's column-0 list is untouched",
			text: "images:\n- nginxlatestimage\n- redislatestimage\n",
			want: "images:\n- nginxlatestimage\n- redislatestimage\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a column-0 pass-key list was read wrong\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}
}

// A key opens a block sequence only where it is structure. A sentence that
// happens to end in a credential word is prose, and the bullet list under it is
// the operator's evidence — Flux names the objects that failed that way.
func TestOnlyAStructuralPassKeyOpensACredentialSequence(t *testing.T) {
	survives := []struct {
		name string
		text string
	}{
		{
			name: "a flux error ending in a pass key over a bullet list",
			text: "reconciliation failed to load db_pass:\n  - HelmReleasepodinfo\n  - Kustomizationbase\n",
		},
		{
			name: "a sentence separated from the key by a comma",
			text: "could not decode the secret,pass:\n  - HelmReleasepodinfo\n  - Kustomizationbase\n",
		},
		{
			name: "a path-qualified name is a reference not a key",
			text: "flux-system/db_pass:\n  - HelmReleasepodinfo\n  - Kustomizationbase\n",
		},
		{
			name: "a sentence whose key word stands alone",
			text: "the check did not pass:\n  - HelmReleasepodinfo\n  - Kustomizationbase\n",
		},
	}
	for _, test := range survives {
		t.Run("prose/"+test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.text {
				t.Fatalf("a prose sentence opened a credential sequence\n want: %s\n  got: %s", test.text, scrubbed)
			}
		})
	}

	structural := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a bare pass key",
			text: "pass:\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "an underscore-prefixed pass key",
			text: "db_pass:\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "db_pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "a dotted pass key",
			text: "spec.db_pass:\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "spec.db_pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "a quoted pass key",
			text: "\"db_pass\":\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "\"db_pass\":\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "an indented pass key",
			text: "spec:\n  db_pass:\n    - hunter2correcthorse\n    - othersecretvalue\n",
			want: "spec:\n  db_pass:\n    - [REDACTED]\n    - [REDACTED]\n",
		},
		{
			name: "a sequence item whose own key is a pass key",
			text: "stores:\n  - db_pass:\n      - hunter2correcthorse\n      - othersecretvalue\n",
			want: "stores:\n  - db_pass:\n      - [REDACTED]\n      - [REDACTED]\n",
		},
		{
			name: "a diff-added pass key",
			text: "+db_pass:\n+  - hunter2correcthorse\n+  - othersecretvalue\n",
			want: "+db_pass:\n+  - [REDACTED]\n+  - [REDACTED]\n",
		},
		{
			name: "a leading-underscore pass key",
			text: "_pass:\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "_pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
		{
			name: "a double-underscore pass key",
			text: "__pass:\n  - hunter2correcthorse\n  - othersecretvalue\n",
			want: "__pass:\n  - [REDACTED]\n  - [REDACTED]\n",
		},
	}
	for _, test := range structural {
		t.Run("structural/"+test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a structural pass key lost its sequence\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}

	t.Run("a mid-sentence pass key in a hunk header is the key its items belong to", func(t *testing.T) {
		// Byte-for-byte `git diff --no-index` output over a rotated list item.
		text := "diff --git a/creds.yaml b/creds.yaml\n" +
			"index 2c29cf6..9ea4699 100644\n" +
			"--- a/creds.yaml\n" +
			"+++ b/creds.yaml\n" +
			"@@ -4,4 +4,4 @@ db_pass:\n" +
			" - thirdsecretvalue\n" +
			" - fourthsecretvalue\n" +
			" - fifthsecretvalue\n" +
			"-- sixthsecretvalue\n" +
			"+- rotatedsecretvalue\n"
		want := "diff --git a/creds.yaml b/creds.yaml\n" +
			"index 2c29cf6..9ea4699 100644\n" +
			"--- a/creds.yaml\n" +
			"+++ b/creds.yaml\n" +
			"@@ -4,4 +4,4 @@ db_pass:\n" +
			" - [REDACTED]\n" +
			" - [REDACTED]\n" +
			" - [REDACTED]\n" +
			"-- [REDACTED]\n" +
			"+- [REDACTED]\n"
		if scrubbed := diagnostics.ScrubSecrets(text); scrubbed != want {
			t.Fatalf("a hunk header's credential key lost its list\n want: %s\n  got: %s", want, scrubbed)
		}
	})

	headings := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a dotted key as the heading",
			text: "@@ -4,4 +4,4 @@ spec.db_pass:\n - thirdsecretvalue\n+- rotatedsecretvalue\n",
			want: "@@ -4,4 +4,4 @@ spec.db_pass:\n - [REDACTED]\n+- [REDACTED]\n",
		},
		{
			name: "a carriage-return heading and items",
			text: "@@ -4,4 +4,4 @@ db_pass:\r\n - thirdsecretvalue\r\n+- rotatedsecretvalue\r\n",
			want: "@@ -4,4 +4,4 @@ db_pass:\n - [REDACTED]\n+- [REDACTED]\n",
		},
		{
			name: "a named key as the heading",
			text: "@@ -4,4 +4,4 @@ apikey:\n   - thirdsecretvalue\n+  - rotatedsecretvalue\n",
			want: "@@ -4,4 +4,4 @@ apikey:\n   - [REDACTED]\n+  - [REDACTED]\n",
		},
		{
			name: "an ordinary key as the heading opens nothing",
			text: "@@ -4,4 +4,4 @@ images:\n - nginxlatestimage\n+- redislatestimage\n",
			want: "@@ -4,4 +4,4 @@ images:\n - nginxlatestimage\n+- redislatestimage\n",
		},
		{
			name: "a prose heading opens nothing",
			text: "@@ -4,4 +4,4 @@ failed to load db_pass:\n - HelmReleasepodinfo\n",
			want: "@@ -4,4 +4,4 @@ failed to load db_pass:\n - HelmReleasepodinfo\n",
		},
		{
			name: "a headingless hunk opens nothing",
			text: "@@ -4,4 +4,4 @@\n - HelmReleasepodinfo\n",
			want: "@@ -4,4 +4,4 @@\n - HelmReleasepodinfo\n",
		},
		{
			name: "the next hunk header ends the previous heading's list",
			text: "@@ -4,4 +4,4 @@ db_pass:\n - thirdsecretvalue\n@@ -40,2 +40,2 @@ images:\n - nginxlatestimage\n",
			want: "@@ -4,4 +4,4 @@ db_pass:\n - [REDACTED]\n@@ -40,2 +40,2 @@ images:\n - nginxlatestimage\n",
		},
	}
	for _, test := range headings {
		t.Run("heading/"+test.name, func(t *testing.T) {
			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("a hunk heading was read wrong\n want: %s\n  got: %s", test.want, scrubbed)
			}
		})
	}

	t.Run("a commented sentence ending in a pass key opens nothing", func(t *testing.T) {
		text := "apikey:\n  - secretonevalue\n# failed to load db_pass:\n    - HelmReleasepodinfo\n"
		want := "apikey:\n  - [REDACTED]\n# failed to load db_pass:\n    - HelmReleasepodinfo\n"
		if scrubbed := diagnostics.ScrubSecrets(text); scrubbed != want {
			t.Fatalf("a commented sentence opened a credential sequence\n want: %s\n  got: %s", want, scrubbed)
		}
	})

	t.Run("a mid-sentence pass value is still redacted", func(t *testing.T) {
		text := "error reading pass: hunter2correcthorse from vault\n"
		want := "error reading pass: [REDACTED] from vault\n"
		if scrubbed := diagnostics.ScrubSecrets(text); scrubbed != want {
			t.Fatalf("the prose value rule was disarmed\n want: %s\n  got: %s", want, scrubbed)
		}
	})
}

// Every sink drops the runes that show a reader nothing, so the rules are run
// against that text and not against what a hostile author typed. Line and column
// structure is exempt, because the key-name rules read it.
func TestScrubbingSeesTheTextItsSinksWillRenderNotWhatWasTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{"a zero width space inside a credential", "AKIA\u200bIOSFODNN7EXAMPLE", "[REDACTED]"},
		{"a bidi override inside a credential", "ghp_\u202eabcdefghijklmnop0123", "[REDACTED]"},
		{"a soft hyphen inside a key name", "pass\u00adword: hunter2correcthorse", "password: [REDACTED]"},
		{"a carriage return inside a credential", "AKIA\rIOSFODNN7EXAMPLE", "[REDACTED]"},
		{"windows line endings become unix ones", "a\r\nb", "a\nb"},
		{"newlines are kept, the key rules read them", "password:\n  hunter2correcthorse", "password:\n  [REDACTED]"},
		{"tabs are kept", "a\tb", "a\tb"},
		{"a line separator is line structure and is kept", "a\u2028b", "a\u2028b"},
		{"ordinary text is untouched", "Deployment/flux-system/podinfo failed", "Deployment/flux-system/podinfo failed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if scrubbed := diagnostics.ScrubSecrets(test.text); scrubbed != test.want {
				t.Fatalf("the rules read the wrong text\n want: %q\n  got: %q", test.want, scrubbed)
			}
		})
	}
}
