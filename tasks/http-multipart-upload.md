# Task: Add multipart/form-data file upload support to http_request tool

## Context
The `http_request` tool (in `tools/http.go`) currently only supports string bodies. Agents need to upload files via multipart/form-data (e.g., Telegram Bot API sendDocument, sendVideo, etc.). Fotini specifically needs this.

## Current State
- `body` parameter is a string — used as-is with `strings.NewReader`
- No support for file attachments or multipart encoding
- Tool definition is in `tools/http.go`, function `executeHTTPRequest`

## Requirements

### New parameter: `files`
Add a `files` parameter to the tool schema — an array of file attachment objects:

```json
"files": {
  "type": "array",
  "description": "File attachments for multipart/form-data upload. When files are present, the request is sent as multipart/form-data. Other form fields can be sent via the 'form_fields' parameter.",
  "items": {
    "type": "object",
    "properties": {
      "field_name": {
        "type": "string",
        "description": "Form field name (e.g. 'document', 'photo', 'file')"
      },
      "file_path": {
        "type": "string",
        "description": "Path to the file to upload"
      },
      "filename": {
        "type": "string",
        "description": "Override filename sent in the multipart header (optional, defaults to basename of file_path)"
      }
    },
    "required": ["field_name", "file_path"]
  }
}
```

### New parameter: `form_fields`
Add a `form_fields` parameter for non-file multipart fields:

```json
"form_fields": {
  "type": "object",
  "description": "Additional form fields for multipart/form-data requests. Values support {{secret:NAME}} templates.",
  "additionalProperties": { "type": "string" }
}
```

### Behavior
1. When `files` is non-empty, build a `multipart/form-data` request body using `mime/multipart`
2. Write all `form_fields` as text form fields (resolve secrets in values)
3. Write all `files` as file parts — read from disk, stream into the multipart writer
4. Set Content-Type to the multipart writer's `FormDataContentType()`
5. `body` and `files` are mutually exclusive — return error if both provided
6. File paths must be validated — exist, readable, reasonable size (cap at 50MB to prevent accidents)
7. Secret resolution should work in `form_fields` values (same as existing `body` resolution)

### Implementation Notes
- Use `mime/multipart.NewWriter` with a `bytes.Buffer` or `io.Pipe` for the body
- `bytes.Buffer` is simpler and fine for files up to 50MB
- Set the Content-Type header from `writer.FormDataContentType()` — don't let the agent override it when files are present
- The existing `bodyReader` path stays unchanged for non-multipart requests
- File path security: files must be under allowed paths (agent workspace, /tmp, shared dirs). Don't allow reading arbitrary system files. Use the same path validation as other file-reading tools if one exists, or at minimum block paths outside home dirs.

### Tests
- Multipart request with one file
- Multipart request with file + form fields
- Multipart request with multiple files  
- Error: body + files both set
- Error: file doesn't exist
- Error: file too large
- Secret resolution in form_fields values
- Filename override vs default basename

### Docs
- Update SPEC.md with the new parameters
- Update any tool documentation that references http_request

## File to modify
- `tools/http.go` — main implementation
- `tools/http_test.go` — tests (create if doesn't exist, or add to existing)
- `SPEC.md` — documentation
- `docs/CONFIG.md` — if relevant
