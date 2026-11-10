// Package e2e runs browser acceptance against real, isolated server processes.
//
// Nothing here replaces the database, Office, Mermaid or OCR. Only the language model is a
// scripted substitute (web/e2e/stub_llm.py), and every report says so. Each test holds
// officetest.Hold for its whole duration, because cmd/server does not take that lock itself.
package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/export/officetest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const stubKey = "s7-scripted-substitute"

type harness struct {
	t         *testing.T
	ctx       context.Context
	root      string
	dir       string
	out       string
	dsn       string
	token     string
	tokenFile string
	bin       string
	stubURL   string
	stubPort  int
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newHarness prepares an isolated database, a freshly built server and the scripted model.
func newHarness(t *testing.T, suite string, timeout time.Duration) *harness {
	t.Helper()
	if os.Getenv("PLAYWRIGHT_MODULE") == "" {
		t.Skip("PLAYWRIGHT_MODULE is not set: browser acceptance was not run")
	}
	if os.Getenv("AUTOBID_TEST_DATABASE_URL") == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL is not set: browser acceptance was not run")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "web", "dist", "index.html")); err != nil {
		t.Fatal("run npm --prefix web run build before browser acceptance")
	}
	if os.Getenv("S7_DEBUG_OFF") == "1" {
		t.Setenv("AUTOBID_OFFICE_ENGINE", "off")
		t.Log("development run: document engine off, strict final export is NOT validated")
	} else {
		officetest.Hold(t)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("AUTOBID_TEST_DATABASE_URL is not a valid database configuration")
	}
	if cfg.Database == "" || strings.EqualFold(cfg.Database, "autobid") {
		t.Fatal("refusing to run against a database that is not a test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	name := "s7_e2e_" + randomID()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal("could not create the isolated acceptance database")
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error("could not remove the isolated acceptance database")
		}
	})
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), Path: "/" + name, User: url.UserPassword(cfg.User, cfg.Password)}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()

	dir := t.TempDir()
	out := filepath.Join(root, "data", "s7-checks", suite)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, root: root, dir: dir, out: out, dsn: u.String(), token: randomID(), bin: filepath.Join(dir, "server.exe")}
	h.tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(h.tokenFile, []byte(h.token), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", h.bin, "./cmd/server")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, b)
	}
	return h
}

// startStub runs the scripted model: delay seconds per Worker turn, steps turns per work item.
func (h *harness) startStub(delay, steps string) {
	h.t.Helper()
	h.stubPort = freePort(h.t)
	h.stubURL = fmt.Sprintf("http://127.0.0.1:%d", h.stubPort)
	cmd := exec.CommandContext(h.ctx, "python", "-X", "utf8", filepath.Join(h.root, "web", "e2e", "stub_llm.py"), fmt.Sprint(h.stubPort), delay, steps)
	logf, err := os.Create(filepath.Join(h.out, "stub.log"))
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		h.t.Fatal("could not start the scripted model")
	}
	h.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = logf.Close() })
	for until := time.Now().Add(20 * time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
		if res, err := http.Get(h.stubURL + "/state"); err == nil {
			res.Body.Close()
			return
		}
	}
	h.t.Fatal("scripted model did not start")
}

type server struct {
	h       *harness
	cmd     *exec.Cmd
	base    string
	done    chan struct{}
	once    sync.Once
	logMu   sync.Mutex
	logFile *os.File
}

// baseEnv drops any local configuration, so a run cannot reach the operator's database or models.
func (h *harness) baseEnv() []string {
	env := []string{}
	for _, s := range os.Environ() {
		k, _, _ := strings.Cut(s, "=")
		if strings.HasPrefix(strings.ToUpper(k), "AUTOBID_") || strings.HasPrefix(strings.ToUpper(k), "E2E_") {
			continue
		}
		env = append(env, s)
	}
	for _, k := range []string{"AUTOBID_MMDC_PATH", "AUTOBID_MMDC_BROWSER", "AUTOBID_OCR_URL", "AUTOBID_OFFICE_ENGINE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"AUTOBID_DATABASE_URL="+h.dsn,
		"AUTOBID_API_TOKEN="+h.token,
		"AUTOBID_WEB_DIR="+filepath.Join(h.root, "web", "dist"),
		"AUTOBID_FILE_PROCESSING_ENABLED=1",
		"AUTOBID_FILEWORK_DIR="+filepath.Join(h.dir, "files"),
		"AUTOBID_EXPORT_DIR="+filepath.Join(h.dir, "exports"),
		"AUTOBID_EVIDENCE_DIR="+filepath.Join(h.dir, "evidence"),
		"AUTOBID_LLM_KEY_FILE="+filepath.Join(h.dir, "master.key"),
		"AUTOBID_AGENT_EXPORT_ENABLED=0",
		"STUB_LLM_KEY="+stubKey,
	)
}

