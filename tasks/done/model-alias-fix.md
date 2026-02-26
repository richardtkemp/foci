# Fix: /model command should resolve short names

## Bug
`/model haiku`, `/model opus`, `/model sonnet` set the literal string as the model, causing 404 API errors. The API needs full model IDs like `claude-haiku-4-5`.

## Fix
In `command/builtins.go` line 563, change:
```go
setModel(args)
```
to:
```go
setModel(resolveModel(args))
```

The `resolveModel` function already exists in `command/agents_new.go:192` and handles opus→claude-opus-4-6, sonnet→claude-sonnet-4-6, haiku→claude-haiku-4-5.

Also update the response message to show the resolved model name:
```go
resolved := resolveModel(args)
setModel(resolved)
return fmt.Sprintf("Model switched to: %s", resolved), nil
```

## Also fix
The `resolveModel` function uses hardcoded model versions (claude-opus-4-6, claude-sonnet-4-6, claude-haiku-4-5). These should probably come from a shared constant or map, but for now the hardcoded values are fine — just make sure they match what Anthropic currently accepts.

## Test
Add a test case to TestModelCommand verifying that short names get resolved.

## Do NOT rename the project or change import paths. Module is "clod".

Commit and push when done.
