package tools

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
)

// isLoopbackHost reports whether host (a URL hostname, no port) is a loopback
// target: the literal "localhost" or a loopback IP literal (127.0.0.0/8, ::1).
// Cleartext to loopback never crosses the network, so it is exempt from the
// secrets-require-https rule.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

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

	// Collect secret refs from headers separately from body/form_fields
	var headerText strings.Builder
	for _, v := range headers {
		headerText.WriteString(v)
		headerText.WriteByte(' ')
	}
	headerRefs := secrets.FindSecretRefs(headerText.String())

	var bodyText strings.Builder
	bodyText.WriteString(body)
	for _, v := range formFields {
		bodyText.WriteByte(' ')
		bodyText.WriteString(v)
	}
	bodyRefs := secrets.FindSecretRefs(bodyText.String())

	// Enforce allowed_in_body: secrets in body/body_file/form_fields are blocked
	// unless explicitly listed in allowed_in_body for the secret's section.
	for _, name := range bodyRefs {
		if bitwarden.IsBitwardenRef(name) {
			return nil, fmt.Errorf("bitwarden secret %q is not permitted in request body", name)
		}
		if store != nil && !store.IsAllowedInBody(name) {
			parts := strings.SplitN(name, ".", 2)
			return nil, fmt.Errorf("secret %q found in request body but not listed in allowed_in_body for [%s] — add %q to allowed_in_body in secrets.toml to permit body resolution",
				name, parts[0], parts[1])
		}
	}

	// Combine all refs (deduplicating, preserving order)
	secretRefSet := make(map[string]struct{})
	var secretRefs []string
	for _, refs := range [][]string{headerRefs, bodyRefs} {
		for _, r := range refs {
			if _, seen := secretRefSet[r]; !seen {
				secretRefSet[r] = struct{}{}
				secretRefs = append(secretRefs, r)
			}
		}
	}
	hasSecrets := len(secretRefs) > 0

	// Secrets must only ever travel over TLS. Reject a cleartext initial scheme
	// up front — fail closed, because there is no safe way to send a credential
	// over plain http across the network. Loopback is exempt: it never leaves
	// the host, so http to 127.0.0.1/::1/localhost is not a network exposure
	// (and the SSRF dialer blocks remote loopback regardless). The redirect
	// guard (newSafeClient + the secret cross-host guard) blocks https->http
	// downgrades after this point. (P2-2.)
	if hasSecrets {
		parsed, err := url.Parse(reqURL)
		if err != nil {
			return nil, fmt.Errorf("parse request URL: %w", err)
		}
		if !strings.EqualFold(parsed.Scheme, "https") && !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("refusing to send secrets to %q over %q: https required (cleartext credential)", parsed.Hostname(), parsed.Scheme)
		}
	}

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
		http_requestLog.Debugf("session=%s request %s %s secrets=%d (bw=%d)", sessionKey, method, parsed.Hostname(), len(secretRefs), len(bwRefs))
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

	// Log secret suffixes for debugging (only when debug.log_api_key_suffix is enabled).
	if log.DebugLogKeySuffix {
		for _, name := range regularRefs {
			if store != nil {
				if val, _ := store.Get(name); len(val) >= 4 {
					http_requestLog.Debugf("secret %q suffix: ...%s", name, val[len(val)-4:])
				}
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
