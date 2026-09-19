package gofeed_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/mmcdole/gofeed"
)

// Polling a feed with conditional requests: the first fetch downloads and
// parses the full body, later fetches only revalidate and skip parsing
// entirely while the feed is unchanged.
func ExampleParser_ParseURLConditional() {
	const etag = `"v1"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel>`+
			`<title>Example</title><link>http://example.com</link>`+
			`<description>d</description></channel></rss>`)
	}))
	defer server.Close()

	parser := gofeed.NewParser()

	// State kept between polls by the caller; the parser caches nothing.
	var opts gofeed.ConditionalOptions
	var feed *gofeed.Feed

	for poll := 0; poll < 2; poll++ {
		res, err := parser.ParseURLConditional(server.URL, opts)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		if res.NotModified {
			fmt.Printf("poll %d: unchanged, keep %q\n", poll, feed.Title)
			continue
		}
		feed = res.Feed
		// Store the fresh validators (and res.FinalURL, if the cache is
		// keyed by URL) for the next poll.
		opts = gofeed.ConditionalOptions{
			ETag:         res.ETag,
			LastModified: res.LastModified,
		}
		fmt.Printf("poll %d: fetched %q\n", poll, feed.Title)
	}

	// Output:
	// poll 0: fetched "Example"
	// poll 1: unchanged, keep "Example"
}
