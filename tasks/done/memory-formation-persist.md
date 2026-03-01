# Task: Persist memory formation timestamp (#228)

## Problem
`lastMemoryFormation` in keepalive/keepalive.go is in-memory only — resets to `now` on restart. This means after a restart, formation won't run until the next interval passes, even if it was overdue.

## Reference
Consolidation already does this correctly:
- **Persist:** line ~401: `r.stateStore.Set("consolidation_last:"+r.agentID, time.Now())`
- **Restore:** line ~140: `cfg.StateStore.Get("consolidation_last:"+cfg.AgentID, &ts)`

## Fix
Apply the same pattern to memory formation:

1. **Persist** after formation runs (~line 340 where `r.lastMemoryFormation = now`):
   ```go
   if r.stateStore != nil {
       r.stateStore.Set("formation_last:"+r.agentID, time.Now())
   }
   ```

2. **Restore** in NewRunner (~line 138, after consolidation restore):
   ```go
   if cfg.StateStore.Get("formation_last:"+cfg.AgentID, &ts) {
       r.lastMemoryFormation = ts
   }
   ```

3. **Remove the Truncate** call if present — wall-clock alignment via Truncate was a workaround for not persisting.

## Verification
- `go build -o foci . && go test ./... && go vet ./...`
- Check that formation_last key appears in state.db after a formation run

Commit and push.
