package oci

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

type Metadata struct {
	Identity                 domain.ArtifactIdentity
	Digest, Source, Revision string
	Annotations              map[string]string
}
type ArtifactVerification struct {
	Metadata Metadata
	Blobs    []domain.ArtifactBlobProof
}

func (c *Client) Resolve(ctx context.Context, raw string) (Metadata, error) {
	r, err := parseReference(raw)
	if err != nil {
		return Metadata{}, err
	}
	ctx, cancel := c.callDeadline(withRetryBudget(ctx))
	defer cancel()
	root, err := c.loadRoot(ctx, r)
	if err != nil {
		return Metadata{}, err
	}
	if !root.sourceDeclared {
		labelled, err := c.configProvenance(ctx, r, root.configCandidate)
		if err != nil {
			return Metadata{}, err
		}
		root.metadata.Source, root.metadata.Revision = labelled.source, labelled.revision
	}
	return root.metadata, nil
}

func (c *Client) VerifyArtifact(ctx context.Context, raw string) (ArtifactVerification, error) {
	ctx, cancel := c.callDeadline(withRetryBudget(ctx))
	defer cancel()
	root, err := c.resolveManifest(ctx, raw)
	if err != nil {
		return ArtifactVerification{}, err
	}
	if root.metadata.Identity.IndexDigest != "" || !validDescriptor(root.manifest.Config) || root.manifest.Config.MediaType != chartConfig || len(root.manifest.Layers) == 0 || len(root.manifest.Layers)+1 > maxBlobs {
		return ArtifactVerification{}, category(ErrIntegrity, "invalid chart artifact")
	}
	descriptors := append([]descriptor{root.manifest.Config}, root.manifest.Layers...)
	proof := make([]domain.ArtifactBlobProof, 0, len(descriptors))
	var total int64
	for i, d := range descriptors {
		if !validDescriptor(d) || len(d.URLs) != 0 || d.Size > maxBlobBytes || total > maxArtifactBytes-d.Size || (i > 0 && d.MediaType != chartLayer) {
			return ArtifactVerification{}, category(ErrIntegrity, "invalid chart blob")
		}
		total += d.Size
		limit := d.Size
		if limit == 0 {
			limit = 1
		}
		res, body, err := c.operation(ctx, root.reference.authority, http.MethodGet, "/v2/"+root.reference.name+"/blobs/"+d.Digest, "", limit)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return ArtifactVerification{}, cause
			}
			return ArtifactVerification{}, fmt.Errorf("chart blob retrieval: %w", err)
		}
		closeErr := res.Body.Close()
		if cause := context.Cause(ctx); cause != nil {
			return ArtifactVerification{}, cause
		}
		if closeErr != nil {
			return ArtifactVerification{}, unavailable(closeErr, "close chart blob")
		}
		if int64(len(body)) != d.Size || bodyDigest(body) != d.Digest || matchingDigests(d.Digest, res.Header.Get("Docker-Content-Digest")) != nil {
			return ArtifactVerification{}, category(ErrIntegrity, "chart blob verification failed")
		}
		kind := "layer"
		if i == 0 {
			kind = "config"
		}
		proof = append(proof, domain.ArtifactBlobProof{Kind: kind, Digest: d.Digest, Size: d.Size})
	}
	sort.Slice(proof, func(i, j int) bool {
		if proof[i].Kind != proof[j].Kind {
			return proof[i].Kind < proof[j].Kind
		}
		return proof[i].Digest < proof[j].Digest
	})
	return ArtifactVerification{Metadata: root.metadata, Blobs: proof}, nil
}
