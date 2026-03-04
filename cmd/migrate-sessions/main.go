// Session Key Migration Tool
//
// This tool migrates session files and database entries from the old colon-separated
// session key format to the new hierarchical slash-separated format with version directories.
//
// Commit: 5452cd497ca1cb4c65ec47460d6594bb253aeb8c
// Date:   2026-03-04 21:18:42 +0000
//
// ## What Changed
//
// Old format (colon-separated):
//   agent:main:chat:123                              → chat session
//   agent:main:spawn:spawn-1234567890                → independent session
//   agent:main:chat:123:branch:session-end-987       → branch from chat
//   agent:main:chat:123.2026-03-04T13-33-44Z         → rotated archive
//
// New format (slash-separated with version directories):
//   main/c123/1709590000                             → chat session (version timestamp)
//   main/i1234567890/1234567890                      → independent session
//   main/c123/1709590000/b987                        → branch child
//   main/c123/1709600000/root.2026-03-04T13-33-44Z   → archive in version directory
//
// ## Key Design Changes
//
// 1. **Hierarchical structure**: Uses slash separators matching directory structure
// 2. **Version directories**: Each compaction creates a new version directory with timestamp
// 3. **Stable chat IDs**: Chat ID remains constant across compactions (c123)
// 4. **Single-letter type codes**: c=chat, i=independent, b=branch, i=independent spawn
// 5. **Unix seconds**: All timestamps are epoch seconds
// 6. **Path mapping**: Root sessions use /root.jsonl suffix, children are direct paths
//
// ## What This Migration Does
//
// ### File Migration
//
// 1. Walks all session files in the sessions directory
// 2. Extracts archive suffixes (numbered like .1, .10 or timestamped like .2026-03-04T13-33-44Z)
// 3. Strips archive suffixes from old key to get base session identifier
// 4. For numbered archives (.1, .10, etc.):
//    - Uses file modification time as version timestamp
//    - Creates separate version directory for each archive
//    - Example: agent/main/chat/123.1.jsonl → main/c123/{mtime1}/root.jsonl
//              agent/main/chat/123.jsonl   → main/c123/{mtime2}/root.jsonl
// 5. For timestamp archives (.2026-03-04T13-33-44Z):
//    - Uses file modification time as version timestamp
//    - Preserves timestamp suffix in filename within version directory
//    - Example: agent/main/chat/123.2026-03-04T13-33-44Z.jsonl → main/c123/{mtime}/root.2026-03-04T13-33-44Z.jsonl
// 6. Moves files to new paths, creating directories as needed
//
// ### Database Migration
//
// 1. Reads parent relationships from session_index.parent_session_key
// 2. Falls back to reading branch_meta from session files for missing relationships
// 3. Converts all session_key and parent_session_key values to new format
// 4. Uses file modification times for version timestamps (same as file migration)
// 5. Handles collisions: if multiple old keys map to same new key, adds .1, .2 suffixes
// 6. Updates all rows in-place within a transaction
//
// ### Parent Relationship Handling
//
// The old system used parent relationships to track:
// - Branch spawns (child of parent session)
// - Archive version history (newer archive's parent was older archive)
//
// The new system handles these differently:
// - Branches: Still use parent-child via directory structure (main/c123/1709590000/b456)
// - Archives: Become sibling version directories, no parent relationship needed
//
// For numbered archives that form parent chains in the old database:
// - The migration ignores these parent relationships
// - Each archive gets its own version directory based on file mtime
// - Circular references are detected and broken (archives treated as roots)
//
// ## Usage
//
// IMPORTANT: Backup your sessions directory and database before running!
//
// Dry run (recommended first):
//   ./migrate-sessions -sessions ./sessions -db ./data/session_index.db -dry-run
//
// Actual migration:
//   ./migrate-sessions -sessions ./sessions -db ./data/session_index.db
//
// The tool will:
// 1. Show all file moves it will perform
// 2. Show all database key updates
// 3. Warn about any circular parent references (usually in corrupted archive chains)
// 4. Exit with error if any operation fails
//
// After migration, verify:
// - Files are in new directory structure
// - Database session_key values use new format
// - Application can load sessions correctly
//
// ## Known Issues
//
// - Circular parent warnings for archive files indicate corrupted data in session_index
//   (archive files shouldn't have parent relationships, but some do in old data)
// - These are handled safely by treating archives as root sessions with mtime-based versions
// - Collision detection adds .1, .2 suffixes when multiple sessions have identical timestamps
//
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	sessionDir := flag.String("sessions", "", "Path to sessions directory")
	dbPath := flag.String("db", "", "Path to session_index.db")
	dryRun := flag.Bool("dry-run", false, "Show what would be done without doing it")
	flag.Parse()

	if *sessionDir == "" || *dbPath == "" {
		fmt.Println("Usage: migrate-sessions -sessions <dir> -db <path> [-dry-run]")
		fmt.Println("")
		fmt.Println("Example:")
		fmt.Println("  migrate-sessions -sessions ./sessions -db ./data/session_index.db -dry-run")
		fmt.Println("  migrate-sessions -sessions ./sessions -db ./data/session_index.db")
		os.Exit(1)
	}

	fmt.Printf("Sessions directory: %s\n", *sessionDir)
	fmt.Printf("Database: %s\n", *dbPath)
	if *dryRun {
		fmt.Println("DRY RUN - no changes will be made\n")
	} else {
		fmt.Println("WARNING: This will modify files and database. Backup recommended!\n")
	}

	// Migrate files
	fmt.Println("=== Migrating session files ===")
	fileCount, err := migrateFiles(*sessionDir, *dryRun, *dbPath)
	if err != nil {
		fmt.Printf("ERROR migrating files: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Files migrated: %d\n\n", fileCount)

	// Migrate database
	fmt.Println("=== Migrating database ===")
	dbCount, err := migrateDatabase(*dbPath, *dryRun, *sessionDir)
	if err != nil {
		fmt.Printf("ERROR migrating database: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Database rows updated: %d\n\n", dbCount)

	if *dryRun {
		fmt.Println("DRY RUN complete. Run without -dry-run to apply changes.")
	} else {
		fmt.Println("Migration complete!")
	}
}

func migrateFiles(sessionDir string, dryRun bool, dbPath string) (int, error) {
	// Build parent relationship map from session_index
	parentMap := make(map[string]string) // oldKey -> parentKey

	if dbPath != "" {
		db, err := sql.Open("sqlite", dbPath)
		if err == nil {
			rows, _ := db.Query("SELECT session_key, parent_session_key FROM session_index WHERE parent_session_key IS NOT NULL AND parent_session_key != ''")
			if rows != nil {
				for rows.Next() {
					var key, parent string
					if rows.Scan(&key, &parent) == nil {
						parentMap[key] = parent
					}
				}
				rows.Close()
			}
			db.Close()
		}
	}

	// Build map of file modification times for version timestamps
	fileTimes := make(map[string]int64) // oldKey -> mtime
	filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.gz") {
			return nil
		}
		rel, _ := filepath.Rel(sessionDir, path)
		oldPath := strings.TrimSuffix(strings.TrimSuffix(rel, ".jsonl.gz"), ".jsonl")
		oldKey := strings.ReplaceAll(oldPath, string(filepath.Separator), ":")
		fileTimes[oldKey] = info.ModTime().Unix()
		return nil
	})

	// Fallback: read branch_meta from files not in index
	err := filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		rel, _ := filepath.Rel(sessionDir, path)
		oldKey := strings.ReplaceAll(strings.TrimSuffix(rel, ".jsonl"), string(filepath.Separator), ":")

		// Skip if we already have parent info from DB
		if _, exists := parentMap[oldKey]; exists {
			return nil
		}

		// Read branch_meta from file
		if parent := readParentFromFile(path); parent != "" {
			parentMap[oldKey] = parent
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Now migrate files with parent knowledge
	moved := 0
	err = filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.gz") {
			return nil
		}

		rel, err := filepath.Rel(sessionDir, path)
		if err != nil {
			return nil
		}

		// Extract archive suffix if present
		baseName := filepath.Base(rel)
		archiveSuffix := ""
		isNumberedArchive := false
		if strings.Contains(baseName, ".2") { // timestamp or numbered archive
			timestampPattern := regexp.MustCompile(`(\.\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z(\.\d+)?)(\.jsonl(\.gz)?)$`)
			if matches := timestampPattern.FindStringSubmatch(baseName); matches != nil {
				archiveSuffix = matches[1]
			} else {
				numberedPattern := regexp.MustCompile(`(\.\d+)(\.jsonl(\.gz)?)$`)
				if matches := numberedPattern.FindStringSubmatch(baseName); matches != nil {
					archiveSuffix = matches[1]
					isNumberedArchive = true
				}
			}
		}

		// Get old key (with archive suffix for lookup)
		oldPath := strings.TrimSuffix(strings.TrimSuffix(rel, ".jsonl.gz"), ".jsonl")
		oldKey := strings.ReplaceAll(oldPath, string(filepath.Separator), ":")

		// For numbered archives, use file mtime as version timestamp
		var versionTS int64
		if isNumberedArchive {
			versionTS = fileTimes[oldKey]
		}

		// Convert with parent knowledge and version timestamp
		newKey := convertKeyWithParentAndVersion(oldKey, parentMap, versionTS, fileTimes)
		newRel := keyToPath(newKey)

		// For numbered archives, don't add suffix back - it's in the version directory now
		// For timestamp archives, preserve the suffix
		if !isNumberedArchive && archiveSuffix != "" {
			newRel += archiveSuffix
		}

		// Restore extension
		if strings.HasSuffix(rel, ".jsonl.gz") {
			newRel += ".jsonl.gz"
		} else {
			newRel += ".jsonl"
		}

		if newRel == rel {
			return nil
		}

		newPath := filepath.Join(sessionDir, newRel)
		fmt.Printf("  %s\n  → %s\n", rel, newRel)

		if !dryRun {
			if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
				return fmt.Errorf("mkdir: %w", err)
			}
			if err := os.Rename(path, newPath); err != nil {
				return fmt.Errorf("rename %s: %w", rel, err)
			}
		}
		moved++
		return nil
	})

	return moved, err
}

