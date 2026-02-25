# Edit tool: syntax validation after edits

## Concept
When the edit tool modifies a file with checkable syntax, validate before and after the edit. Prevents edits from introducing syntax errors.

## Logic
1. **Before edit:** check syntax of the file
   - If already invalid: allow the edit to proceed, but include syntax warnings in the tool result
   - If valid: proceed to step 2
2. **Apply edit** (in memory, not yet persisted)
3. **After edit:** check syntax again
   - If still valid: persist the edit, return success
   - If now invalid: **reject the edit** (don't write to disk), return error with syntax checker output
   - This prevents a good file from being broken by an edit

## Validators (registry-based)
Map file extension → validator function. Validators should be:
- Built into Go's standard library or trivially importable
- Fast (no external process spawning ideally, but acceptable for some)

### Must have:
- `.json` — `json.Valid()` or `json.Unmarshal` (stdlib)
- `.toml` — `toml.Decode` (we already have BurntSushi/toml)
- `.yaml` / `.yml` — parse with a YAML library (we may need to add one, or use `gopkg.in/yaml.v3`)
- `.go` — `go/parser.ParseFile` (stdlib) for syntax, optionally `go/format` to check if it formats cleanly

### Nice to have:
- `.md` — probably skip, markdown is too permissive
- `.html` — `html.Parse` from `golang.org/x/net/html` (forgiving parser, limited value)
- `.sh` / `.bash` — `bash -n` syntax check (requires exec, but very useful)
- `.xml` — `xml.Decoder` (stdlib)
- `.css` — skip (no good Go parser)
- `.py` — `python3 -c "import ast; ast.parse(open('file').read())"` (requires exec)

### Implementation
```go
type SyntaxChecker func(content []byte) error

var checkers = map[string]SyntaxChecker{
    ".json": checkJSON,
    ".toml": checkTOML,
    ".yaml": checkYAML,
    ".yml":  checkYAML,
    ".go":   checkGo,
}
```

The edit tool looks up the checker by extension. If no checker exists, edit proceeds as normal (no validation).

## Error messages
- Pre-edit invalid: "⚠️ File already has syntax errors: {details}. Edit applied anyway."
- Post-edit invalid: "❌ Edit rejected — would introduce syntax error: {details}. File unchanged."
- Success: normal edit response (no extra noise)

## Tests
- Test each validator with valid + invalid content
- Test the before/after logic: valid→valid (pass), valid→invalid (reject), invalid→valid (pass with warning), invalid→invalid (pass with warning)

## Docs
- Update SPEC.md tool description for `edit`
