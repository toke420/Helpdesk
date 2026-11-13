//go:build !devdriver

package main

// Pure-Go SQLite driver (no CGO), registered as "sqlite".
import _ "modernc.org/sqlite"

// sqliteDSN builds the connection string with per-connection pragmas.
func sqliteDSN(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
}