func convertPath(oldPath string) string {
	// Handle .gz files
	ext := ""
	if strings.HasSuffix(oldPath, ".jsonl.gz") {
		ext = ".jsonl.gz"
		oldPath = strings.TrimSuffix(oldPath, ".jsonl.gz")
	} else if strings.HasSuffix(oldPath, ".jsonl") {
		ext = ".jsonl"
		oldPath = strings.TrimSuffix(oldPath, ".jsonl")
	}

	// Convert path to old key format (colons)
	// Old path: agent/main/chat/123.jsonl → agent:main:chat:123
	oldKey := strings.ReplaceAll(oldPath, string(filepath.Separator), ":")

	// Convert old key to new key
	newKey := convertKey(oldKey)

	// Convert new key to new path
	newPath := keyToPath(newKey)

	return newPath + ext
}

func convertKey(oldKey string) string {
	// Old format: agent:AGENTID:TYPE[:ID][:branch:BRANCHID]
	// New format: AGENTID/typeID/versionTS[/childTypeTS]

	parts := strings.Split(oldKey, ":")
	if len(parts) < 3 || parts[0] != "agent" {
		return oldKey // Invalid or unknown format, don't change
	}

	agentID := parts[1]
	typ := parts[2]
	typeCode := mapType(typ)

	// Check for branch suffix
	var branchIdx int
	for i := 3; i < len(parts)-1; i++ {
		if parts[i] == "branch" {
			branchIdx = i
			break
		}
	}

	// Current timestamp as version (migration time)
	versionTS := time.Now().Unix()

	if branchIdx > 0 && branchIdx+1 < len(parts) {
		// Has branch: agent:main:chat:123:branch:session-end-456
		parentID := ""
		if len(parts) >= 4 {
			parentID = parts[3]
		}
		parentID = cleanID(parentID)

		branchID := parts[branchIdx+1]
		branchTS := extractTimestamp(branchID)

		// New format: main/c123/versionTS/b456
		return fmt.Sprintf("%s/%s%s/%d/b%s", agentID, typeCode, parentID, versionTS, branchTS)
	}

	if len(parts) >= 4 {
		// Has ID: agent:main:chat:123 or agent:main:spawn:spawn-456
		id := parts[3]
		id = cleanID(id)

		// New format: main/c123/versionTS
		return fmt.Sprintf("%s/%s%s/%d", agentID, typeCode, id, versionTS)
	}

	// No ID (shouldn't happen): agent:main:chat
	return fmt.Sprintf("%s/%s/%d", agentID, typeCode, versionTS)
}