// start launches a real server process. Later entries in extra win over the base environment.
func (h *harness) start(extra ...string) *server {
	h.t.Helper()
	s, err := h.launch(fmt.Sprintf("127.0.0.1:%d", freePort(h.t)), extra...)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) launch(addr string, extra ...string) (*server, error) {
	logPath := filepath.Join(h.out, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(h.bin)
	cmd.Dir = h.root
	cmd.Env = append(append(h.baseEnv(), "AUTOBID_HTTP_ADDR="+addr), extra...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return nil, errors.New("could not start the server")
	}
	s := &server{h: h, cmd: cmd, base: "http://" + addr, done: make(chan struct{}), logFile: logFile}
	go func() { _ = cmd.Wait(); close(s.done) }()
	h.t.Cleanup(s.kill)
	return s, s.waitReady()
}

// kill ends the process the way a crash or a power cut would: no graceful shutdown.
func (s *server) kill() {
	s.once.Do(func() {
		_ = s.cmd.Process.Kill()
		<-s.done
		_ = s.logFile.Close()
	})
}

func (s *server) waitReady() error {
	h := s.h
	client := &http.Client{Timeout: 2 * time.Second}
	for until := time.Now().Add(4 * time.Minute); time.Now().Before(until); time.Sleep(200 * time.Millisecond) {
		select {
		case <-s.done:
			return fmt.Errorf("server exited during startup; see %s", filepath.Join(h.out, "server.log"))
		default:
		}
		req, _ := http.NewRequestWithContext(h.ctx, "GET", s.base+"/api/runtime/tools", nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		res, err := client.Do(req)
		if err != nil {
			continue
		}
		var body struct {
			Capabilities []struct {
				State string `json:"state"`
			} `json:"capabilities"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			continue
		}
		starting := false
		for _, c := range body.Capabilities {
			starting = starting || c.State == "starting"
		}
		if !starting {
			return nil
		}
	}
	return errors.New("server capabilities did not settle")
}

// control lets a browser suite kill and restart the real server, optionally with different
// configuration, so failures are real process deaths rather than simulated offline modes.
type control struct {
	h    *harness
	mu   sync.Mutex
	cur  *server
	addr string
	url  string
}

func (h *harness) control(s *server) *control {
	c := &control{h: h, cur: s, addr: strings.TrimPrefix(s.base, "http://")}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	srv := &http.Server{Handler: c}
	go func() { _ = srv.Serve(l) }()
	h.t.Cleanup(func() { _ = srv.Close() })
	c.url = "http://" + l.Addr().String()
	return c
}

func (c *control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path == "/kill" || r.URL.Path == "/restart" {
		c.cur.kill()
	}
	if r.URL.Path == "/start" || r.URL.Path == "/restart" {
		s, err := c.h.launch(c.addr, r.URL.Query()["env"]...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.cur = s
	}
	w.WriteHeader(http.StatusNoContent)
}

// node runs one browser suite and streams its output into the test log and a file.
func (h *harness) node(s *server, script string, extra ...string) error {
	h.t.Helper()
	cmd := exec.CommandContext(h.ctx, "node", filepath.Join(h.root, "web", "e2e", script+".mjs"))
	cmd.Dir = h.root
	cmd.Env = append(append(os.Environ(), "E2E_BASE="+s.base, "E2E_TOKEN_FILE="+h.tokenFile, "E2E_OUT="+h.out, "E2E_STUB="+h.stubURL, "E2E_STUB_KEY="+stubKey), extra...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	logFile, err := os.Create(filepath.Join(h.out, script+".log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			h.t.Log(sc.Text())
			fmt.Fprintln(logFile, sc.Text())
		}
	}()
	err = cmd.Run()
	_ = pw.Close()
	<-done
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%s failed; see %s", script, filepath.Join(h.out, script+".log"))
	}
	return err
}

func (h *harness) mustNode(s *server, script string, extra ...string) {
	h.t.Helper()
	if err := h.node(s, script, extra...); err != nil {
		h.t.Fatal(err)
	}
}
