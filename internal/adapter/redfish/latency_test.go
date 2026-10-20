package redfish_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"
)

func TestLatency(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/a01/compute-tray-07")
	addr, _, _ := a.BMC(d.Path)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, MaxIdleConnsPerHost: 4}}
	for _, p := range []string{"/redfish/v1", "/redfish/v1/Chassis", "/redfish/v1/Chassis/Chassis_0", "/redfish/v1/Systems/System_0", "/redfish/v1/Chassis/HGX_GPU_0/Sensors"} {
		var ds []time.Duration
		for i := 0; i < 30; i++ {
			req, _ := http.NewRequest("GET", "https://"+addr+p, nil)
			req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
			t0 := time.Now()
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-45s p50 %v p99 %v", p, ds[15], ds[29])
	}
}
