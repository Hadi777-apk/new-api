package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
	"golang.org/x/image/riff"
	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
)

// Batch workers spool upstream responses privately so storage recovery can reuse
// an image URL without repeating a billable generation request.
const DrawingResponseSpoolContextKey = "server_drawing_response_spool"

const (
	imageResponseConversionTimeout = 120 * time.Second
	imageDownloadAttemptTimeout    = 60 * time.Second
	imageDownloadMaxAttempts       = 2
	imageDownloadRetryDelay        = 250 * time.Millisecond
)

// Preserve transport causes for retry classification without returning signed
// image URLs or other transport details to callers.
type imageDownloadError struct {
	message string
	cause   error
	status  int
}

func (e *imageDownloadError) Error() string { return e.message }
func (e *imageDownloadError) Unwrap() error { return e.cause }

type imageDownloadAttemptInfo struct {
	bytes         int64
	contentLength int64
	status        int
	phase         string
}

// Pixel decoders may allocate up to 128 MiB for a permitted 16-bit PNG.
// Bound concurrent decoding without limiting upstream generation concurrency.
var imageResponseDecodeSlots = make(chan struct{}, 2)

// EnsureImageBase64Response makes URL-only images usable by Base64-only clients.
// Native Base64 is validated without fetching its URL; valid payloads are kept.
func EnsureImageBase64Response(ctx context.Context, body []byte) ([]byte, error) {
	return ensureImageBase64Response(ctx, body, false)
}

// ValidateImageNativeBase64Response validates native images while preserving
// URL-only responses for private workers to recover without regenerating.
func ValidateImageNativeBase64Response(ctx context.Context, body []byte) ([]byte, error) {
	return ensureImageBase64Response(ctx, body, true)
}

func ensureImageBase64Response(ctx context.Context, body []byte, preserveURLs bool) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("invalid image response")
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return nil, errors.New("upstream returned an invalid image list")
	}
	items := data.Array()
	if len(items) == 0 {
		return nil, errors.New("upstream returned no images")
	}
	if len(items) > dto.MaxImageN {
		return nil, errors.New("upstream returned too many images")
	}
	remaining := MaxDrawingResponseBytes
	needsConversion := false
	normalized := make(map[int]string)
	for index, item := range items {
		if ctx.Err() != nil {
			return nil, errors.New("image delivery cancelled or timed out")
		}
		if !item.IsObject() {
			return nil, errors.New("upstream returned an invalid image item")
		}
		encoded := item.Get("b64_json")
		if encoded.Type == gjson.String && encoded.Str != "" {
			value, err := normalizeImageResponseBase64(encoded.Str)
			if err != nil {
				return nil, fmt.Errorf("image %d: %w", index+1, err)
			}
			remaining -= int64(len(value))
			if remaining < 0 {
				return nil, errors.New("image response exceeds the Base64 size limit")
			}
			decoded, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)), MaxDrawingBytes+1))
			if err != nil {
				return nil, fmt.Errorf("image %d: invalid image Base64", index+1)
			}
			if err := validateImageResponseBytes(ctx, decoded); err != nil {
				return nil, fmt.Errorf("image %d: %w", index+1, err)
			}
			if value != encoded.Str {
				normalized[index] = value
			}
		} else if !preserveURLs {
			needsConversion = true
		}
	}
	if !needsConversion && len(normalized) == 0 {
		return body, nil
	}
	if needsConversion && remaining <= 0 {
		return nil, errors.New("converted image response exceeds the Base64 size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, imageResponseConversionTimeout)
	defer cancel()
	var response map[string]json.RawMessage
	var images []map[string]json.RawMessage
	if common.Unmarshal(body, &response) != nil || common.Unmarshal(response["data"], &images) != nil {
		return nil, errors.New("invalid image response")
	}
	for index, item := range images {
		if value, changed := normalized[index]; changed {
			item["b64_json"], _ = common.Marshal(value)
		}
		var encoded, location string
		_ = common.Unmarshal(item["b64_json"], &encoded)
		if encoded != "" {
			continue
		}
		if preserveURLs {
			continue
		}
		_ = common.Unmarshal(item["url"], &location)
		limit := min(MaxDrawingBytes, remaining/4*3)
		if limit <= 0 {
			return nil, errors.New("converted image response exceeds the Base64 size limit")
		}
		imageBytes, err := downloadImageForBase64(ctx, location, limit)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index+1, err)
		}
		encoded = base64.StdEncoding.EncodeToString(imageBytes)
		remaining -= int64(len(encoded))
		item["b64_json"], err = common.Marshal(encoded)
		if err != nil {
			return nil, errors.New("failed to encode downloaded image")
		}
	}
	var err error
	response["data"], err = common.Marshal(images)
	if err != nil {
		return nil, errors.New("failed to encode image response")
	}
	return common.Marshal(response)
}

func downloadImageForBase64(ctx context.Context, location string, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, errors.New("image download cancelled or timed out")
	}
	if location == "" {
		return nil, errors.New("upstream returned neither Base64 nor an image URL")
	}
	for attempt := 1; attempt <= imageDownloadMaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, imageDownloadAttemptTimeout)
		started := time.Now()
		data, info, err := downloadImageForBase64Attempt(attemptCtx, location, limit)
		cancel()
		if err == nil {
			return data, nil
		}
		reason, retry := imageDownloadFailureReason(err, info.phase)
		if ctx.Err() != nil {
			reason, retry = "cancelled", false
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}
		}
		host := ""
		if u, parseErr := url.Parse(location); parseErr == nil {
			host = u.Hostname()
		}
		logger.LogWarn(ctx, fmt.Sprintf("image download failed host=%q attempt=%d status=%d bytes=%d content_length=%d elapsed_ms=%d reason=%s", host, attempt, info.status, info.bytes, info.contentLength, time.Since(started).Milliseconds(), reason))
		if ctx.Err() != nil {
			return nil, &imageDownloadError{message: "image download cancelled or timed out", cause: ctx.Err()}
		}
		if !retry || attempt == imageDownloadMaxAttempts {
			return nil, err
		}
		// Retry only the already generated image URL, never its paid generation.
		timer := time.NewTimer(imageDownloadRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &imageDownloadError{message: "image download cancelled or timed out", cause: ctx.Err()}
		case <-timer.C:
		}
	}
	return nil, errors.New("image download failed")
}

