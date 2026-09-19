package gofeed

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The 304 path must not invoke the format detector at all: there is no body
// to sniff, and running detection on an empty body would only risk a
// spurious ErrFeedTypeNotDetected.
func TestParseURLConditionalSkipsDetectorOn304(t *testing.T) {
	var detectCalls atomic.Int32
	orig := detectFeedType
	detectFeedType = func(r io.Reader) FeedType {
		detectCalls.Add(1)
		return orig(r)
	}
	t.Cleanup(func() { detectFeedType = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(`<rss version="2.0"><channel><title>t</title></channel></rss>`))
	}))
	defer srv.Close()

	fp := NewParser()
	if _, err := fp.ParseURLConditional(srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if got := detectCalls.Load(); got != 1 {
		t.Fatalf("200 response: detector called %d times, want 1", got)
	}

	res, err := fp.ParseURLConditional(srv.URL, &ConditionalOptions{ETag: `"v1"`})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged {
		t.Fatal("expected Unchanged on 304")
	}
	if got := detectCalls.Load(); got != 1 {
		t.Fatalf("304 response: detector called %d times, want still 1", got)
	}
}
