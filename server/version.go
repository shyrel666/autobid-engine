package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/autobid/autobid-engine/internal/buildinfo"
)

// Initialized once per process, before database recovery and component probes.
var processStartedAt = time.Now().UTC()

func (a *API) runtimeVersion(w http.ResponseWriter, r *http.Request) {
	checksum, err := a.pg.AppliedSchemaChecksum(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("无法读取数据库版本，请检查数据库连接"))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		buildinfo.Version
		SchemaChecksum string    `json:"schema_checksum"`
		StartedAt      time.Time `json:"started_at"`
	}{buildinfo.Current(), checksum, processStartedAt})
}
