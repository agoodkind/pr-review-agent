package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"goodkind.io/gklog"

	"goodkind.io/pr-review-agent/internal/diff"
)

const (
	maximumInspectionBodyBytes = 1024 * 1024
	maximumInspectionRedirects = 3
	inspectionHTTPTimeout      = 20 * time.Second
	inspectionIdleTimeout      = 90 * time.Second
	inspectionIdleConnections  = 100
)

var publicInspectionTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           dialPublicInspection,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          inspectionIdleConnections,
	IdleConnTimeout:       inspectionIdleTimeout,
	TLSHandshakeTimeout:   inspectionHTTPTimeout,
	ResponseHeaderTimeout: inspectionHTTPTimeout,
}

func newPublicInspectionClient() *http.Client {
	return &http.Client{
		Transport: publicInspectionTransport,
		Timeout:   inspectionHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func validatePublicInspectionURL(address *url.URL) error {
	if address.Scheme != "https" || address.Hostname() == "" || address.User != nil ||
		address.Opaque != "" {
		return errors.New("inspection accepts public HTTPS URLs without user credentials")
	}

	host := address.Hostname()
	if strings.Contains(host, "%") || strings.EqualFold(host, "localhost") ||
		strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("inspection cannot read a private network URL")
	}

	if parsed, err := netip.ParseAddr(host); err == nil && !publicInspectionAddress(parsed) {
		return errors.New("inspection cannot read a private network URL")
	}

	return nil
}

func publicInspectionAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() {
		return false
	}

	shared := netip.MustParsePrefix("100.64.0.0/10")

	return !shared.Contains(address)
}

func dialPublicInspection(ctx context.Context, network, target string) (net.Conn, error) {
	logger := gklog.L(ctx)

	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, errors.New("inspection could not parse the source network address")
	}

	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		logger.WarnContext(ctx, "Resolve public inspection source failed", "err", err)
		return nil, fmt.Errorf("resolve public source: %w", err)
	}

	if len(addresses) == 0 {
		return nil, errors.New("the public source did not resolve to an address")
	}

	for _, address := range addresses {
		if !publicInspectionAddress(address) {
			return nil, errors.New("the public source resolved to a private network address")
		}
	}

	var (
		connectionError error
		dialer          net.Dialer
	)

	for _, address := range addresses {
		connection, err := dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(address.String(), port),
		)
		if err == nil {
			return connection, nil
		}

		connectionError = err
	}

	logger.WarnContext(ctx, "Connect to public inspection source failed", "err", connectionError)

	return nil, fmt.Errorf("connect to public inspection source: %w", connectionError)
}

func (inspector *repositoryInspector) readPublicURL(
	ctx context.Context,
	address *url.URL,
	requested InspectionRequest,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectionHTTPTimeout)
	defer cancel()

	response, err := inspector.publicResponse(ctx, address)
	if err != nil {
		return "", err
	}

	defer func() { _ = response.Body.Close() }()

	content, contentType, truncated, err := readPublicInspectionBody(response)
	if err != nil {
		return "", err
	}

	var file diff.FileContext

	file.Path = strings.TrimPrefix(path.Clean(response.Request.URL.Path), "/")
	file.CurrentContent = string(content)

	file.CoverageComplete = true
	if err := inspector.checkContent(file); err != nil {
		return "", err
	}

	identity := "Public source URL: " + response.Request.URL.String()

	text := string(content)
	if contentType == "text/html" {
		text, err = inspectionHTMLText(text)
		if err != nil {
			return "", err
		}

		identity += fmt.Sprintf(
			"\nResponse SHA-256: %x\nResponse bytes retrieved: %d\nRepresentation: extracted HTML text",
			sha256.Sum256(content),
			len(content),
		)
	}

	start, end, err := inspectionQueryRange(text, requested)
	if err != nil {
		return "", err
	}

	return inspectionExcerpt(identity, text, start, end, inspector.maximumBytes, truncated)
}

func (inspector *repositoryInspector) publicResponse(
	ctx context.Context,
	address *url.URL,
) (*http.Response, error) {
	logger := gklog.L(ctx)

	for attempt := range maximumInspectionRedirects {
		if err := validatePublicInspectionURL(address); err != nil {
			return nil, err
		}

		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			address.String(),
			http.NoBody,
		)
		if err != nil {
			return nil, errors.New("create public source request")
		}

		request.Header.Set("Accept", "text/plain, text/html, application/json")

		response, err := inspector.publicClient.Do(request)
		if err != nil {
			logger.WarnContext(ctx, "Read public inspection source failed", "err", err)
			return nil, fmt.Errorf("read public source: %w", err)
		}

		if !slices.Contains(
			[]int{
				http.StatusMovedPermanently,
				http.StatusFound,
				http.StatusSeeOther,
				http.StatusTemporaryRedirect,
				http.StatusPermanentRedirect,
			},
			response.StatusCode,
		) {
			return response, nil
		}

		location, err := response.Location()
		_ = response.Body.Close()

		if err != nil {
			logger.WarnContext(ctx, "Read public inspection redirect failed", "err", err)
			return nil, fmt.Errorf("read public inspection redirect: %w", err)
		}

		if attempt+1 >= maximumInspectionRedirects {
			return nil, errors.New("public source exceeded the inspection redirect limit")
		}

		address = location
	}

	return nil, errors.New("public source exceeded the inspection redirect limit")
}

func readPublicInspectionBody(response *http.Response) ([]byte, string, bool, error) {
	if response.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("public source returned HTTP %d", response.StatusCode)
	}

	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		slog.Warn("Parse public inspection content type failed", "err", err)
		return nil, "", false, errors.New("the public source did not identify a text content type")
	}

	if !strings.HasPrefix(contentType, "text/") && contentType != "application/json" &&
		contentType != "application/xml" &&
		contentType != "application/javascript" {
		return nil, "", false, errors.New(
			"inspection cannot interpret the public source content type",
		)
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, maximumInspectionBodyBytes+1))
	if err != nil {
		slog.Warn("Read public inspection response body failed", "err", err)
		return nil, "", false, fmt.Errorf("read public source body: %w", err)
	}

	truncated := len(content) > maximumInspectionBodyBytes
	if truncated {
		content = content[:maximumInspectionBodyBytes]
		if index := bytes.LastIndexByte(content, '\n'); index >= 0 {
			content = content[:index+1]
		} else {
			return nil, "", false, errors.New(
				"the public source line exceeds the inspection body limit",
			)
		}
	}

	return content, contentType, truncated, nil
}
