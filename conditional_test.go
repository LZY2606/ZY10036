package gofeed_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/require"
)

const conditionalTestFeed = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<rss version="2.0"><channel><title>Conditional</title>` +
	`<link>http://example.com</link><description>d</description>` +
	`</channel></rss>`

// A 200 fetch returns the parsed feed plus the response validators, and a
// follow-up request echoing those validators gets a 304 that is reported
// as unchanged without touching the body.
func TestParseURLConditional_NotModifiedAfter200(t *testing.T) {
	const etag = `"v1"`
	const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
	var sawConditionalHeaders bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag && r.Header.Get("If-Modified-Since") == lastModified {
			sawConditionalHeaders = true
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			// Junk that is not a feed in any format: if the 304 path tried
			// to detect or parse the body, the call would fail.
			fmt.Fprint(w, "this is not a feed")
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastModified)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()

	first, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.False(t, first.NotModified)
	require.NotNil(t, first.Feed)
	require.Equal(t, "Conditional", first.Feed.Title)
	require.Equal(t, etag, first.ETag)
	require.Equal(t, lastModified, first.LastModified)
	require.Equal(t, server.URL, first.FinalURL)

	second, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{
		ETag:         first.ETag,
		LastModified: first.LastModified,
	})
	require.NoError(t, err)
	require.True(t, sawConditionalHeaders)
	require.True(t, second.NotModified)
	require.Nil(t, second.Feed)
	// Validators survive the 304 round trip for the next poll.
	require.Equal(t, etag, second.ETag)
	require.Equal(t, lastModified, second.LastModified)
}

// A 304 whose body would fail feed-type detection proves the unchanged
// path never invokes the format detector or a format parser.
func TestParseURLConditional_NotModifiedSkipsDetector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
		fmt.Fprint(w, "<html><body>definitely not a feed</body></html>")
	}))
	defer server.Close()

	res, err := gofeed.NewParser().ParseURLConditional(server.URL, gofeed.ConditionalOptions{ETag: `"x"`})
	require.NoError(t, err)
	require.True(t, res.NotModified)
	require.Nil(t, res.Feed)
}

// When the validator changes, the server answers 200 with a new ETag and
// the result carries the updated metadata.
func TestParseURLConditional_ETagChanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, `"stale"`, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"fresh"`)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	res, err := gofeed.NewParser().ParseURLConditional(server.URL, gofeed.ConditionalOptions{ETag: `"stale"`})
	require.NoError(t, err)
	require.False(t, res.NotModified)
	require.NotNil(t, res.Feed)
	require.Equal(t, `"fresh"`, res.ETag)
}

// A server that only honors If-Modified-Since still yields a working
// conditional cycle without any ETag.
func TestParseURLConditional_LastModifiedOnly(t *testing.T) {
	const lastModified = "Tue, 15 Nov 1994 12:45:26 GMT"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	first, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.Equal(t, lastModified, first.LastModified)
	require.Empty(t, first.ETag)

	second, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{LastModified: first.LastModified})
	require.NoError(t, err)
	require.True(t, second.NotModified)
	require.Equal(t, lastModified, second.LastModified)
}

// Weak ETags are forwarded byte-for-byte, including the W/ prefix.
func TestParseURLConditional_WeakETagRoundTrip(t *testing.T) {
	const weak = `W/"weak-1"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == weak {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", weak)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	first, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.Equal(t, weak, first.ETag)

	second, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{ETag: first.ETag})
	require.NoError(t, err)
	require.True(t, second.NotModified)
	require.Equal(t, weak, second.ETag)
}

