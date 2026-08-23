// Package kubernetes reads the cluster state needed to snapshot health and to
// diagnose a failed reconcile.
package kubernetes

import (
	"context"
	"fmt"
	"io"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	maxCollectedItems int64 = 10_000
	maxLogBytes             = 16 << 10
)

type Client struct {
	client   kubernetes.Interface
	degraded func(reason string)
}

type Option func(*Client)

// WithDegradedReadObserver reports the 1+N per-namespace pod-read regime every
// poll it is active, including the steady state where it succeeds - which is the
// only signal that namespace-scoped RBAC has silently made every poll cost one
// list per namespace, permanently, until the RBAC changes.
func WithDegradedReadObserver(observe func(reason string)) Option {
	return func(c *Client) { c.degraded = observe }
}

func New(client kubernetes.Interface, opts ...Option) (*Client, error) {
	if client == nil {
		return nil, fmt.Errorf("Kubernetes client is required")
	}
	c := &Client{client: client}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *Client) ListDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	items, err := c.client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, listOptions())
	if err != nil {
		return nil, err
	}
	return items.Items, complete(items.Continue, len(items.Items), "Deployment")
}

func (c *Client) ListStatefulSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	items, err := c.client.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, listOptions())
	if err != nil {
		return nil, err
	}
	return items.Items, complete(items.Continue, len(items.Items), "StatefulSet")
}

func (c *Client) ListDaemonSets(ctx context.Context) ([]appsv1.DaemonSet, error) {
	items, err := c.client.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, listOptions())
	if err != nil {
		return nil, err
	}
	return items.Items, complete(items.Continue, len(items.Items), "DaemonSet")
}

func (c *Client) ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	items, err := c.client.CoreV1().Pods(namespace).List(ctx, listOptions())
	if err != nil {
		return nil, err
	}
	return items.Items, complete(items.Continue, len(items.Items), "Pod")
}

func (c *Client) ListEvents(ctx context.Context, namespace string) ([]corev1.Event, error) {
	items, err := c.client.CoreV1().Events(namespace).List(ctx, listOptions())
	if err != nil {
		return nil, err
	}
	return items.Items, complete(items.Continue, len(items.Items), "Event")
}

func (c *Client) PodLogs(ctx context.Context, namespace, pod, container string, lines int64) (string, error) {
	options := &corev1.PodLogOptions{Container: container}
	if lines > 0 {
		options.TailLines = &lines
	}
	stream, err := c.client.CoreV1().Pods(namespace).GetLogs(pod, options).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	return readLogTail(stream), nil
}

// readLogTail collects at most maxLogBytes, keeping whatever a failed read had
// already produced rather than discarding the evidence with the error.
func readLogTail(stream io.Reader) string {
	buffer := make([]byte, maxLogBytes)
	filled := 0
	for filled < len(buffer) {
		n, err := stream.Read(buffer[filled:])
		filled += n
		// io.Reader permits (0, nil); looping on it would spin an unattended run forever.
		if err != nil || n == 0 {
			break
		}
	}
	if filled == len(buffer) {
		return string(trimPartialRune(buffer)) + truncationMarker
	}
	return string(buffer[:filled])
}

// trimPartialRune drops a multi-byte rune the byte cap split at the tail, so the
// truncated tip is valid UTF-8; complete bytes, even invalid ones the stream
// really sent, are evidence and stay.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && len(b)-i <= utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}

func desiredReplicas(value *int32) int32 {
	if value == nil {
		return 1
	}
	return *value
}

func listOptions() metav1.ListOptions {
	return metav1.ListOptions{Limit: maxCollectedItems}
}

func complete(next string, count int, kind string) error {
	if next != "" || count > int(maxCollectedItems) {
		return fmt.Errorf("%s collection limit exceeded", kind)
	}
	return nil
}
