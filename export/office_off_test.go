package export

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/toolhealth"
)

func TestOfficeExplicitlyOff(t *testing.T) {
	// Even a cancelled context must return configuration status, without trying COM.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, value := range []string{"off", " OFF "} {
		o, err := DetectOffice(ctx, OfficeOptions{Engine: value})
		if o != nil || !errors.Is(err, ErrOfficeUnavailable) || !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("off: office=%v err=%v", o, err)
		}
		h := toolhealth.New()
		recordProbe(h, toolhealth.Office, toolhealth.Result{}, time.Duration(0), err)
		if got := h.Get(toolhealth.Office).State; got != toolhealth.Unconfigured {
			t.Fatalf("off runtime state = %s, want unconfigured", got)
		}
	}
}