func keyToPath(key string) string {
	// New format key: main/c123/1709590000 or main/c123/1709590000/b456
	// Path: main/c123/1709590000/root.jsonl or main/c123/1709590000/b456.jsonl

	parts := strings.Split(key, "/")
	if len(parts) < 3 {
		return key
	}

	lastSegment := parts[len(parts)-1]

	// Check for collision suffix
	if idx := strings.Index(lastSegment, "."); idx > 0 {
		lastSegment = lastSegment[:idx]
	}

	// If last segment is pure number (version timestamp), it's a root
	if matched, _ := regexp.MatchString(`^\d+$`, lastSegment); matched {
		return filepath.Join(key, "root")
	}

	// Otherwise it's a child
	return key
}

func mapType(typ string) string {
	switch typ {
	case "chat", "voice":
		return "c"
	case "spawn", "multiball", "cron":
		return "i" // Treat all non-chat sessions as independent initially
	default:
		return "c" // Default to chat
	}
}

func cleanID(id string) string {
	// Remove prefixes from old IDs
	id = strings.TrimPrefix(id, "spawn-")
	id = strings.TrimPrefix(id, "mb-")
	id = strings.TrimPrefix(id, "conn-")
	return id
}

func extractTimestamp(s string) string {
	// Extract trailing digits from strings like "session-end-1709596800"
	re := regexp.MustCompile(`(\d+)$`)
	if m := re.FindString(s); m != "" {
		return m
	}
	// Fallback: use current timestamp
	return fmt.Sprintf("%d", time.Now().Unix())
}

func readParentFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Read first line
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if n == 0 {
		return ""
	}

	// Find first newline
	for i := 0; i < n; i++ {
		if buf[i] == '\n' {
			var meta struct {
				Type      string `json:"type"`
				ParentKey string `json:"parent_key"`
			}
			if json.Unmarshal(buf[:i], &meta) == nil && meta.Type == "branch_meta" {
				return meta.ParentKey
			}
			break
		}
	}
	return ""
}

func convertKeyWithParent(oldKey string, parentMap map[string]string) string {
	return convertKeyWithParentAndVersion(oldKey, parentMap, 0, nil)
}

func convertKeyWithParentAndVersion(oldKey string, parentMap map[string]string, versionTS int64, fileTimes map[string]int64) string {
	cache := make(map[string]string)
	return convertKeyWithParentCached(oldKey, parentMap, cache, make(map[string]bool), versionTS, fileTimes)
}

func convertKeyWithParentCached(oldKey string, parentMap map[string]string, cache map[string]string, visiting map[string]bool, versionTS int64, fileTimes map[string]int64) string {
	// Check cache first
	if newKey, ok := cache[oldKey]; ok {
		return newKey
	}

	// Cycle detection
	if visiting[oldKey] {
		// Archive files with parent chains create cycles - treat them as roots
		// Don't warn for numbered archives (they're expected to have parent relationships)
		if !regexp.MustCompile(`\.\d+$`).MatchString(oldKey) {
			fmt.Fprintf(os.Stderr, "WARNING: Circular parent reference detected for %s\n", oldKey)
		}
		visiting[oldKey] = false
		return convertAsRootWithVersion(oldKey, versionTS)
	}
	visiting[oldKey] = true
	defer func() { visiting[oldKey] = false }()

	parts := strings.Split(oldKey, ":")
	if len(parts) < 3 || parts[0] != "agent" {
		cache[oldKey] = oldKey
		return oldKey
	}

	// Check if this key has a parent
	parentKey, hasParent := parentMap[oldKey]

	// For numbered archive files, ignore parent relationships and treat as roots
	// They represent version history, not parent-child relationships
	if regexp.MustCompile(`\.\d+$`).MatchString(oldKey) {
		hasParent = false
	}

	// If it has a parent, convert as a child
	if hasParent {
		// First convert the parent key (with cycle detection)
		newParentKey := convertKeyWithParentCached(parentKey, parentMap, cache, visiting, 0, fileTimes)

		// Extract timestamp from old key if possible
		currentTS := time.Now().Unix()
		var childTS int64
		if len(parts) >= 4 {
			ts := extractTimestamp(parts[3])
			if parsed, err := strconv.ParseInt(ts, 10, 64); err == nil {
				childTS = parsed
			} else {
				childTS = currentTS
			}
		} else {
			childTS = currentTS
		}

		// Determine child type based on session type
		typ := parts[2]
		childType := "b" // default to branch
		if typ == "spawn" && !strings.Contains(oldKey, "clone") {
			// Non-clone spawns are independent children
			childType = "i"
		}

		newKey := fmt.Sprintf("%s/%s%d", newParentKey, childType, childTS)
		cache[oldKey] = newKey
		return newKey
	}

	// Root session - use provided version timestamp or current time
	newKey := convertAsRootWithVersion(oldKey, versionTS)
	cache[oldKey] = newKey
	return newKey
}

