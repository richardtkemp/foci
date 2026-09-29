package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"foci/internal/delegator/accounting"
	"foci/internal/sqlite"
	"foci/internal/tempdir"
)

// runLedgerMigrateDryRun is `foci-gw ledger-migrate`: it runs the #2111 cost
// ledger migration on a COPY of an api.db and prints the migration report —
// the old-versus-new totals, overall and per day — without touching the
// original. The live cutover runs the same code (accounting.Open) at startup
// once the backends book through the ledger; this is how to see its effect on
// real history before then.
func runLedgerMigrateDryRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ledger-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "api.db to migrate a copy of (required; never modified)")
	out := fs.String("out", "", "keep the migrated copy at this path (must not exist); default: a temp file, removed afterwards")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `foci-gw ledger-migrate — dry-run the cost ledger migration (#2111)

Copies an api.db, migrates the copy to the per-call ledger, and prints the
migration report. The source database is only read.

Usage: foci-gw ledger-migrate -from <api.db> [-out <path>]

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" {
		fs.Usage()
		return 2
	}
	target := *out
	if target == "" {
		dir, err := tempdir.Mkdir("ledger-migrate-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "ledger-migrate: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		target = filepath.Join(dir, "api.db")
	}

	// Not OpenReadOnly: its query_only pragma refuses VACUUM INTO, which only
	// READS the source and writes the target file.
	src, err := sqlite.Open(*from)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-migrate: %v\n", err)
		return 1
	}
	_, err = src.Exec(`VACUUM INTO ?`, target)
	_ = src.Close()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-migrate: copy %s to %s: %v\n", *from, target, err)
		return 1
	}

	ledger, report, err := accounting.Open(target, accounting.Options{NoBackup: true})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-migrate: %v\n", err)
		return 1
	}
	_ = ledger.Close()
	if report == nil {
		_, _ = fmt.Fprintf(stdout, "%s already holds the ledger schema; nothing to migrate\n", *from)
		return 0
	}
	_, _ = fmt.Fprint(stdout, report.String())
	if *out != "" {
		_, _ = fmt.Fprintf(stdout, "migrated copy: %s\n", *out)
	}
	return 0
}
