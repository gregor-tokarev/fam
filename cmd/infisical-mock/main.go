// Command infisical-mock serves a small in-memory Infisical for development and demos of FAM.
//
//	go run ./cmd/infisical-mock --addr 127.0.0.1:8085 [--data data.json] [--rate-limit 60]
//
// Without --data it serves a demo project. See docs/development.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gregor-tokarev/fam/internal/infisical/mock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8085", "listen address")
	dataFile := flag.String("data", "", "JSON file with identities, tokens, and secrets (default: the demo data)")
	tokenTTL := flag.Duration("token-ttl", 2*time.Hour, "lifetime of an access token from a login")
	rate := flag.Int("rate-limit", 0, "secret reads per minute, 0 for no limit")
	latency := flag.Duration("latency", 0, "delay of each answer")
	quiet := flag.Bool("quiet", false, "do not log the requests")
	flag.Parse()

	data := mock.Demo()
	if *dataFile != "" {
		raw, err := os.ReadFile(*dataFile)
		if err != nil {
			fail(err)
		}
		data = mock.Data{}
		if err := json.Unmarshal(raw, &data); err != nil {
			fail(fmt.Errorf("%s: %w", *dataFile, err))
		}
	}
	opts := mock.Options{TokenTTL: *tokenTTL, RateLimit: *rate, Latency: *latency}
	if !*quiet {
		opts.Log = os.Stderr
	}
	srv := mock.New(data, opts)
	fmt.Fprintf(os.Stderr, "infisical-mock: http://%s (siteUrl in config.pkl)\n", *addr)
	if *dataFile == "" {
		fmt.Fprintln(os.Stderr, "infisical-mock: demo data: projectId \"fam-demo\", environments \"prod\" and \"shared\"")
		fmt.Fprintln(os.Stderr, "infisical-mock: INFISICAL_UNIVERSAL_AUTH_CLIENT_ID=fam-demo-client INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET=fam-demo-secret")
	}
	fmt.Fprintln(os.Stderr, "infisical-mock: request counts at /mock/stats")
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "infisical-mock:", err)
	os.Exit(1)
}
