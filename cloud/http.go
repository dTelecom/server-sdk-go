package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// APIError is a non-2xx answer from the cloud.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is the cloud's error code, such as insufficient_balance; http_<status> when the body
	// has none.
	Code string
	// RetryAfter is how long to wait before retrying, when the cloud says so (429s and 503s).
	RetryAfter time.Duration
	// ID is the job or request the error belongs to, when the cloud names one.
	ID string
	// Detail is what the speech or AI backend said about the input, with backend_rejected (422).
	// For SSML: code, message and position, e.g.
	// {"code": "UNSUPPORTED_TAG", "message": "Unsupported element: emphasis", "position": 13}.
	Detail map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dtelecom cloud: %d %s", e.Status, e.Code)
}

// IsCode reports whether err is an *APIError with the given code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

type cloudHTTP struct {
	authorization string
	baseURL       string
	client        *http.Client
}

// json sends body as JSON (or as is when it is a *multipartBody) and decodes a 2xx answer into out.
func (h *cloudHTTP) json(ctx context.Context, method, path string, body any, headers map[string]string, out any) error {
	res, err := h.response(ctx, method, path, body, headers)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("dtelecom cloud: read response: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("dtelecom cloud: decode response: %w", err)
	}
	return nil
}

// response sends a request and returns a 2xx response; anything else becomes an *APIError.
// The caller closes the body.
func (h *cloudHTTP) response(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		reader, contentType = b.reader, b.contentType
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("dtelecom cloud: encode request: %w", err)
		}
		reader, contentType = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: %w", err)
	}
	req.Header.Set("Authorization", h.authorization)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		return nil, apiError(res)
	}
	return res, nil
}

func apiError(res *http.Response) *APIError {
	e := &APIError{Status: res.StatusCode, Code: "http_" + strconv.Itoa(res.StatusCode)}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var parsed struct {
		Error  any `json:"error"`
		ID     any `json:"id"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		if code, ok := parsed.Error.(string); ok {
			e.Code = code
		}
		if id, ok := parsed.ID.(string); ok {
			e.ID = id
		}
		if detail, ok := parsed.Detail.(map[string]any); ok {
			e.Detail = detail
		}
	}
	if s, err := strconv.ParseFloat(res.Header.Get("Retry-After"), 64); err == nil && s > 0 && !math.IsInf(s, 0) {
		e.RetryAfter = time.Duration(s * float64(time.Second))
	}
	return e
}

const maxDuration = 24 * time.Hour

// durationMinutes rounds a positive duration up to whole minutes, at most 24 hours.
func durationMinutes(d time.Duration) (int, error) {
	if d <= 0 {
		return 0, fmt.Errorf("dtelecom cloud: invalid duration %s", d)
	}
	minutes := int((d + time.Minute - 1) / time.Minute)
	if time.Duration(minutes)*time.Minute > maxDuration {
		return 0, fmt.Errorf("dtelecom cloud: duration %s is above the 24 hour maximum", d)
	}
	return minutes, nil
}

var (
	fileKeyPattern      = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	inferenceKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// checkFileKey checks an Idempotency-Key for a file job: 1-128 printable ASCII characters.
func checkFileKey(key string) error {
	if !fileKeyPattern.MatchString(key) {
		return errors.New("dtelecom cloud: IdempotencyKey must be 1-128 printable ASCII characters")
	}
	return nil
}

// checkInferenceKey checks an Idempotency-Key for translation, vision and moderation:
// 1-128 letters, digits, underscores or hyphens.
func checkInferenceKey(key string) error {
	if !inferenceKeyPattern.MatchString(key) {
		return errors.New("dtelecom cloud: IdempotencyKey must be 1-128 letters, digits, underscores or hyphens")
	}
	return nil
}

func segment(id string) (string, error) {
	if id == "" {
		return "", errors.New("dtelecom cloud: an id is required")
	}
	return url.PathEscape(id), nil
}