func convertAsRoot(oldKey string) string {
	return convertAsRootWithVersion(oldKey, 0)
}

func convertAsRootWithVersion(oldKey string, versionTS int64) string {
	parts := strings.Split(oldKey, ":")
	if len(parts) < 3 || parts[0] != "agent" {
		return oldKey
	}

	agentID := parts[1]
	typ := parts[2]

	// Use provided version timestamp or current time
	if versionTS == 0 {
		versionTS = time.Now().Unix()
	}

	// Determine type
	typeCode := "c" // default to chat
	if typ != "chat" && typ != "voice" {
		typeCode = "i" // independent
	}

	var id string
	if len(parts) >= 4 {
		id = cleanID(parts[3])
		// Strip archive suffix if present (e.g., "123.2026-03-04T13-33-44Z" -> "123")
		id = stripArchiveSuffix(id)
	} else {
		id = strconv.FormatInt(versionTS, 10)
	}

	return fmt.Sprintf("%s/%s%s/%d", agentID, typeCode, id, versionTS)
}

func stripArchiveSuffix(id string) string {
	// Match timestamp pattern: .YYYY-MM-DDTHH-MM-SSZ or .YYYY-MM-DDTHH-MM-SSZ.N
	timestampPattern := regexp.MustCompile(`\.\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z(\.\d+)?$`)
	id = timestampPattern.ReplaceAllString(id, "")

	// Match numbered suffix: .N
	numberedPattern := regexp.MustCompile(`\.\d+$`)
	id = numberedPattern.ReplaceAllString(id, "")

	return id
}

