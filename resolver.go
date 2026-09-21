package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type MediaResolver interface {
	Resolve(
		ctx context.Context,
		track Track,
		req ResolveRequest,
	) (*ResolveResponse, error)
}

// HTTPMediaResolver resolves a track to a direct media URL.
//
// Concurrency is owned by the pipeline stage that calls Resolve, not by
// this type. The previous implementation kept its own semaphore on top
// of a worker pool that already bounded it, which was redundant
// synchronisation with no effect on the achieved rate.
//
// Session state is owned by the shared SessionManager, so the resolver,
// the metadata stage and the download all speak to the service as one
// session instead of three unrelated ones.
type HTTPMediaResolver struct {
	Endpoint string
	Session  *SessionManager
	Log      *Logger
}

func (r *HTTPMediaResolver) Resolve(
	ctx context.Context,
	track Track,
	req ResolveRequest,
) (*ResolveResponse, error) {

	// Input problems can never be fixed by trying again.
	if req.SongName == "" {
		return nil, permanent(fmt.Errorf("song name is empty"))
	}

	if req.Artist == "" {
		return nil, permanent(fmt.Errorf("artist is empty"))
	}

	if req.URL == "" {
		return nil, permanent(fmt.Errorf("source URL is empty"))
	}

	if strings.TrimSpace(r.Endpoint) == "" {
		return nil, permanent(fmt.Errorf("resolver endpoint is empty"))
	}

	if r.Session == nil {
		return nil, permanent(fmt.Errorf("resolver session manager is nil"))
	}

	handle, err := r.Session.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("establish resolver session: %w", err)
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
		return nil, permanent(fmt.Errorf(
			"create resolver request: %w",
			err,
		))
	}

	httpReq.Header.Set(
		"Accept",
		"application/json, text/javascript, */*; q=0.01",
	)

	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
	httpReq.Header.Set("Cache-Control", "no-cache")

	httpReq.Header.Set(
		"Content-Type",
		"application/x-www-form-urlencoded; charset=UTF-8",
	)

	httpReq.Header.Set("Origin", "https://aaplmusicdownloader.com")
	httpReq.Header.Set("Pragma", "no-cache")

	httpReq.Header.Set(
		"Referer",
		"https://aaplmusicdownloader.com/album.php",
	)

	httpReq.Header.Set("User-Agent", browserUserAgent)
	httpReq.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := handle.Client().Do(httpReq)
	if err != nil {
		r.Session.Failed(handle)

		return nil, classifyTransportError(
			err,
			"resolver request failed",
		)
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 1024*1024)
	if err != nil {
		return nil, fmt.Errorf(
			"read resolver response: %w",
			err,
		)
	}

	// A transport level 401/403 is the service refusing the session
	// itself. Discard it and let the retry layer come back on a fresh
	// one rather than failing the track.
	if r.Session.CheckResponse(handle, resp, "resolver") {
		return nil, sessionReset(fmt.Errorf(
			"resolver rejected session with HTTP %d: %s",
			resp.StatusCode,
			resp.Status,
		))
	}

	if err := classifyHTTPStatus(resp, body, "resolver"); err != nil {
		return nil, err
	}

	var result ResolveResponse

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, permanent(fmt.Errorf(
			"decode resolver response: %w; body=%q",
			err,
			truncate(strings.TrimSpace(string(body)), 256),
		))
	}

	// An in-band error arrives inside an HTTP 200: the API reporting
	// that its own upstream fetch was refused, typically because the
	// track is not available in that storefront.
	//
	// One such failure says nothing about the session, so it is
	// permanent for this track. A run of them on the same session is
	// a different signal, and Failed decides when that threshold is
	// crossed and the session should be replaced.
	if result.Error != "" {
		err := fmt.Errorf("resolver API error: %s", result.Error)

		if r.Session.Failed(handle) {
			return nil, err
		}

		return nil, permanent(err)
	}

	if result.DLink == "" {
		err := fmt.Errorf(
			"resolver returned empty download URL; status=%q comments=%q",
			result.Status,
			truncate(result.Comments, 128),
		)

		if r.Session.Failed(handle) {
			return nil, err
		}

		return nil, permanent(err)
	}

	r.Session.Succeeded(handle)

	return &result, nil
}
