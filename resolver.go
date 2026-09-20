package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type MediaResolver interface {
	Resolve(
		ctx context.Context,
		track Track,
		req ResolveRequest,
	) (*ResolveResponse, error)
}

type HTTPMediaResolver struct {
	Endpoint string
	Client   *http.Client

	// Maximum number of resolver sessions running at once.
	Concurrency int

	once      sync.Once
	semaphore chan struct{}
}

func (r *HTTPMediaResolver) Resolve(
	ctx context.Context,
	track Track,
	req ResolveRequest,
) (*ResolveResponse, error) {

	if req.SongName == "" {
		return nil, fmt.Errorf("song name is empty")
	}

	if req.Artist == "" {
		return nil, fmt.Errorf("artist is empty")
	}

	if req.URL == "" {
		return nil, fmt.Errorf("source URL is empty")
	}

	if strings.TrimSpace(r.Endpoint) == "" {
		return nil, fmt.Errorf("resolver endpoint is empty")
	}

	if err := r.acquire(ctx); err != nil {
		return nil, err
	}

	defer r.release()

	client, err := r.newSessionClient()
	if err != nil {
		return nil, fmt.Errorf(
			"create resolver session client: %w",
			err,
		)
	}

	if err := refreshSession(ctx, client); err != nil {
		return nil, fmt.Errorf(
			"refresh resolver session: %w",
			err,
		)
	}

	form := url.Values{}

	form.Set("song_name", req.SongName)
	form.Set("artist_name", req.Artist)
	form.Set("url", req.URL)
	form.Set("token", "na")
	form.Set("zip_download", "false")
	form.Set("quality", fmt.Sprintf("%d", req.Quality))

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		r.Endpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create resolver request: %w",
			err,
		)
	}

	httpReq.Header.Set(
		"Accept",
		"application/json, text/javascript, */*; q=0.01",
	)

	httpReq.Header.Set(
		"Accept-Language",
		"en-US,en;q=0.9",
	)

	httpReq.Header.Set(
		"Cache-Control",
		"no-cache",
	)

	httpReq.Header.Set(
		"Content-Type",
		"application/x-www-form-urlencoded; charset=UTF-8",
	)

	httpReq.Header.Set(
		"Origin",
		"https://aaplmusicdownloader.com",
	)

	httpReq.Header.Set(
		"Pragma",
		"no-cache",
	)

	httpReq.Header.Set(
		"Referer",
		"https://aaplmusicdownloader.com/album.php",
	)

	httpReq.Header.Set(
		"User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
			"AppleWebKit/537.36 (KHTML, like Gecko) "+
			"Chrome/153.0.0.0 Safari/537.36",
	)

	httpReq.Header.Set(
		"X-Requested-With",
		"XMLHttpRequest",
	)

	fmt.Printf(
		"[RESOLVER] POST %s\n",
		r.Endpoint,
	)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf(
			"resolver request failed: %w",
			err,
		)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(
		io.LimitReader(
			resp.Body,
			1024*1024,
		),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"read resolver response: %w",
			err,
		)
	}

	fmt.Printf(
		"[RESOLVER] HTTP Status: %s\n",
		resp.Status,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		return nil, fmt.Errorf(
			"resolver HTTP %d: %s; body=%q",
			resp.StatusCode,
			resp.Status,
			string(body),
		)
	}

	var result ResolveResponse

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf(
			"decode resolver response: %w; body=%q",
			err,
			string(body),
		)
	}

	if result.Error != "" {
		return nil, fmt.Errorf(
			"resolver API error: %s",
			result.Error,
		)
	}

	if result.DLink == "" {
		return nil, fmt.Errorf(
			"resolver returned empty download URL; body=%q",
			string(body),
		)
	}

	return &result, nil
}

func (r *HTTPMediaResolver) acquire(
	ctx context.Context,
) error {

	limit := r.Concurrency

	if limit <= 0 {
		limit = 3
	}

	r.once.Do(func() {
		r.semaphore = make(chan struct{}, limit)
	})

	select {

	case r.semaphore <- struct{}{}:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *HTTPMediaResolver) release() {

	select {

	case <-r.semaphore:

	default:
	}
}

func (r *HTTPMediaResolver) newSessionClient() (*http.Client, error) {

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf(
			"create cookie jar: %w",
			err,
		)
	}

	timeout := 10 * time.Minute

	var transport http.RoundTripper

	if r.Client != nil {

		transport = r.Client.Transport

		if r.Client.Timeout > 0 {
			timeout = r.Client.Timeout
		}
	}

	if transport == nil {
		transport = http.DefaultTransport
	}

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   timeout,
	}, nil
}

func refreshSession(
	ctx context.Context,
	client *http.Client,
) error {

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"https://aaplmusicdownloader.com/album.php",
		nil,
	)
	if err != nil {
		return fmt.Errorf(
			"create session request: %w",
			err,
		)
	}

	req.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	)

	req.Header.Set(
		"Accept-Language",
		"en-US,en;q=0.9",
	)

	req.Header.Set(
		"Cache-Control",
		"no-cache",
	)

	req.Header.Set(
		"Pragma",
		"no-cache",
	)

	req.Header.Set(
		"Upgrade-Insecure-Requests",
		"1",
	)

	req.Header.Set(
		"User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
			"AppleWebKit/537.36 (KHTML, like Gecko) "+
			"Chrome/153.0.0.0 Safari/537.36",
	)

	fmt.Println(
		"[RESOLVER] Creating fresh session...",
	)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf(
			"session request failed: %w",
			err,
		)
	}

	defer resp.Body.Close()

	_, _ = io.Copy(
		io.Discard,
		io.LimitReader(
			resp.Body,
			1024*1024,
		),
	)

	fmt.Printf(
		"[RESOLVER] Session HTTP Status: %s\n",
		resp.Status,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 400 {

		return fmt.Errorf(
			"session HTTP %d: %s",
			resp.StatusCode,
			resp.Status,
		)
	}

	return nil
}