package docker

import (
	"reflect"
	"testing"

	mclient "github.com/moby/moby/client"
)

func TestContainerListOptions(t *testing.T) {
	// No statuses: the compatibility path, all alone decides the listing.
	opts := containerListOptions(true, nil)
	if !opts.All {
		t.Error("All must stay true when no statuses are given")
	}
	if len(opts.Filters) != 0 {
		t.Errorf("empty statuses must not build a status filter, got %v", opts.Filters)
	}

	// Tagged-only listing.
	opts = containerListOptions(false, nil)
	if opts.All {
		t.Error("All must stay false when no statuses are given")
	}

	// A status set becomes an OR-combined "status" filter term; the All flag
	// is whatever the caller requested (the daemon honours status filters
	// regardless of it).
	opts = containerListOptions(false, []string{"running", "exited"})
	if opts.All {
		t.Error("All must stay false when the caller requested the tagged listing")
	}
	if got := mclient.Filters(opts.Filters); !reflect.DeepEqual(
		map[string]map[string]bool(got)["status"], map[string]bool{"running": true, "exited": true},
	) {
		t.Errorf("status term = %v, want {running, exited}", got["status"])
	}
}
