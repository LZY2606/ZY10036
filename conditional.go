package gofeed

import (
	"context"
	"io"
	"net/http"
)

// ConditionalOptions carries the HTTP cache validators from a previous
// response, letting a poller revalidate a feed instead of downloading it in
// full on every tick.
//
// ETag is sent verbatim as If-None-Match and LastModified as
// If-Modified-Since. Pass the raw header values exactly as they were
// received: weak ETags (for example `W/"abc"`) are round-tripped unchanged,
// including the W/ prefix. Empty fields are simply not sent, so a zero
// ConditionalOptions (or a nil *ConditionalOptions) performs an
// unconditional fetch equivalent to ParseURL.
//
// A ConditionalOptions value is only read during a call, never mutated; the
// same value may be shared by concurrent calls. The validators for the next
// poll are reported on ConditionalResult, not written back here.
type ConditionalOptions struct {
	// ETag is the value of the ETag response header from the previous
	// fetch, sent as If-None-Match. Weak validators are sent as-is.
	ETag string
	// LastModified is the value of the Last-Modified response header from
	// the previous fetch, sent as If-Modified-Since.
	LastModified string
}

// ConditionalResult is the outcome of a conditional fetch.
//
// The cache validators ETag and LastModified are copied from the response
// headers so the caller can persist them for the next poll. When the server
// omits a validator on a 304 response, the value sent in the request is
// retained, since it remains the current validator. If a response repeats a
// header, net/http semantics apply: the first value wins.
//
// FinalURL is the URL of the response after any redirects
// (resp.Request.URL). Validators describe the representation served at
// FinalURL, so a cache keyed by URL should use FinalURL as its key; a caller
// that keeps keying by the originally requested URL should be aware that a
// changed redirect target invalidates that association.
type ConditionalResult struct {
	// Feed is the parsed feed. It is nil when Unchanged is true.
	Feed *Feed
	// Unchanged reports that the server answered 304 Not Modified. The
	// (empty) body is not parsed and the previously stored feed remains
	// current.
	Unchanged bool
	// ETag is the ETag validator to send on the next poll.
	ETag string
	// LastModified is the Last-Modified validator to send on the next poll.
	LastModified string
	// FinalURL is the URL that ultimately served the response, after
	// following redirects.
	FinalURL string
}

// ParseURLConditional fetches a feed like ParseURL, adding HTTP conditional
// request support: the validators in opts are sent to the server, and the
// result reports whether the feed changed along with the validators to use
// on the next poll. It applies the same default request timeout as ParseURL;
// use ParseURLConditionalWithContext to control cancellation.
//
// The parser keeps no state between calls: there is no global or per-Parser
// cache, and the returned Feed is not retained anywhere. Persisting
// ConditionalResult.ETag/LastModified (and the Feed itself) between polls is
// the caller's responsibility.
func (f *Parser) ParseURLConditional(feedURL string, opts *ConditionalOptions) (*ConditionalResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	return f.ParseURLConditionalWithContext(feedURL, opts, ctx)
}

// ParseURLConditionalWithContext fetches a feed like
// ParseURLWithContext, adding HTTP conditional request support.
//
// When opts provides an ETag and/or LastModified, they are sent as
// If-None-Match and If-Modified-Since. A 304 Not Modified response is
// reported as ConditionalResult.Unchanged with a nil Feed; the format
// detector and parsers are not invoked. A 2xx response updates the result's
// validators and its body is parsed through the same format-detection path
// as ParseURL. Any other status is an HTTPError, exactly as in
// ParseURLWithContext.
//
// Context cancellation, MaxByteSize, AuthConfig, UserAgent, a custom Client,
// and transparent gzip decompression behave identically to
// ParseURLWithContext. opts is only read, so a single value is safe to share
// across concurrent calls.
func (f *Parser) ParseURLConditionalWithContext(feedURL string, opts *ConditionalOptions, ctx context.Context) (result *ConditionalResult, err error) {
	req, err := f.newRequest(ctx, feedURL)
	if err != nil {
		return nil, err
	}
	if opts != nil {
		if opts.ETag != "" {
			req.Header.Set("If-None-Match", opts.ETag)
		}
		if opts.LastModified != "" {
			req.Header.Set("If-Modified-Since", opts.LastModified)
		}
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
		FinalURL:     resp.Request.URL.String(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	if resp.StatusCode == http.StatusNotModified {
		result.Unchanged = true
		// A 304 may omit validators it would repeat; keep the ones sent.
		if result.ETag == "" && opts != nil {
			result.ETag = opts.ETag
		}
		if result.LastModified == "" && opts != nil {
			result.LastModified = opts.LastModified
		}
		// Drain so the connection can be reused; 304 bodies are empty.
		_, _ = io.Copy(io.Discard, resp.Body)
		return result, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
		}
	}

	feed, err := f.parseBody(resp)
	if err != nil {
		return nil, err
	}
	result.Feed = feed
	return result, nil
}
