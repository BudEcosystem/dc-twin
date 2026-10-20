// Command redfish-poll polls every BMC of a running estate the way a lifecycle manager does (each
// once per 30 s, spread evenly) and prints latency quantiles, then a burst of every BMC at once.
// Usage: dctwin credentials "?class=server" | go run ./scripts/perf/redfish-poll
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type cred struct{ Path, User, Password, Redfish string }

func main() {
	var creds []cred
	json.NewDecoder(os.Stdin).Decode(&creds)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, MaxIdleConnsPerHost: 2, MaxIdleConns: 4000}
	cl := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	var mu sync.Mutex
	var lat []time.Duration
	errs := 0
	poll := func(c cred) {
		req, _ := http.NewRequest("GET", "https://"+c.Redfish+"/redfish/v1/Systems/1", nil)
		req.SetBasicAuth(c.User, c.Password)
		t0 := time.Now()
		resp, err := cl.Do(req)
		d := time.Since(t0)
		mu.Lock()
		defer mu.Unlock()
		if err != nil || resp.StatusCode != 200 {
			errs++
			if resp != nil {
				resp.Body.Close()
			}
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lat = append(lat, d)
	}
	// the stack's poll: every BMC once per 30 s, spread evenly, for two minutes
	period := 30 * time.Second
	rounds := 4
	start := time.Now()
	var wg sync.WaitGroup
	for r := 0; r < rounds; r++ {
		for i, c := range creds {
			at := start.Add(time.Duration(r)*period + time.Duration(i)*period/time.Duration(len(creds)))
			time.Sleep(time.Until(at))
			wg.Add(1)
			go func(c cred) { defer wg.Done(); poll(c) }(c)
		}
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	q := func(p float64) time.Duration { return lat[int(float64(len(lat)-1)*p)] }
	fmt.Printf("BMCs %d, requests %d, errors %d, p50 %v, p90 %v, p99 %v, max %v\n", len(creds), len(lat), errs, q(0.5), q(0.9), q(0.99), lat[len(lat)-1])
	// a burst: every BMC at once (64 concurrent)
	lat, errs = nil, 0
	sem := make(chan struct{}, 64)
	t0 := time.Now()
	for _, c := range creds {
		sem <- struct{}{}
		wg.Add(1)
		go func(c cred) { defer wg.Done(); poll(c); <-sem }(c)
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Printf("burst: %d requests in %v (64 concurrent), errors %d, p50 %v, p99 %v\n", len(lat), time.Since(t0).Round(time.Millisecond), errs, q(0.5), q(0.99))
}