func downloadImageForBase64Attempt(ctx context.Context, location string, limit int64) ([]byte, imageDownloadAttemptInfo, error) {
	info := imageDownloadAttemptInfo{contentLength: -1, phase: "request_failed"}
	response, err := requestImageResponse(ctx, location, false)
	if err != nil {
		return nil, info, err
	}
	defer response.Body.Close()
	info.status, info.contentLength = response.StatusCode, response.ContentLength
	if response.StatusCode != http.StatusOK {
		return nil, info, &imageDownloadError{message: fmt.Sprintf("image download returned HTTP %d", response.StatusCode), status: response.StatusCode}
	}
	if response.ContentLength > limit {
		info.phase = "size_limit"
		return nil, info, errors.New("image download exceeds the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	info.bytes = int64(len(data))
	if int64(len(data)) > limit {
		info.phase = "size_limit"
		return nil, info, errors.New("image download exceeds the size limit")
	}
	if err != nil {
		info.phase = "read_failed"
		return nil, info, &imageDownloadError{message: "image download was incomplete", cause: err}
	}
	if response.ContentLength >= 0 && int64(len(data)) != response.ContentLength {
		info.phase = "read_failed"
		return nil, info, &imageDownloadError{message: "image download was incomplete", cause: io.ErrUnexpectedEOF}
	}
	if err := validateImageResponseBytes(ctx, data); err != nil {
		info.phase = "invalid_image"
		return nil, info, err
	}
	return data, info, nil
}

func imageDownloadFailureReason(err error, fallback string) (string, bool) {
	if errors.Is(err, context.Canceled) {
		return "cancelled", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "unexpected_eof", true
	}
	// The PNG decoder converts a truncated IDAT stream into a FormatError.
	// Keep corrupt checksums and other permanent format errors non-retryable.
	var pngError png.FormatError
	if errors.As(err, &pngError) && (string(pngError) == "not enough pixel data" || string(pngError) == "unexpected EOF") {
		return "unexpected_eof", true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return "connection_reset", true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "timeout", true
		}
		if networkError.Temporary() {
			return "network_temporary", true
		}
	}
	var downloadError *imageDownloadError
	if errors.As(err, &downloadError) && downloadError.status != 0 {
		switch downloadError.status {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Sprintf("http_%d", downloadError.status), true
		default:
			return fmt.Sprintf("http_%d", downloadError.status), false
		}
	}
	return fallback, false
}

func normalizeImageResponseBase64(encoded string) (string, error) {
	if len(encoded) < 5 || !strings.EqualFold(encoded[:5], "data:") {
		return encoded, nil
	}
	separator := strings.IndexByte(encoded, ',')
	if separator < 0 {
		return "", errors.New("invalid image Base64 data URL")
	}
	header := strings.ToLower(encoded[:separator])
	if !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return "", errors.New("invalid image Base64 data URL")
	}
	return encoded[separator+1:], nil
}

func validateImageResponseBytes(ctx context.Context, data []byte) error {
	if int64(len(data)) > MaxDrawingBytes {
		return errors.New("image exceeds the size limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return &imageDownloadError{message: "upstream content is not a supported image", cause: err}
	}
	// Bound decoded pixel memory before invoking a decoder. This permits 4K
	// square images, including 16-bit PNGs, without allocating for huge headers.
	if config.Width <= 0 || config.Height <= 0 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > 16_777_216 {
		return errors.New("image dimensions exceed the decoding limit")
	}
	if format == "webp" {
		// WebP DecodeConfig can stop at VP8X's canvas header, while Decode
		// allocates using the embedded VP8/VP8L dimensions. Verify both first.
		_, chunks, err := riff.NewReader(bytes.NewReader(data))
		if err != nil {
			return errors.New("invalid WebP image")
		}
		for {
			id, length, chunk, err := chunks.Next()
			if err != nil {
				return errors.New("invalid WebP image")
			}
			var width, height int
			switch string(id[:]) {
			case "VP8 ":
				decoder := vp8.NewDecoder()
				decoder.Init(chunk, int(length))
				header, err := decoder.DecodeFrameHeader()
				if err != nil {
					return errors.New("invalid WebP image")
				}
				width, height = header.Width, header.Height
			case "VP8L":
				header, err := vp8l.DecodeConfig(chunk)
				if err != nil {
					return errors.New("invalid WebP image")
				}
				width, height = header.Width, header.Height
			default:
				continue
			}
			if width != config.Width || height != config.Height {
				return errors.New("WebP frame dimensions do not match its canvas")
			}
			break
		}
	}
	// Decode checks pixel data, rather than accepting a recognizable header.
	// For animated GIFs the standard decoder validates the first image only.
	if ctx.Err() != nil {
		return errors.New("image decoding cancelled or timed out")
	}
	select {
	case imageResponseDecodeSlots <- struct{}{}:
		defer func() { <-imageResponseDecodeSlots }()
	case <-ctx.Done():
		return errors.New("image decoding cancelled or timed out")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return &imageDownloadError{message: "upstream returned an incomplete or invalid image", cause: err}
	}
	return nil
}
