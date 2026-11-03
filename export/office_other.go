//go:build !windows

package export

import (
	"fmt"
	"time"
)

func newOffice(string, time.Duration) (officeEngine, error) {
	return nil, fmt.Errorf("%w: WPS / Word COM automation requires Windows", ErrNotInstalled)
}
