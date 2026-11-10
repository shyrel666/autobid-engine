package e2e

import (
	"os"
	"testing"
	"time"
)

// TestS7Flow: one business user, browser only, from a new project to a strict final draft.
func TestS7Flow(t *testing.T) {
	h := newHarness(t, "flow", 35*time.Minute)
	h.startStub("0.1", "2")
	h.mustNode(h.start(), "s7-flow")
}

// TestS7RealTender: a real public tender document, from upload to an approved outline.
// E2E_REAL_TENDER points at a development-set file that stays out of the repository.
func TestS7RealTender(t *testing.T) {
	file := os.Getenv("E2E_REAL_TENDER")
	if file == "" {
		t.Skip("E2E_REAL_TENDER is not set: real tender acceptance was not run")
	}
	h := newHarness(t, "real", 20*time.Minute)
	h.startStub("0.1", "2")
	h.mustNode(h.start(), "s7-real", "E2E_REAL_TENDER="+file)
}

// TestS7ExistingSuites keeps the browser suites from earlier stages running against a real process.
func TestS7ExistingSuites(t *testing.T) {
	h := newHarness(t, "existing", 30*time.Minute)
	// Long enough that work is still in flight when live-run restarts the server.
	h.startStub("3", "60")
	s := h.start()
	t.Run("scenarios", func(t *testing.T) { h.mustNode(s, "scenarios", "E2E_SCENARIOS="+os.Getenv("E2E_SCENARIOS")) })
	ctl := h.control(s)
	restart := `node -e "fetch('` + ctl.url + `/restart',{method:'POST'}).then(r=>process.exit(r.ok?0:1))"`
	t.Run("live-run", func(t *testing.T) { h.mustNode(s, "live-run", "E2E_RESTART="+restart) })
}

// TestS7Agent: the scripted model drives the real Planner and Worker through the Skills.
func TestS7Agent(t *testing.T) {
	h := newHarness(t, "agent", 20*time.Minute)
	h.startStub("0.1", "2")
	h.mustNode(h.start(), "s7-agent")
}

// TestS7Capabilities restarts the server with each capability off and reads what the page says.
func TestS7Capabilities(t *testing.T) {
	h := newHarness(t, "capabilities", 25*time.Minute)
	h.startStub("0.1", "2")
	s := h.start()
	h.mustNode(s, "s7-capabilities", "E2E_CONTROL="+h.control(s).url)
}

// TestS7Recovery kills the server while durable jobs are running.
func TestS7Recovery(t *testing.T) {
	h := newHarness(t, "recovery", 30*time.Minute)
	h.startStub("0.1", "2")
	s := h.start(recoveryEnv(h)...)
	h.mustNode(s, "s7-recovery", "E2E_CONTROL="+h.control(s).url)
}

// recoveryEnv shortens leases so a killed run is taken over within the test, and points the
// semantic index at the scripted embedding endpoint so a build can be interrupted.
func recoveryEnv(h *harness) []string {
	return []string{
		"AUTOBID_OFFICE_TIMEOUT_SECONDS=20", "AUTOBID_MMDC_TIMEOUT_SECONDS=20",
		"AUTOBID_EMBEDDING_URL=" + h.stubURL + "/v1/embeddings", "AUTOBID_EMBEDDING_MODEL=s7-scripted", "AUTOBID_EMBEDDING_REVISION=s7",
		"AUTOBID_EMBEDDING_DIMENSION=3", "AUTOBID_EMBEDDING_FORMAT=compatible", "AUTOBID_EMBEDDING_MAX_BATCH=1",
	}
}
