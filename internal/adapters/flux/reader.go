// Package flux reads the status-only subset of Flux resources needed to snapshot
// cluster health and to confirm that a merge commit has been reconciled.
package flux

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/lkshrk/ops-pilot/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

var (
	gitRepositories = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	kustomizations  = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	helmReleases    = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

type Client struct {
	client dynamic.Interface
	source domain.ObjectRef
}

func New(client dynamic.Interface, discoveryClient discovery.DiscoveryInterface, source domain.ObjectRef) (*Client, error) {
	if nilInterface(client) || nilInterface(discoveryClient) {
		return nil, fmt.Errorf("Flux dynamic and discovery clients are required")
	}
	if source.Kind != "GitRepository" || source.Namespace == "" || source.Name == "" {
		return nil, fmt.Errorf("configured Flux source must be a GitRepository")
	}
	for _, required := range []schema.GroupVersionResource{gitRepositories, kustomizations, helmReleases} {
		resources, err := discoveryClient.ServerResourcesForGroupVersion(required.GroupVersion().String())
		if err != nil {
			return nil, fmt.Errorf("discover required Flux API %s: %w", required.GroupVersion(), err)
		}
		if !serves(resources, required.Resource) {
			return nil, fmt.Errorf("required Flux API %s is not served", required.GroupVersion())
		}
	}
	return &Client{client: client, source: source}, nil
}

func serves(resources *metav1.APIResourceList, resource string) bool {
	if resources == nil {
		return false
	}
	for _, item := range resources.APIResources {
		if item.Name == resource {
			return true
		}
	}
	return false
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (c *Client) get(
	ctx context.Context,
	resource schema.GroupVersionResource,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	return c.client.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) list(
	ctx context.Context,
	resource schema.GroupVersionResource,
) (*unstructured.UnstructuredList, error) {
	return c.client.Resource(resource).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
}

// sourceRevision returns the commit the source has on disk, which is what every
// Kustomization is applying and so what answers whether a merge has reached the
// cluster - the source's readiness does not change that.
//
// It reports an error only when source-controller has stopped trying. Stalled is
// deleted on every retryable error, so it means a misconfiguration that no
// amount of waiting resolves; waiting anyway spends the settle timeout and then
// hands the repair path a window with no cause.
func sourceRevision(item *sourcev1.GitRepository) (string, error) {
	if stalled, ok := currentCondition(item.Status.Conditions, meta.StalledCondition,
		item.Generation, item.Status.ObservedGeneration); ok && stalled.Status == metav1.ConditionTrue {
		return "", fmt.Errorf("Flux source has stopped retrying: %s", message(stalled))
	}
	if item.Status.Artifact == nil || item.Status.Artifact.Revision == "" {
		return "", nil
	}
	return normalizeRevision(item.Status.Artifact.Revision), nil
}

func decodeGitRepository(item *unstructured.Unstructured) (*sourcev1.GitRepository, error) {
	result := new(sourcev1.GitRepository)
	return result, runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, result)
}

func decodeKustomization(item *unstructured.Unstructured) (*kustomizev1.Kustomization, error) {
	result := new(kustomizev1.Kustomization)
	return result, runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, result)
}

func decodeHelmRelease(item *unstructured.Unstructured) (*helmv2.HelmRelease, error) {
	result := new(helmv2.HelmRelease)
	return result, runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, result)
}

func normalizeRevision(value string) string {
	if i := strings.LastIndex(value, "sha1:"); i >= 0 {
		value = value[i+len("sha1:"):]
	}
	if len(value) != 40 {
		return value
	}
	for _, r := range value {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f' || 'A' <= r && r <= 'F') {
			return value
		}
	}
	return strings.ToLower(value)
}
