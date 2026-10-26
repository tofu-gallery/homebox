// Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Homebox.

package migrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rowLevelSecurityFile states in SQL that every table has row-level security. Nothing runs it: the start's schema step
// (app/api/tofu_schema.go) turns it on for every table at every start. It is there for tools that read SQL files rather
// than run the app, such as Tofu's safety check, which warns about every table a .sql file makes when no .sql file
// turns row-level security on for it.
const rowLevelSecurityFile = "tofu/row_level_security.sql"

// The safety check's two patterns, as it writes them in JavaScript. They read every .sql file whole, comments included.
// They match as the safety check's do for ASCII text; Go's (?i) also folds the long s and the Kelvin sign into s and
// k, which no migration here uses.
var (
	createTablePattern = safetyCheckPattern(`create\s+table\s+(?:if\s+not\s+exists\s+)?((?:"[^"]+"|[\w]+)(?:\.(?:"[^"]+"|[\w]+))?)`)
	enableRLSPattern   = safetyCheckPattern(`alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?((?:"[^"]+"|[\w]+)(?:\.(?:"[^"]+"|[\w]+))?)\s+enable\s+row\s+level\s+security`)
)

// safetyCheckPattern compiles a pattern of the safety check: case-insensitive, and with JavaScript's \s, which also
// matches \v and the Unicode spaces, where Go's \s is only [\t\n\f\r ].
func safetyCheckPattern(js string) *regexp.Regexp {
	const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	return regexp.MustCompile(`(?i)` + strings.ReplaceAll(js, `\s`, jsSpace))
}

// The schemas whose tables the safety check never warns about.
var safetyCheckSystemSchemas = map[string]bool{
	"auth": true, "storage": true, "extensions": true, "realtime": true, "vault": true, "graphql": true,
	"graphql_public": true, "net": true, "cron": true, "pgsodium": true, "pgsodium_masks": true,
	"supabase_functions": true, "supabase_migrations": true, "information_schema": true, "pg_catalog": true,
}

// safetyCheckTable names a table as the safety check does: quotes removed, trimmed, lower-cased, and in public when
// no schema is given.
func safetyCheckTable(raw string) (schema, table string) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, `"`, ""))), ".")
	schema, table = "public", parts[len(parts)-1]
	if len(parts) > 1 {
		schema = parts[len(parts)-2]
	}
	return schema, table
}

// TestRowLevelSecurityFileNamesEveryTable keeps tofu/row_level_security.sql naming exactly the tables that every other
// .sql file in the repository makes, as the safety check reads them, so that a table a future upstream migration adds
// fails this test instead of bringing back the warning that it has no access rules.
func TestRowLevelSecurityFileNamesEveryTable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..")) // the repository root; the Go module is backend/
	if err != nil {
		t.Fatal(err)
	}
	ownPath, err := filepath.Abs(filepath.FromSlash(rowLevelSecurityFile))
	if err != nil {
		t.Fatal(err)
	}
	own, err := os.ReadFile(ownPath)
	if err != nil {
		t.Fatal(err)
	}

	made := map[string][]string{} // table: the files that make it
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".sql") || path == ownPath {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		for _, m := range createTablePattern.FindAllStringSubmatch(string(data), -1) {
			schema, table := safetyCheckTable(m[1])
			if !safetyCheckSystemSchemas[schema] {
				made[schema+"."+table] = append(made[schema+"."+table], filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(made) == 0 {
		t.Fatalf("found no table in the %d .sql files under %s", files, root)
	}

	if m := createTablePattern.FindAllString(string(own), -1); len(m) > 0 {
		t.Errorf("%s reads to the safety check as making a table itself (the words create table and a name, comments "+
			"included): %q", rowLevelSecurityFile, m)
	}
	listed := map[string]bool{}
	for _, m := range enableRLSPattern.FindAllStringSubmatch(string(own), -1) {
		schema, table := safetyCheckTable(m[1])
		listed[schema+"."+table] = true
	}

	var missing, extra []string
	for name, from := range made {
		if !listed[name] {
			schema, table, _ := strings.Cut(name, ".")
			missing = append(missing, "  ALTER TABLE IF EXISTS "+schema+`."`+table+`" ENABLE ROW LEVEL SECURITY; -- made in `+
				strings.Join(from, ", "))
		}
	}
	for name := range listed {
		if made[name] == nil {
			extra = append(extra, "  "+name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s does not name %d table(s) that other .sql files make; add these lines:\n%s", rowLevelSecurityFile,
			len(missing), strings.Join(missing, "\n"))
	}
	if len(extra) > 0 {
		t.Errorf("%s names %d table(s) that no other .sql file makes; remove their lines:\n%s", rowLevelSecurityFile,
			len(extra), strings.Join(extra, "\n"))
	}
	t.Logf("%d .sql files make %d tables; %s names %d", files, len(made), rowLevelSecurityFile, len(listed))
}
