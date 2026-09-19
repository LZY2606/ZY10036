package gofeed_test

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mmcdole/gofeed"
	"github.com/stretchr/testify/require"
)

const condTestRSS = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<rss version="2.0"><channel><title>cond</title>` +
	`<link>http://example.com/</link><description>d</description>` +
	`</channel></rss>`

// etagServer serves condTestRSS with a fixed ETag, answering 304 when the
// client revalidates with the matching validator.
func etagServer(t *testing.T, etag string) (*httptest.Server, *condRequestLog) {
	t.Helper()
	log := &condRequestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(condTestRSS))
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

type condRequestLog struct {
	mu              sync.Mutex
	ifNoneMatch     []string
	ifModifiedSince []string
	authorization   []string
}

func (l *condRequestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ifNoneMatch = append(l.ifNoneMatch, r.Header.Get("If-None-Match"))
	l.ifModifiedSince = append(l.ifModifiedSince, r.Header.Get("If-Modified-Since"))
	l.authorization = append(l.authorization, r.Header.Get("Authorization"))
}

func (l *condRequestLog) last() (inm, ims, auth string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.ifNoneMatch)
	if n == 0 {
		return "", "", ""
	}
	return l.ifNoneMatch[n-1], l.ifModifiedSince[n-1], l.authorization[n-1]
}

func TestParseURLConditionalNotModifiedFlow(t *testing.T) {
	srv, log := etagServer(t, `"v1"`)
	fp := gofeed.NewParser()

	first, err := fp.ParseURLConditional(srv.URL, nil)
	require.NoError(t, err)
	require.False(t, first.Unchanged)
	require.NotNil(t, first.Feed)
	require.Equal(t, "cond", first.Feed.Title)
	require.Equal(t, `"v1"`, first.ETag)
	require.Equal(t, srv.URL, first.FinalURL)

	second, err := fp.ParseURLConditional(srv.URL, &gofeed.ConditionalOptions{
		ETag: first.ETag,
	})
	require.NoError(t, err)
	require.True(t, second.Unchanged)
	require.Nil(t, second.Feed)
	require.Equal(t, `"v1"`, second.ETag)

	inm, _, _ := log.last()
	require.Equal(t, `"v1"`, inm)
}

func TestParseURLConditionalETagChanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		if r.Header.Get("If-None-Match") == `"v2"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(condTestRSS))
	}))
	defer srv.Close()

	fp := gofeed.NewParser()
	res, err := fp.ParseURLConditional(srv.URL, &gofeed.ConditionalOptions{ETag: `"v1"`})
	require.NoError(t, err)
	require.False(t, res.Unchanged)
	require.NotNil(t, res.Feed)
	require.Equal(t, `"v2"`, res.ETag)
}

func TestParseURLConditionalLastModifiedOnly(t *testing.T) {
	const lastMod = "Wed, 21 Oct 2015 07:28:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", lastMod)
		if r.Header.Get("If-Modified-Since") == lastMod {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(condTestRSS))
	}))
	defer srv.Close()

	fp := gofeed.NewParser()
	first, err := fp.ParseURLConditional(srv.URL, nil)
	require.NoError(t, err)
	require.Equal(t, lastMod, first.LastModified)
	require.Empty(t, first.ETag)

	second, err := fp.ParseURLConditional(srv.URL, &gofeed.ConditionalOptions{
		LastModified: first.LastModified,
	})
	require.NoError(t, err)
	require.True(t, second.Unchanged)
	require.Nil(t, second.Feed)
	require.Equal(t, lastMod, second.LastModified)
}

func TestParseURLConditionalWeakETagRoundTrip(t *testing.T) {
	srv, log := etagServer(t, `W/"weak-1"`)
	fp := gofeed.NewParser()

	first, err := fp.ParseURLConditional(srv.URL, nil)
	require.NoError(t, err)
	require.Equal(t, `W/"weak-1"`, first.ETag)

	second, err := fp.ParseURLConditional(srv.URL, &gofeed.ConditionalOptions{
		ETag: first.ETag,
	})
	require.NoError(t, err)
	require.True(t, second.Unchanged)

	inm, _, _ := log.last()
	require.Equal(t, `W/"weak-1"`, inm, "weak ETag must round-trip verbatim")
}

func TestParseURLConditionalDuplicateHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("ETag", `"first"`)
		w.Header().Add("ETag", `"second"`)
		w.Write([]byte(condTestRSS))
	}))
	defer srv.Close()

	res, err := gofeed.NewParser().ParseURLConditional(srv.URL, nil)
	require.NoError(t, err)
	// net/http semantics: Header.Get returns the first of repeated headers.
	require.Equal(t, `"first"`, res.ETag)
}

func TestParseURLConditionalRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/feed", http.StatusFound)
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"redir"`)
		if r.Header.Get("If-None-Match") == `"redir"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(condTestRSS))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fp := gofeed.NewParser()
	first, err := fp.ParseURLConditional(srv.URL+"/old", nil)
	require.NoError(t, err)
	require.Equal(t, srv.URL+"/feed", first.FinalURL)
	require.Equal(t, `"redir"`, first.ETag)

	// Validators follow the redirect to the final URL on revalidation.
	second, err := fp.ParseURLConditional(srv.URL+"/old", &gofeed.ConditionalOptions{
		ETag: first.ETag,
	})
	require.NoError(t, err)
	require.True(t, second.Unchanged)
	require.Equal(t, srv.URL+"/feed", second.FinalURL)
}

func TestParseURLConditionalMaxByteSize(t *testing.T) {
	srv, _ := etagServer(t, `"v1"`)
	fp := gofeed.NewParser()
	fp.MaxByteSize = 16

	_, err := fp.ParseURLConditional(srv.URL, nil)
	require.ErrorIs(t, err, gofeed.ErrResponseTooLarge)
}

func TestParseURLConditionalContextCancel(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer srv.Close()
	defer close(unblock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gofeed.NewParser().ParseURLConditionalWithContext(srv.URL, nil, ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

func TestParseURLConditionalHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := gofeed.NewParser().ParseURLConditional(srv.URL, nil)
	require.Error(t, err)
	var httpErr gofeed.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusInternalServerError, httpErr.StatusCode)
}

func TestParseURLConditionalGzip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"gz"`)
		gz := gzip.NewWriter(w)
		gz.Write([]byte(condTestRSS))
		gz.Close()
	}))
	defer srv.Close()

	res, err := gofeed.NewParser().ParseURLConditional(srv.URL, nil)
	require.NoError(t, err)
	require.Equal(t, "cond", res.Feed.Title)
	require.Equal(t, `"gz"`, res.ETag)
}

func TestParseURLConditionalAuth(t *testing.T) {
	srv, log := etagServer(t, `"v1"`)
	fp := gofeed.NewParser()
	fp.AuthConfig = &gofeed.Auth{Username: "u", Password: "p"}

	_, err := fp.ParseURLConditional(srv.URL, &gofeed.ConditionalOptions{ETag: `"stale"`})
	require.NoError(t, err)

	_, _, auth := log.last()
	require.Equal(t, "Basic dTpw", auth)
}

func TestParseURLConditionalConcurrentOptions(t *testing.T) {
	srv, _ := etagServer(t, `"v1"`)
	fp := gofeed.NewParser()
	opts := &gofeed.ConditionalOptions{ETag: `"v1"`}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fp.ParseURLConditional(srv.URL, opts)
			require.NoError(t, err)
			require.True(t, res.Unchanged)
		}()
	}
	wg.Wait()
	require.Equal(t, `"v1"`, opts.ETag, "options must not be mutated")
}