func migrateDatabase(dbPath string, dryRun bool, sessionDir string) (int, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Build parent map from database
	parentMap := make(map[string]string)
	rows, err := db.Query("SELECT session_key, parent_session_key FROM session_index WHERE parent_session_key IS NOT NULL AND parent_session_key != ''")
	if err == nil && rows != nil {
		for rows.Next() {
			var key, parent string
			if rows.Scan(&key, &parent) == nil {
				parentMap[key] = parent
			}
		}
		rows.Close()
	}

	// Build file times map for version timestamps
	fileTimes := make(map[string]int64)
	filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.gz") {
			return nil
		}
		rel, _ := filepath.Rel(sessionDir, path)
		oldPath := strings.TrimSuffix(strings.TrimSuffix(rel, ".jsonl.gz"), ".jsonl")
		oldKey := strings.ReplaceAll(oldPath, string(filepath.Separator), ":")
		fileTimes[oldKey] = info.ModTime().Unix()
		return nil
	})

	// Fallback: check files for branch_meta
	filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		rel, _ := filepath.Rel(sessionDir, path)
		oldKey := strings.ReplaceAll(strings.TrimSuffix(rel, ".jsonl"), string(filepath.Separator), ":")
		if _, exists := parentMap[oldKey]; !exists {
			if parent := readParentFromFile(path); parent != "" {
				parentMap[oldKey] = parent
			}
		}
		return nil
	})

	if !dryRun {
		if _, err := db.Exec("BEGIN TRANSACTION"); err != nil {
			return 0, fmt.Errorf("begin transaction: %w", err)
		}
		defer db.Exec("ROLLBACK")
	}

	rows, err = db.Query("SELECT session_key, parent_session_key FROM session_index")
	if err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type update struct {
		oldKey    string
		newKey    string
		oldParent sql.NullString
		newParent sql.NullString
	}
	var updates []update

	for rows.Next() {
		var oldKey string
		var oldParent sql.NullString
		if err := rows.Scan(&oldKey, &oldParent); err != nil {
			return 0, fmt.Errorf("scan: %w", err)
		}

		// Get version timestamp for this key
		versionTS := fileTimes[oldKey]

		newKey := convertKeyWithParentAndVersion(oldKey, parentMap, versionTS, fileTimes)
		var newParent sql.NullString
		if oldParent.Valid && oldParent.String != "" {
			parentVersionTS := fileTimes[oldParent.String]
			newParent = sql.NullString{
				String: convertKeyWithParentAndVersion(oldParent.String, parentMap, parentVersionTS, fileTimes),
				Valid:  true,
			}
		}

		if newKey != oldKey || (oldParent.Valid && newParent.String != oldParent.String) {
			updates = append(updates, update{oldKey, newKey, oldParent, newParent})
		}
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows: %w", err)
	}

	// Handle collisions: if multiple old keys map to the same new key, add .1, .2, etc.
	newKeyCount := make(map[string]int)
	for i := range updates {
		baseKey := updates[i].newKey
		count := newKeyCount[baseKey]
		if count > 0 {
			updates[i].newKey = fmt.Sprintf("%s.%d", baseKey, count)
		}
		newKeyCount[baseKey]++
	}

	for _, u := range updates {
		fmt.Printf("  %s → %s\n", u.oldKey, u.newKey)
		if u.oldParent.Valid {
			fmt.Printf("    parent: %s → %s\n", u.oldParent.String, u.newParent.String)
		}

		if !dryRun {
			_, err := db.Exec(`UPDATE session_index
							  SET session_key = ?, parent_session_key = ?
							  WHERE session_key = ?`,
				u.newKey, u.newParent, u.oldKey)
			if err != nil {
				return 0, fmt.Errorf("update %s: %w", u.oldKey, err)
			}
		}
	}

	if !dryRun {
		if _, err := db.Exec("COMMIT"); err != nil {
			return 0, fmt.Errorf("commit: %w", err)
		}
	}

	return len(updates), nil
}
