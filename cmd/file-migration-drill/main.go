// file-migration-drill restores a frozen pre-F1 schema with synthetic legacy data
// into separate PostgreSQL databases, upgrades it, and verifies backup recovery.
// It never migrates the database named by AUTOBID_TEST_DATABASE_URL itself.
package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const oldRef = "e4bc8a510c775562f7be5d693cbd64310af4ea37"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "drill failed:", err)
		os.Exit(1)
	}
}
func run() error {
	bin := flag.String("pg-bin", "", "directory containing pg_dump and pg_restore")
	out := flag.String("out", "data/file-processing-drill", "artifact parent directory (synthetic data only)")
	flag.Parse()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("AUTOBID_TEST_DATABASE_URL is required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("invalid test database configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := &db.DB{DB: stdlib.OpenDB(*cfg)}
	defer admin.Close()
	if err = admin.PingContext(ctx); err != nil {
		return fmt.Errorf("cannot connect to test PostgreSQL")
	}
	stamp := time.Now().UTC().Format("20060102t150405") + fmt.Sprintf("_%d", os.Getpid())
	dir := filepath.Join(*out, stamp)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var created []string
	var opened []*db.DB
	defer func() {
		for _, d := range opened {
			d.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range created {
			// Only identifiers generated and successfully created in this invocation.
			if name == cfg.Database || !strings.HasPrefix(name, "autobid_drill_"+stamp+"_") {
				continue
			}
			if _, e := admin.ExecContext(cleanup, `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); e != nil {
				fmt.Fprintln(os.Stderr, "cleanup failed for", name)
			}
		}
	}()
	create := func(suffix string) (*db.DB, *pgx.ConnConfig, error) {
		name := "autobid_drill_" + stamp + "_" + suffix
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+` TEMPLATE template0`); err != nil {
			return nil, nil, fmt.Errorf("create isolated %s database: %w", suffix, err)
		}
		created = append(created, name)
		c := cfg.Copy()
		c.Database = name
		c.RuntimeParams["search_path"] = "public"
		d := &db.DB{DB: stdlib.OpenDB(*c)}
		opened = append(opened, d)
		return d, c, nil
	}
	runPG := func(name string, c *pgx.ConnConfig, args ...string) error {
		if name == "pg_restore" {
			args = append([]string{"--dbname=" + c.Database}, args...)
		}
		executable := name
		if *bin != "" {
			executable = filepath.Join(*bin, name)
		}
		cmd := exec.CommandContext(ctx, executable, args...)
		ssl := "disable"
		if c.TLSConfig != nil {
			ssl = "require"
		}
		cmd.Env = append(os.Environ(), "PGHOST="+c.Host, fmt.Sprintf("PGPORT=%d", c.Port), "PGUSER="+c.User, "PGPASSWORD="+c.Password, "PGDATABASE="+c.Database, "PGSSLMODE="+ssl)
		// Credentials are passed only through the child environment, never arguments/logs.
		data, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(data)))
		}
		return nil
	}
	oldSQL, err := exec.CommandContext(ctx, "git", "show", oldRef+":db/schema.sql").Output()
	if err != nil {
		return fmt.Errorf("read frozen legacy schema: %w", err)
	}
	legacy, legacyCfg, err := create("legacy")
	if err != nil {
		return err
	}
	if _, err = legacy.ExecContext(ctx, string(oldSQL)); err != nil {
		return fmt.Errorf("apply legacy schema: %w", err)
	}
	if err = seedLegacy(ctx, legacy, db.ContentHash(string(oldSQL))); err != nil {
		return err
	}
	before, err := legacySnapshot(ctx, legacy)
	if err != nil {
		return err
	}
	backup := filepath.Join(dir, "legacy.dump")
	if err = runPG("pg_dump", legacyCfg, "--format=custom", "--file="+backup); err != nil {
		return err
	}
	fmt.Println("PASS legacy schema + representative data backed up")
	upgrade, upgradeCfg, err := create("upgrade")
	if err != nil {
		return err
	}
	if err = runPG("pg_restore", upgradeCfg, "--exit-on-error", "--no-owner", "--no-privileges", backup); err != nil {
		return err
	}
	if err = upgrade.Migrate(ctx); err != nil {
		return fmt.Errorf("legacy upgrade: %w", err)
	}
	after, err := legacySnapshot(ctx, upgrade)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("legacy document/requirement/evidence state changed during upgrade")
	}
	var original *int64
	var revision int
	if err = upgrade.QueryRowContext(ctx, `SELECT original_file_id,parse_revision FROM tender_documents WHERE id=1`).Scan(&original, &revision); err != nil || original != nil || revision != 0 {
		return fmt.Errorf("legacy original/revision defaults incorrect: %v", err)
	}
	fmt.Println("PASS isolated restore + current migration preserved legacy state")
	empty, _, err := create("empty")
	if err != nil {
		return err
	}
	if err = empty.Migrate(ctx); err != nil {
		return fmt.Errorf("empty migration: %w", err)
	}
	if err = empty.Migrate(ctx); err != nil {
		return err
	}
	fmt.Println("PASS empty database + repeated migration")
	rollback, rollbackCfg, err := create("rollback")
	if err != nil {
		return err
	}
	if err = runPG("pg_restore", rollbackCfg, "--exit-on-error", "--no-owner", "--no-privileges", backup); err != nil {
		return err
	}
	restored, err := legacySnapshot(ctx, rollback)
	if err != nil || restored != before {
		return fmt.Errorf("rollback did not restore legacy data: %v", err)
	}
	var checksum string
	if err = rollback.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE id=1`).Scan(&checksum); err != nil || checksum != db.ContentHash(string(oldSQL)) {
		return fmt.Errorf("rollback schema checksum incorrect")
	}
	var columns int
	if err = rollback.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='tender_documents' AND column_name='original_file_id'`).Scan(&columns); err != nil || columns != 0 {
		return fmt.Errorf("rollback still contains new schema")
	}
	// Exercise the actual old Go read path against the restored old schema.
	if err = oldReader(ctx, rollbackCfg, dir); err != nil {
		return err
	}
	fmt.Println("PASS backup restored to separate rollback database + old-version reader")
	// A current backup must include both PostgreSQL and the content-addressed files.
	originals := filepath.Join(dir, "originals")
	store, err := filework.NewStore(upgrade, originals)
	if err != nil {
		return err
	}
	source := []byte("编号,金额\n001,123.45\n")
	object, err := store.PutBytes(ctx, filework.PutOptions{TaskID: 1, Kind: db.FileKindOriginal, OriginalName: "restore.csv", Format: "csv"}, source)
	if err != nil {
		return err
	}
	p := &filework.Processor{DB: upgrade, Store: store}
	record, err := p.Process(ctx, 1, filework.ProcessInput{FileID: object.ID, TenderNo: "DRILL-CSV", RequestKey: "csv"}, "drill")
	if err != nil {
		return err
	}
	var parsed ingest.Result
	if err = json.Unmarshal(record.Result, &parsed); err != nil {
		return err
	}
	beforeReplay, err := legacySnapshot(ctx, upgrade)
	if err != nil {
		return err
	}
	// Force a full schema replay, not merely the matching-checksum fast path.
	if _, err = upgrade.ExecContext(ctx, `UPDATE schema_version SET checksum='force-drill-replay'`); err != nil {
		return err
	}
	if err = upgrade.Migrate(ctx); err != nil {
		return err
	}
	var pages int
	if err = upgrade.QueryRowContext(ctx, `SELECT count(*) FROM document_chunks WHERE doc_id=$1 AND (page_start IS NOT NULL OR page_end IS NOT NULL)`, parsed.Document.ID).Scan(&pages); err != nil || pages != 0 {
		return fmt.Errorf("schema replay fabricated spreadsheet pages")
	}
	if after, err = legacySnapshot(ctx, upgrade); err != nil || after != beforeReplay {
		return fmt.Errorf("schema replay changed legacy evidence: %v", err)
	}
	// The manifest may legitimately change when the new file is imported, so the
	// source document/evidence checks below are covered separately by readback.
	currentDump := filepath.Join(dir, "current.dump")
	if err = runPG("pg_dump", upgradeCfg, "--format=custom", "--file="+currentDump); err != nil {
		return err
	}
	restoredFiles := filepath.Join(dir, "restored-originals")
	if err = copyTree(originals, restoredFiles); err != nil {
		return err
	}
	recovered, recoveredCfg, err := create("files")
	if err != nil {
		return err
	}
	if err = runPG("pg_restore", recoveredCfg, "--exit-on-error", "--no-owner", "--no-privileges", currentDump); err != nil {
		return err
	}
	restoredStore, err := filework.NewStore(recovered, restoredFiles)
	if err != nil {
		return err
	}
	raw, _, err := restoredStore.Read(ctx, db.DefaultTenant, 1, object.ID)
	if err != nil || string(raw) != string(source) {
		return fmt.Errorf("original backup hash/readback failed: %v", err)
	}
	if _, err = recovered.GetTenderDocument(ctx, 1, parsed.Document.ID); err != nil {
		return err
	}
	fmt.Println("PASS current database + original directory restore and SHA-256 readback")
	report := map[string]any{"at": time.Now().UTC(), "kind": "synthetic legacy upgrade and recovery rehearsal", "legacy_commit": oldRef, "legacy_schema_sha256": db.ContentHash(string(oldSQL)), "databases": created, "checks": []string{"legacy_backup", "isolated_upgrade_preserves_data", "empty_database", "repeat_migration", "rollback_old_version_reader", "spreadsheet_no_fake_pages_after_schema_replay", "database_and_originals_restore"}, "all_passed": true, "limitation": "No real historical production backup was supplied; legacy data is synthetic. Not a production restore or V/P acceptance."}
	rawReport, _ := json.MarshalIndent(report, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(rawReport, '\n'), 0600); err != nil {
		return err
	}
	fmt.Println("Report:", filepath.Join(dir, "report.json"))
	return nil
}

func seedLegacy(ctx context.Context, d *db.DB, checksum string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO explorations(id,goal) VALUES(1,'migration rehearsal');
 INSERT INTO tasks(id,name,exploration_id) VALUES(1,'legacy rehearsal',1);
 INSERT INTO tender_documents(id,task_id,tender_no,title,raw_text,content_hash) VALUES(1,1,'LEGACY','旧招标文件','投标人须提供营业执照。','legacy-hash');
 INSERT INTO document_chunks(id,doc_id,task_id,seq,level,section_code,section_title,page_no,page_start,page_end,content,token_estimate,is_mandatory) VALUES(1,1,1,0,1,'1.1','资格',1,1,1,'投标人须提供营业执照。',10,true);
 INSERT INTO requirement_manifests(task_id) VALUES(1);
 INSERT INTO requirement_items(task_id,code,category,title,origin,source_doc_id,source_chunk_id,source_hash,review_status) VALUES(1,'REQ-1','mandatory_clause','营业执照','extracted',1,1,'legacy-source','confirmed');
 UPDATE requirement_manifests SET status='confirmed',confirmed_at=now() WHERE task_id=1;
 INSERT INTO retrieval_runs(id,task_id,request_key,request_hash,request,kind,status,max_calls,max_tokens,deadline) VALUES(1,1,'legacy','legacy','{}','search','completed',1,1,now()+interval '1 hour');
 INSERT INTO retrieval_evidence(run_id,source_domain,source_id,doc_id,content_hash,snapshot,status) VALUES(1,'tender',1,1,'legacy-hash','{"text":"投标人须提供营业执照。"}','verified');
 SELECT setval(pg_get_serial_sequence('tender_documents','id'),1);
 SELECT setval(pg_get_serial_sequence('document_chunks','id'),1);
 CREATE TABLE IF NOT EXISTS schema_version(id INT PRIMARY KEY CHECK(id=1),checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT now());`)
	if err != nil {
		return fmt.Errorf("seed legacy: %w", err)
	}
	_, err = d.ExecContext(ctx, `INSERT INTO schema_version(id,checksum) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET checksum=EXCLUDED.checksum`, checksum)
	return err
}
func legacySnapshot(ctx context.Context, d *db.DB) (string, error) {
	var s string
	err := d.QueryRowContext(ctx, `SELECT jsonb_build_object(
 'doc',(SELECT jsonb_build_array(id,task_id,title,raw_text,content_hash) FROM tender_documents WHERE id=1),
 'chunk',(SELECT jsonb_build_array(id,doc_id,content,page_start,page_end) FROM document_chunks WHERE id=1),
 'requirement',(SELECT jsonb_build_array(code,source_doc_id,source_chunk_id,source_hash,review_status) FROM requirement_items WHERE code='REQ-1'),
 'manifest',(SELECT jsonb_build_array(status,epoch) FROM requirement_manifests WHERE task_id=1),
 'evidence',(SELECT jsonb_build_array(status,content_hash,snapshot) FROM retrieval_evidence WHERE run_id=1))::text`).Scan(&s)
	return s, err
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(to, 0700)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
func oldReader(ctx context.Context, cfg *pgx.ConnConfig, dir string) error {
	// Export old source without changing the user's checkout or worktrees.
	archive := filepath.Join(dir, "old-source.tar")
	if out, err := exec.CommandContext(ctx, "git", "archive", "--format=tar", "--output="+archive, oldRef).CombinedOutput(); err != nil {
		return fmt.Errorf("old source archive: %w %s", err, out)
	}
	source := filepath.Join(dir, "old-source")
	if err := os.MkdirAll(source, 0700); err != nil {
		return err
	}
	if err := extractSource(archive, source); err != nil {
		return err
	}
	program := `package main
import("context";"os";"github.com/autobid/autobid-engine/db")
func main(){ctx:=context.Background();d,err:=db.Open(ctx,os.Getenv("AUTOBID_DRILL_RESTORE_URL"));if err!=nil{panic("open failed")};defer d.Close();v,err:=d.GetTenderDocument(ctx,1,1);if err!=nil||v.Title!="旧招标文件"{panic("old reader failed")}}
`
	target := filepath.Join(source, "cmd", "restore-reader")
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(target, "main.go"), []byte(program), 0600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/restore-reader")
	cmd.Dir = source
	// ConnString must reflect the new database: pgx config's original string does not.
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", quoteDSN(cfg.Host), cfg.Port, quoteDSN(cfg.User), quoteDSN(cfg.Password), quoteDSN(cfg.Database))
	cmd.Env = append(os.Environ(), "AUTOBID_DRILL_RESTORE_URL="+dsn)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("old reader: %w %s", err, out)
	}
	return nil
}
func quoteDSN(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func extractSource(archive, root string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		name := filepath.FromSlash(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(filepath.Clean(name), ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path")
		}
		target := filepath.Join(root, name)
		if h.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported source archive entry")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
}
