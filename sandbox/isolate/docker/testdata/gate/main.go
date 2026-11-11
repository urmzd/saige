// Command gate serves an egress.Gate on :3128 for the docker egress test.
// It grants one policy, prints its token, and prints every decision.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/urmzd/saige/sandbox/isolate/egress"
)

func main() {
	gate := egress.New(egress.Config{
		AllowNonPublic: true, // the test's upstreams are on a private network
		Resolve: func(_ context.Context, c egress.Credential) (string, error) {
			return "real-" + c.Name, nil
		},
		OnDecision: func(d egress.Decision) { fmt.Printf("DECISION %s %s %v\n", d.Method, d.Host, d.Allowed) },
	})
	token, err := gate.Grant("lease-1", egress.Policy{Allow: []egress.Rule{{
		Host:   "upstream",
		Ports:  []int{8080},
		Inject: []egress.Injection{{Header: "Authorization", Placeholder: "PLACEHOLDER", Credential: "gh"}},
	}}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("TOKEN %s\n", token)
	log.Fatal(http.ListenAndServe(":3128", gate)) //nolint:gosec // test helper
}
