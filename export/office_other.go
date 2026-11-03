//go:build !windows

package export

import (
	"errors"
	"time"
)

func newOffice(string, time.Duration) (officeEngine, error) {
	return nil, errors.New("WPS / Word COM automation requires Windows")
}
