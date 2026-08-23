package kubernetes

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/retry"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestReadRetryTransportRetriesGETAtMostThreeTimes(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &http.Client{Transport: readRetryTransport{base: http.DefaultTransport, retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}}}
	response, err := client.Get(server.URL)
	if err != nil || hits != 3 || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err=%v hits=%d response=%#v", err, hits, response)
	}
}

func TestReadRetryTransportStopsClientGoRetryAfterThreeTransportFailures(t *testing.T) {
	hits := 0
	client, err := rest.UnversionedRESTClientFor(&rest.Config{Host: "https://cluster.test", ContentConfig: rest.ContentConfig{NegotiatedSerializer: scheme.Codecs.WithoutConversion()}, Transport: readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, &net.OpError{Err: syscall.ECONNRESET}
	}), retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Get().AbsPath("/api").Do(context.Background()).Error()
	if err == nil || hits != 3 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestReadRetryTransportReturnsFinalKubernetesStatusAfterThreeAttempts(t *testing.T) {
	hits := 0
	status := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"apiserver unavailable","reason":"ServiceUnavailable","code":503}`
	client, err := rest.UnversionedRESTClientFor(&rest.Config{Host: "https://cluster.test", ContentConfig: rest.ContentConfig{NegotiatedSerializer: scheme.Codecs.WithoutConversion()}, Transport: readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"10"}, "Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(status))}, nil
	}), retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Get().AbsPath("/api").Do(context.Background()).Error()
	if !apierrors.IsServiceUnavailable(err) || hits != 3 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
	apiStatus, ok := err.(apierrors.APIStatus)
	if !ok || apiStatus.Status().Code != 503 || apiStatus.Status().Reason != metav1.StatusReasonServiceUnavailable || apiStatus.Status().Message != "apiserver unavailable" {
		t.Fatalf("status=%+v", apiStatus.Status())
	}
}

func TestReadRetryTransportWaitsAsLongAsTheAPIServerAsked(t *testing.T) {
	hits := 0
	var waits []time.Duration
	transport := readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"5"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}), retry: retry.Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}}
	response, err := transport.RoundTrip(mustRequest(t, http.MethodGet))
	if err != nil || hits != 3 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
	response.Body.Close()
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s 5s]", waits)
	}
}

func TestOneBlipAskingForLongerThanAPollIsStillRetried(t *testing.T) {
	hits := 0
	var waits []time.Duration
	transport := readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		hits++
		if hits == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}), retry: retry.Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}}
	response, err := transport.RoundTrip(mustRequest(t, http.MethodGet))
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("err=%v response=%#v, want the read to recover", err, response)
	}
	response.Body.Close()
	if hits != 2 || len(waits) != 1 || waits[0] != maxDirectedDelay {
		t.Fatalf("hits=%d waits=%v, want a second attempt after a capped wait", hits, waits)
	}
}

func TestTheDirectedDelayCapHasNoCliffAcrossItsBound(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: "1", want: time.Second},
		{value: "9", want: 9 * time.Second},
		{value: "10", want: maxDirectedDelay},
		{value: "11", want: maxDirectedDelay},
		{value: "30", want: maxDirectedDelay},
		{value: "3600", want: maxDirectedDelay},
	} {
		t.Run(test.value, func(t *testing.T) {
			hits := 0
			var waits []time.Duration
			transport := readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
				hits++
				if hits == 1 {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {test.value}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
			}), retry: retry.Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}}
			response, err := transport.RoundTrip(mustRequest(t, http.MethodGet))
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("err=%v status=%d, want the read to recover", err, response.StatusCode)
			}
			response.Body.Close()
			if hits != 2 || len(waits) != 1 || waits[0] != test.want {
				t.Fatalf("hits=%d waits=%v, want one wait of %v", hits, waits, test.want)
			}
		})
	}
}

func TestReadRetryTransportStripsRetryAfterFromTheResponseItReturns(t *testing.T) {
	transport := readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"600"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}), retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}}
	response, err := transport.RoundTrip(mustRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Retry-After") != "" {
		t.Fatalf("Retry-After survived onto the response client-go sees: %q", response.Header.Get("Retry-After"))
	}
}

func mustRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, "https://cluster.test/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestReadRetryTransportNeverRetriesPATCH(t *testing.T) {
	hits := 0
	transport := readRetryTransport{base: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"10"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}
	request, err := http.NewRequest(http.MethodPatch, "https://cluster.test/api", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || hits != 1 || response.Header.Get("Retry-After") != "" {
		t.Fatalf("err=%v hits=%d response=%#v", err, hits, response)
	}
	response.Body.Close()
}
