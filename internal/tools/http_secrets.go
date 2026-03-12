package tools

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
)

// secretResolution holds the result of secret validation and resolution.
type secretResolution struct {
	headers    map[string]string
	body       string
	formFields map[string]string
	hasSecrets bool
}

// validateAndResolveSecrets scans headers, body, and form fields for secret references,
// validates them against allowed hosts, and resolves the templates.
// It also reads body_file contents if specified.
func validateAndResolveSecrets(
	sessionKey string,
	reqURL, method, body, bodyFile string,
	headers, formFields map[string]string,
	files []fileAttachment,
	maxUploadFileSize int64,
	store *secrets.Store,
	bwStore *bitwarden.Store,
) (*secretResolution, error) {
	// Mutual exclusivity: body, body_file, files
	bodySourceCount := 0
	if body != "" {
		bodySourceCount++
	}
	if bodyFile != "" {
		bodySourceCount++
	}
	if len(files) > 0 {
		bodySourceCount++
	}
	if bodySourceCount > 1 {
		return nil, fmt.Errorf("body, body_file, and files are mutually exclusive")
	}
	if len(formFields) > 0 && len(files) == 0 {
		return nil, fmt.Errorf("form_fields requires files")
	}

	// Read body_file contents early so secrets can be scanned
	if bodyFile != "" {
		info, err := os.Stat(bodyFile)
		if err != nil {
			return nil, fmt.Errorf("body_file %q: %w", bodyFile, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("body_file %q is a directory", bodyFile)
		}
		if info.Size() > maxUploadFileSize {
			return nil, fmt.Errorf("body_file %q is %d bytes, exceeds %dMB limit", bodyFile, info.Size(), maxUploadFileSize/(1024*1024))
		}
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return nil, fmt.Errorf("read body_file %q: %w", bodyFile, err)
		}
		body = string(data)
	}

	// Collect all secret refs from headers, body, and form_fields
	var allText strings.Builder
	for _, v := range headers {
		allText.WriteString(v)
		allText.WriteByte(' ')
	}
	allText.WriteString(body)
	for _, v := range formFields {
		allText.WriteByte(' ')
		allText.WriteString(v)
	}

	secretRefs := secrets.FindSecretRefs(allText.String())
	hasSecrets := len(secretRefs) > 0

	// Split refs into regular (custom.key) and bitwarden (bw.UUID) groups
	var regularRefs, bwRefs []string
	for _, name := range secretRefs {
		if bitwarden.IsBitwardenRef(name) {
			bwRefs = append(bwRefs, name)
		} else {
			regularRefs = append(regularRefs, name)
		}
	}
	hasBWSecrets := len(bwRefs) > 0

	if parsed, err := url.Parse(reqURL); err == nil {
		log.Debugf("http_request", "session=%s request %s %s secrets=%d (bw=%d)", sessionKey, method, parsed.Hostname(), len(secretRefs), len(bwRefs))
	}

	// Validate regular secrets against allowed_hosts
	if len(regularRefs) > 0 {
		if store == nil {
			return nil, fmt.Errorf("secrets referenced but no secret store configured")
		}
		for _, name := range regularRefs {
			if err := store.CheckHostAllowed(name, reqURL); err != nil {
				return nil, fmt.Errorf("secret host check: %w", err)
			}
		}
	}

	// Validate bitwarden secrets against vault item URIs
	if hasBWSecrets {
		if bwStore == nil {
			return nil, fmt.Errorf("bitwarden secrets referenced but bitwarden is not configured")
		}
		for _, name := range bwRefs {
			id := bitwarden.ExtractID(name)
			if err := bwStore.CheckHostAllowed(id, reqURL); err != nil {
				return nil, fmt.Errorf("bitwarden host check: %w", err)
			}
		}
	}

	// resolveValue resolves secret templates in a string using both stores.
	resolveValue := func(v, label string) (string, error) {
		if store != nil && len(regularRefs) > 0 {
			resolved, err := store.Resolve(v)
			if err != nil {
				return "", fmt.Errorf("resolve %s: %w", label, err)
			}
			v = resolved
		}
		if bwStore != nil && hasBWSecrets {
			resolved, err := bwStore.Resolve(v)
			if err != nil {
				return "", fmt.Errorf("resolve bw %s: %w", label, err)
			}
			v = resolved
		}
		return v, nil
	}

	// Resolve secret templates in headers
	resolvedHeaders := make(map[string]string, len(headers))
	for k, v := range headers {
		resolved, err := resolveValue(v, fmt.Sprintf("header %q", k))
		if err != nil {
			return nil, err
		}
		resolvedHeaders[k] = resolved
	}

	// Resolve body
	if hasSecrets && body != "" {
		resolved, err := resolveValue(body, "body")
		if err != nil {
			return nil, err
		}
		body = resolved
	}

	// Resolve form fields
	resolvedFormFields := make(map[string]string, len(formFields))
	for k, v := range formFields {
		resolved, err := resolveValue(v, fmt.Sprintf("form_field %q", k))
		if err != nil {
			return nil, err
		}
		resolvedFormFields[k] = resolved
	}

	return &secretResolution{
		headers:    resolvedHeaders,
		body:       body,
		formFields: resolvedFormFields,
		hasSecrets: hasSecrets,
	}, nil
}
