package runtime

import (
	"strconv"
	"time"

	"github.com/urmzd/saige/flow/event"
)

// DefaultTenant is the tenant a run's events carry when its trigger names
// none. The bus requires a tenant on every event.
const DefaultTenant = "default"

// busTenant is the tenant a run's events are published under.
func busTenant(tenant string) string {
	if tenant == "" {
		return DefaultTenant
	}
	return tenant
}

// controlChannel is where requests of one verb for a run are published. A
// run id that could not be a run's is refused.
func controlChannel(verb, tenant, runID string) (string, error) {
	if err := checkSegment("run id", runID); err != nil {
		return "", err
	}
	return event.ControlChannel(busTenant(tenant), verb, runID), nil
}

// requestID names one stop or compact request, so the bus keeps each request
// once and a listener that reads it again after resubscribing knows it.
func requestID(verb, runID string, at time.Time) string {
	return verb + ":" + runID + ":" + strconv.FormatInt(at.UnixNano(), 10)
}
