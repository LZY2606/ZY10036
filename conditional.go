package gofeed

import (
	"context"
	"net/http"
)

// ConditionalOptions carries HTTP cache validators from a previous fetch.
// It is passed by value and never mutated, so a single value is safe to
// share across concurrent ParseURLConditionalWithContext calls.
//
// Both fields hold raw header values and are sent verbatim: ETag becomes
// the If-None-Match header and LastModified becomes If-Modified-Since.
// Weak ETags (e.g. `W/"abc"`) are passed through unchanged; do not strip
// or normalize the "W/" prefix, as servers compare the value byte-for-byte.
// Empty fields omit the corresponding header, so a zero ConditionalOptions
// performs an unconditional fetch.
type ConditionalOptions struct {
	// ETag is the value of a previously received ETag response header,
	// sent as If-None-Match. Strong and weak validators are both
	// supported and forwarded exactly as given.
	ETag string
	// LastModified is the value of a previously received Last-Modified
	// response header, sent as If-Modified-Since.
	LastModified string
}

// ConditionalResult is the outcome of a conditional fetch.
type ConditionalResult struct {
	// Feed is the parsed feed. It is nil when NotModified is true; the
	// result is not cached anywhere by the parser, so retaining it across
	// polls is the caller's responsibility.
	Feed *Feed
	// NotModified reports that the server answered 304 Not Modified. In
	// that case the (empty) body is not parsed and no feed-type detection
	// runs at all.
	NotModified bool
	// ETag is the ETag response header of this fetch, to be stored and
	// passed back as ConditionalOptions.ETag on the next poll. When the
	// response carries no ETag (common on 304), the value sent with the
	// request is echoed back so callers can store the result
	// unconditionally. If the server sends several ETag headers, the
	// first one wins, following net/http's Header.Get semantics.
	ETag string
	// LastModified is the Last-Modified response header of this fetch,
	// with the same fallback and duplicate-header semantics as ETag.
	LastModified string
	// FinalURL is the URL of the response after any redirects
	// (resp.Request.URL). Cache validators are scoped to the resource
	// that emitted them: after a permanent redirect the server issues
	// validators for FinalURL, not for the originally requested URL, so
	// callers keying their cache by URL should re-key to FinalURL (and
	// poll it directly) once it differs from the requested URL.
	FinalURL string
}

// ParseURLConditional fetches feedURL with the cache validators in opts
// and parses the response like ParseURL. It applies the same default
// request timeout as ParseURL; use ParseURLConditionalWithContext to
// control cancellation.
func (f *Parser) ParseURLConditional(feedURL string, opts ConditionalOptions) (*ConditionalResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	return f.ParseURLConditionalWithContext(feedURL, opts, ctx)
}

// ParseURLConditionalWithContext fetches feedURL, sending opts.ETag and
// opts.LastModified as If-None-Match and If-Modified-Since, and parses the
// response into the universal feed type exactly like ParseURLWithContext:
// the same format detection, MaxByteSize limit, AuthConfig, UserAgent,
// custom Client and non-2xx HTTPError handling apply.
//
// When the server answers 304 Not Modified the body is not read or parsed
// and the result reports NotModified with a nil Feed. When the server
// answers 200 the returned ETag/LastModified replace the previous
// validators. The parser keeps no cache of its own: nothing is stored
// between calls and no Feed is retained, so polling loops must thread the
// returned validators (and Feed, if needed) into the next call themselves.
//
// Request could be canceled or timeout via the given context.
func (f *Parser) ParseURLConditionalWithContext(feedURL string, opts ConditionalOptions, ctx context.Context) (result *ConditionalResult, err error) {
	req, err := f.newRequest(ctx, feedURL)
	if err != nil {
		return nil, err
	}
	if opts.ETag != "" {
		req.Header.Set("If-None-Match", opts.ETag)
	}
	if opts.LastModified != "" {
		req.Header.Set("If-Modified-Since", opts.LastModified)
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		defer func() {
			if ce := resp.Body.Close(); ce != nil && err == nil {
				err = ce
			}
		}()
	}

	result = &ConditionalResult{
		NotModified:  resp.StatusCode == http.StatusNotModified,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FinalURL:     resp.Request.URL.String(),
	}
	// A 304 may omit validators it already sent; keep the caller's stored
	// values usable for the next poll instead of blanking them.
	if result.ETag == "" {
		result.ETag = opts.ETag
	}
	if result.LastModified == "" {
		result.LastModified = opts.LastModified
	}

	if result.NotModified {
		return result, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
		}
	}

	result.Feed, err = f.parseResponseBody(resp.Body)
	if err != nil {
		return nil, err
	}
	return result, nil
}
