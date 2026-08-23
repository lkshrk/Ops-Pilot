package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

var (
	// Platform tokens come from the registry rather than the operator, so they stay on the
	// narrower form every real index uses.
	platformPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

type descriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls"`
	Platform  struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

// attestationDescriptor reports whether an index child is build provenance or an SBOM rather than
// a runnable image. Build tooling emits these as `unknown/unknown` platforms, so several may share
// that placeholder without describing conflicting images.
func attestationDescriptor(d descriptor) bool {
	if d.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
		return true
	}
	return d.Platform.OS == "unknown" && d.Platform.Architecture == "unknown"
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Manifests     []descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations"`
}
type loaded struct {
	metadata        Metadata
	manifest        manifest
	reference       reference
	sourceDeclared  bool
	configCandidate descriptor
}

func (c *Client) loadRoot(ctx context.Context, r reference) (loaded, error) {
	if err := c.pingV2(ctx, r.authority); err != nil {
		return loaded{}, err
	}
	path := "/v2/" + r.name + "/manifests/" + r.ref()
	head, _, err := c.operation(ctx, r.authority, http.MethodHead, path, "", 0)
	if err != nil {
		return loaded{}, err
	}
	headDigest := head.Header.Get("Docker-Content-Digest")
	head.Body.Close()
	get, body, err := c.operation(ctx, r.authority, http.MethodGet, path, "", manifestLimit)
	if err != nil {
		return loaded{}, err
	}
	getDigest := get.Header.Get("Docker-Content-Digest")
	get.Body.Close()
	d := bodyDigest(body)
	if r.digest != "" && d != r.digest {
		return loaded{}, category(ErrIntegrity, "requested digest mismatch")
	}
	if err := matchingDigests(d, headDigest, getDigest); err != nil {
		return loaded{}, err
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil || m.SchemaVersion != 2 {
		return loaded{}, category(ErrIntegrity, "invalid OCI manifest")
	}
	media := m.MediaType
	if media == "" {
		media = get.Header.Get("Content-Type")
		media, _, _ = mime.ParseMediaType(media)
	}
	if media != manifestOCI && media != manifestDocker && media != indexOCI && media != indexDocker {
		return loaded{}, category(ErrIntegrity, "unsupported OCI media type")
	}
	// A tag is only a lookup locator. The resolved identity is always pinned to
	// the verified manifest/index digest and is never presented as a runtime
	// identity.
	meta := Metadata{Digest: d, Annotations: cloneAnnotations(m.Annotations), Identity: domain.ArtifactIdentity{Reference: r.authority + "/" + r.name + "@" + d}}
	declared, found := provenanceOf(m.Annotations)
	meta.Source, meta.Revision = declared.source, declared.revision
	if media == manifestOCI || media == manifestDocker {
		meta.Identity.Digest = d
		return loaded{metadata: meta, manifest: m, reference: r, sourceDeclared: found, configCandidate: m.Config}, nil
	}
	if len(m.Manifests) == 0 || len(m.Manifests) > maxDescriptors {
		return loaded{}, category(ErrIntegrity, "invalid index descriptors")
	}
	platforms := make([]domain.PlatformIdentity, 0, len(m.Manifests))
	var configCandidate descriptor
	seenPlatforms, seenDigests := map[string]string{}, map[string]bool{}
	bytesUsed := len(body)
	requests := 1
	for _, child := range m.Manifests {
		if attestationDescriptor(child) {
			continue
		}
		if child.MediaType != manifestOCI && child.MediaType != manifestDocker || !validDescriptor(child) {
			return loaded{}, category(ErrIntegrity, "invalid index child")
		}
		if !validPlatform(child) {
			return loaded{}, category(ErrIntegrity, "invalid platform")
		}
		key := child.Platform.OS + "\x00" + child.Platform.Architecture + "\x00" + child.Platform.Variant
		if seenDigests[child.Digest] {
			return loaded{}, category(ErrIntegrity, "duplicate platform")
		}
		if previous, ok := seenPlatforms[key]; ok && previous != child.Digest {
			return loaded{}, category(ErrIntegrity, "conflicting platform descriptors")
		}
		seenPlatforms[key], seenDigests[child.Digest] = child.Digest, true
		requests++
		if requests > maxManifestRequests {
			return loaded{}, category(ErrIntegrity, "manifest request cap")
		}
		res, childBody, err := c.operation(ctx, r.authority, http.MethodGet, "/v2/"+r.name+"/manifests/"+child.Digest, "", manifestLimit)
		if err != nil {
			return loaded{}, err
		}
		childHeaderDigest := res.Header.Get("Docker-Content-Digest")
		res.Body.Close()
		bytesUsed += len(childBody)
		if bytesUsed > maxManifestBytes || int64(len(childBody)) != child.Size || bodyDigest(childBody) != child.Digest || matchingDigests(child.Digest, childHeaderDigest) != nil {
			return loaded{}, category(ErrIntegrity, "child manifest mismatch")
		}
		var cm manifest
		if json.Unmarshal(childBody, &cm) != nil || cm.SchemaVersion != 2 || (cm.MediaType != manifestOCI && cm.MediaType != manifestDocker) {
			return loaded{}, category(ErrIntegrity, "nested or invalid child manifest")
		}
		if !found {
			if declared, found = provenanceOf(cm.Annotations); found {
				meta.Source, meta.Revision = declared.source, declared.revision
			}
			if configCandidate.Digest == "" {
				configCandidate = cm.Config
			}
		}
		platforms = append(platforms, domain.PlatformIdentity{OS: child.Platform.OS, Architecture: child.Platform.Architecture, Variant: child.Platform.Variant, Digest: child.Digest})
	}
	if len(platforms) == 0 {
		return loaded{}, category(ErrIntegrity, "index has no platform manifests")
	}
	sort.Slice(platforms, func(i, j int) bool {
		a, b := platforms[i], platforms[j]
		if a.OS != b.OS {
			return a.OS < b.OS
		}
		if a.Architecture != b.Architecture {
			return a.Architecture < b.Architecture
		}
		if a.Variant != b.Variant {
			return a.Variant < b.Variant
		}
		return a.Digest < b.Digest
	})
	meta.Identity.IndexDigest, meta.Identity.Platforms = d, platforms
	return loaded{metadata: meta, manifest: m, reference: r, sourceDeclared: found, configCandidate: configCandidate}, nil
}

// provenance is taken whole from the one layer that declares a source, so a source and a revision
// can never be reported from two different manifests.
type provenance struct{ source, revision string }

func provenanceOf(values map[string]string) (provenance, bool) {
	source := values[sourceAnnotation]
	if source == "" {
		return provenance{}, false
	}
	return provenance{source: source, revision: values[revisionAnnotation]}, true
}

// configProvenance reads the Dockerfile labels of an image config when no manifest declared a
// source. The label is optional evidence: a config that cannot be fetched or cannot be verified
// against its descriptor leaves the metadata as it was rather than failing a sound resolve, so no
// unverified byte ever reaches the caller.
func (c *Client) configProvenance(ctx context.Context, r reference, d descriptor) (provenance, error) {
	if d.MediaType != imageConfigOCI && d.MediaType != imageConfigDocker {
		return provenance{}, nil
	}
	if !validDescriptor(d) || len(d.URLs) != 0 || d.Size <= 0 || d.Size > manifestLimit {
		return provenance{}, nil
	}
	res, body, err := c.operation(ctx, r.authority, http.MethodGet, "/v2/"+r.name+"/blobs/"+d.Digest, "", d.Size)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return provenance{}, cause
		}
		if cause := contextCause(err); cause != nil {
			return provenance{}, cause
		}
		return provenance{}, nil
	}
	res.Body.Close()
	if int64(len(body)) != d.Size || bodyDigest(body) != d.Digest || matchingDigests(d.Digest, res.Header.Get("Docker-Content-Digest")) != nil {
		return provenance{}, nil
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if json.Unmarshal(body, &config) != nil {
		return provenance{}, nil
	}
	declared, _ := provenanceOf(config.Config.Labels)
	return declared, nil
}

func (c *Client) resolveManifest(ctx context.Context, raw string) (loaded, error) {
	r, err := parseReference(raw)
	if err != nil {
		return loaded{}, err
	}
	return c.loadRoot(ctx, r)
}

func bodyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func validDescriptor(d descriptor) bool { return digestPattern.MatchString(d.Digest) && d.Size >= 0 }
func validPlatform(d descriptor) bool {
	return platformPattern.MatchString(d.Platform.OS) &&
		platformPattern.MatchString(d.Platform.Architecture) &&
		(d.Platform.Variant == "" || platformPattern.MatchString(d.Platform.Variant))
}
func matchingDigests(want string, got ...string) error {
	for _, value := range got {
		if value != "" && strings.ToLower(value) != want {
			return category(ErrIntegrity, "OCI digest mismatch")
		}
	}
	return nil
}
func cloneAnnotations(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}
