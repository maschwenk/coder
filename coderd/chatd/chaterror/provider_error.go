package chaterror

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

type providerErrorDetails struct {
	statusCode int
	retryAfter time.Duration
}

func extractProviderErrorDetails(err error) providerErrorDetails {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return providerErrorDetails{}
	}

	return providerErrorDetails{
		statusCode: providerErr.StatusCode,
		retryAfter: retryAfterFromHeaders(providerErr.ResponseHeaders),
	}
}

func retryAfterFromHeaders(headers map[string]string) time.Duration {
	if len(headers) == 0 {
		return 0
	}

	if retryAfter := parseRetryAfterMs(headerValue(headers, "retry-after-ms")); retryAfter > 0 {
		return retryAfter
	}

	return parseRetryAfter(headerValue(headers, "retry-after"))
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseRetryAfterMs(value string) time.Duration {
	if value == "" {
		return 0
	}

	ms, err := strconv.ParseFloat(value, 64)
	if err != nil || ms <= 0 {
		return 0
	}

	return time.Duration(ms * float64(time.Millisecond))
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}

	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}

	retryAfter := time.Until(retryAt)
	if retryAfter <= 0 {
		return 0
	}

	return retryAfter
}