// Conditional headers follow the redirect, the feed parses from the final
// response, and FinalURL identifies the resource the validators belong to.
func TestParseURLConditional_Redirect(t *testing.T) {
	const etag = `"redir"`
	var finalSawIfNoneMatch string
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		finalSawIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, conditionalTestFeed)
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/feed", http.StatusMovedPermanently)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	res, err := gofeed.NewParser().ParseURLConditional(server.URL+"/old", gofeed.ConditionalOptions{ETag: etag})
	require.NoError(t, err)
	require.Equal(t, etag, finalSawIfNoneMatch)
	require.NotNil(t, res.Feed)
	require.Equal(t, server.URL+"/feed", res.FinalURL)
	require.Equal(t, etag, res.ETag)
}

// MaxByteSize applies to the conditional path exactly as to ParseURL.
func TestParseURLConditional_ResponseTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	parser.MaxByteSize = 16
	_, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.ErrorIs(t, err, gofeed.ErrResponseTooLarge)
}

// Canceling the context aborts the in-flight conditional request.
func TestParseURLConditional_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := gofeed.NewParser().ParseURLConditionalWithContext(server.URL, gofeed.ConditionalOptions{}, ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}

// Non-2xx responses other than 304 surface as HTTPError, like ParseURL.
func TestParseURLConditional_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := gofeed.NewParser().ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.Error(t, err)
	var httpErr gofeed.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusInternalServerError, httpErr.StatusCode)
}

// Gzip-compressed 200 responses are transparently decoded and parsed.
func TestParseURLConditional_Gzip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.Header.Get("Accept-Encoding"), "gzip")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"gz"`)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, err := gz.Write([]byte(conditionalTestFeed))
		require.NoError(t, err)
		require.NoError(t, gz.Close())
		w.Write(buf.Bytes())
	}))
	defer server.Close()

	res, err := gofeed.NewParser().ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.Equal(t, "Conditional", res.Feed.Title)
	require.Equal(t, `"gz"`, res.ETag)
}

// AuthConfig and UserAgent are applied to conditional requests too.
func TestParseURLConditional_AuthAndUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "u", user)
		require.Equal(t, "p", pass)
		require.Equal(t, "ConditionalTest/1.0", r.Header.Get("User-Agent"))
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	parser.UserAgent = "ConditionalTest/1.0"
	parser.AuthConfig = &gofeed.Auth{Username: "u", Password: "p"}
	res, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{ETag: `"a"`})
	require.NoError(t, err)
	require.NotNil(t, res.Feed)
}

// A custom http.Client on the Parser is used for conditional fetches.
func TestParseURLConditional_CustomClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Via", "custom-transport")
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	parser.Client = server.Client()
	res, err := parser.ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Feed)
}

// One ConditionalOptions value (and one Parser) may be shared by
// concurrent fetches; run with -race to prove it.
func TestParseURLConditional_ConcurrentSharedOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"shared"`)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	parser := gofeed.NewParser()
	opts := gofeed.ConditionalOptions{ETag: `"shared"`, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := parser.ParseURLConditional(server.URL, opts)
			if err != nil {
				errs <- err
				return
			}
			if res.Feed == nil || res.ETag != `"shared"` {
				errs <- fmt.Errorf("unexpected result: %+v", res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// Duplicate response headers follow net/http semantics: Header.Get returns
// the first value.
func TestParseURLConditional_DuplicateETagHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("ETag", `"first"`)
		w.Header().Add("ETag", `"second"`)
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	res, err := gofeed.NewParser().ParseURLConditional(server.URL, gofeed.ConditionalOptions{})
	require.NoError(t, err)
	require.Equal(t, `"first"`, res.ETag)
}

// ParseURL itself is untouched by the conditional additions: it still
// sends no conditional headers and returns a bare feed.
func TestParseURLConditional_ParseURLUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("If-None-Match"))
		require.Empty(t, r.Header.Get("If-Modified-Since"))
		fmt.Fprint(w, conditionalTestFeed)
	}))
	defer server.Close()

	feed, err := gofeed.NewParser().ParseURL(server.URL)
	require.NoError(t, err)
	require.Equal(t, "Conditional", feed.Title)
}
