package gofeed_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/mmcdole/gofeed"
)

// A polling loop that revalidates a feed with the validators from the
// previous response instead of downloading it in full every tick. The caller
// owns the cache: persist opts (ETag/LastModified) and the last feed between
// polls, keyed by the result's FinalURL.
func ExampleParser_ParseURLConditional() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel>`+
			`<title>Example Feed</title><link>http://example.com/</link>`+
			`<description>d</description></channel></rss>`)
	}))
	defer srv.Close()

	fp := gofeed.NewParser()
	var opts *gofeed.ConditionalOptions // nil: first poll is unconditional
	var cached *gofeed.Feed

	for poll := 0; poll < 2; poll++ {
		res, err := fp.ParseURLConditional(srv.URL, opts)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		if res.Unchanged {
			fmt.Println("unchanged, keep cached:", cached.Title)
			continue
		}
		cached = res.Feed
		opts = &gofeed.ConditionalOptions{
			ETag:         res.ETag,
			LastModified: res.LastModified,
		}
		fmt.Println("updated:", cached.Title)
	}

	// Output:
	// updated: Example Feed
	// unchanged, keep cached: Example Feed
}
