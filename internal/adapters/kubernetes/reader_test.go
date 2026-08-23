package kubernetes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"k8s.io/client-go/kubernetes/fake"
)

// chunkedReader hands out size bytes per Read until total is exhausted, which
// is how a real log stream arrives: in chunks that do not divide the bound.
type chunkedReader struct {
	size, remaining int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(r.size, len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

// emptyReader conforms to io.Reader while never making progress, the shape the
// contract explicitly permits and a hand-rolled loop can spin on forever.
type emptyReader struct {
	reads, guard int
}

func (r *emptyReader) Read([]byte) (int, error) {
	r.reads++
	if r.reads >= r.guard {
		return 0, fmt.Errorf("guard tripped after %d empty reads", r.reads)
	}
	return 0, nil
}

// A log the adapter cut has to say so. The 64 KiB tool-level truncate that used
// to announce it cannot fire below a 16 KiB bound, so without a marker here the
// model reads a cut log as the whole log.
func TestALogTailIsBoundedNoMatterHowTheStreamChunksItAndSaysWhenItWasCut(t *testing.T) {
	tests := []struct {
		name       string
		size       int
		total      int
		wantBytes  int
		wantMarker bool
	}{
		{name: "chunks that divide the bound", size: 4096, total: 1 << 20, wantBytes: maxLogBytes, wantMarker: true},
		{name: "chunks that do not divide the bound", size: 4000, total: 1 << 20, wantBytes: maxLogBytes, wantMarker: true},
		{name: "small chunks", size: 1500, total: 1 << 20, wantBytes: maxLogBytes, wantMarker: true},
		{name: "byte at a time", size: 1, total: 1 << 20, wantBytes: maxLogBytes, wantMarker: true},
		{name: "a stream that ends inside the bound", size: 4000, total: 100, wantBytes: 100},
		{name: "an empty stream", size: 4000, total: 0, wantBytes: 0},
		{name: "a stream that ends exactly on the bound", size: 4000, total: maxLogBytes, wantBytes: maxLogBytes, wantMarker: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tail := readLogTail(&chunkedReader{size: test.size, remaining: test.total})
			collected, marked := strings.CutSuffix(tail, "\n... [truncated]")
			if marked != test.wantMarker {
				t.Errorf("truncation marker present=%v, want %v", marked, test.wantMarker)
			}
			if len(collected) != test.wantBytes {
				t.Fatalf("readLogTail collected %d bytes, want %d", len(collected), test.wantBytes)
			}
			if strings.Trim(collected, "x") != "" {
				t.Errorf("readLogTail returned bytes the stream never sent")
			}
		})
	}
}

// A truncated log tail is fed verbatim to the model; a cut landing inside a
// multi-byte rune would hand it a broken UTF-8 sequence at the tip.
func TestALogTailCutInsideARuneIsTrimmedToAValidBoundary(t *testing.T) {
	got := readLogTail(strings.NewReader(strings.Repeat("世", 8000)))
	collected, marked := strings.CutSuffix(got, truncationMarker)
	if !marked {
		t.Fatalf("a stream past the bound must carry the truncation marker")
	}
	if !utf8.ValidString(collected) {
		t.Errorf("truncated tail is not valid UTF-8; a rune was split at the bound")
	}
	if len(collected) != maxLogBytes-1 {
		t.Errorf("trimmed %d bytes, want exactly the one partial rune byte", maxLogBytes-len(collected))
	}
}

// The trim touches only the partial rune the boundary created; invalid bytes the
// stream genuinely sent earlier are evidence and stay in place.
func TestALogTailTrimLeavesEarlierInvalidBytesInPlace(t *testing.T) {
	got := readLogTail(strings.NewReader("\xff\xff" + strings.Repeat("世", 8000)))
	collected, _ := strings.CutSuffix(got, truncationMarker)
	if !strings.HasPrefix(collected, "\xff\xff") {
		t.Errorf("the leading invalid bytes were dropped, not just the tail rune")
	}
	if r, _ := utf8.DecodeLastRuneInString(collected); r == utf8.RuneError {
		t.Errorf("the tip is still a broken rune after trimming")
	}
}

func TestAnASCIILogTailIsTruncatedButNeverTrimmed(t *testing.T) {
	got := readLogTail(&chunkedReader{size: 4000, remaining: 1 << 20})
	collected, marked := strings.CutSuffix(got, truncationMarker)
	if !marked || len(collected) != maxLogBytes {
		t.Errorf("ASCII tail: got %d bytes marked=%v, want %d marked", len(collected), marked, maxLogBytes)
	}
}

// dyingReader delivers its payload and the failure in the same call, the shape
// io.Reader allows and a stream cut mid-flight actually takes.
type dyingReader struct {
	payload string
	spent   bool
}

func (r *dyingReader) Read(p []byte) (int, error) {
	if r.spent {
		return 0, fmt.Errorf("stream is gone")
	}
	r.spent = true
	return copy(p, r.payload), fmt.Errorf("stream died")
}

// The bytes a failing read already produced are the evidence the read was made
// for, so they survive the error that arrived with them.
func TestALogTailKeepsWhatAFailedReadAlreadyProduced(t *testing.T) {
	if tail := readLogTail(&dyingReader{payload: "OOMKilled: container exceeded memory"}); tail != "OOMKilled: container exceeded memory" {
		t.Fatalf("readLogTail returned %q, want the bytes the failing read carried", tail)
	}
}

func TestPodLogsReturnsWhatTheStreamProduced(t *testing.T) {
	c, err := New(fake.NewClientset(pod("media", "sonarr-1", true)))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.PodLogs(context.Background(), "media", "sonarr-1", "app", 200)
	if err != nil {
		t.Fatalf("PodLogs: %v", err)
	}
	if got == "" {
		t.Fatal("PodLogs returned nothing from a stream that had content")
	}
}

// A log read is evidence collection inside an unattended watch: a loop that
// never returns hangs the run with the merge still deployed and nothing to
// report, which is worse than a short log.
func TestALogStreamThatNeverMakesProgressDoesNotSpin(t *testing.T) {
	stream := &emptyReader{guard: 4096}
	if tail := readLogTail(stream); tail != "" {
		t.Errorf("readLogTail returned %q from a stream that sent nothing", tail)
	}
	if stream.reads != 1 {
		t.Fatalf("readLogTail made %d reads against a stream that never advances, want 1", stream.reads)
	}
}
