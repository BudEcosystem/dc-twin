package bluefield

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// maxImageBytes is the captured UpdateService's MaxImageSizeBytes (for pushed images; a BFB
// fetched by SimpleUpdate streams to the DPU and is not held by the BMC).
const maxImageBytes = 209715200

// task is an update task this BMC runs itself: a BFB push. Its id is numbered apart from the
// engine's tasks (which the engine numbers from 1), so the two never collide in TaskService.
type task struct {
	id       string
	state    string // Running, Completed, Exception
	percent  int
	started  time.Time
	ended    time.Time
	messages []string
	payload  map[string]any
}

func (f *front) hasTask(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		if t.id == id {
			return true
		}
	}
	return false
}

func (f *front) getTask(w http.ResponseWriter, r *http.Request, path string, p principal) {
	id := path[strings.LastIndex(path, "/")+1:]
	f.mu.Lock()
	var doc map[string]any
	for _, t := range f.tasks {
		if t.id != id {
			continue
		}
		status := "OK"
		if t.state == "Exception" {
			status = "Critical"
		}
		var msgs []any
		for i, m := range t.messages {
			mid := "TaskEvent.1.0.TaskProgressChanged"
			switch {
			case i == len(t.messages)-1 && t.state == "Completed":
				mid = "TaskEvent.1.0.TaskCompletedOK"
			case i == len(t.messages)-1 && t.state == "Exception":
				mid = "TaskEvent.1.0.TaskAborted"
			}
			msgs = append(msgs, map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": mid, "Message": m, "MessageArgs": []any{id}, "MessageSeverity": map[bool]string{true: "Critical", false: "OK"}[t.state == "Exception" && i == len(t.messages)-1]})
		}
		doc = map[string]any{"@odata.id": path, "@odata.type": "#Task.v1_4_3.Task", "Id": t.id, "Name": "Task " + t.id,
			"TaskState": t.state, "TaskStatus": status, "PercentComplete": t.percent, "StartTime": t.started.UTC().Format(time.RFC3339),
			"Messages": msgs, "Payload": t.payload, "TaskMonitor": "/redfish/v1/TaskService/Tasks/" + t.id + "/Monitor"}
		if !t.ended.IsZero() {
			doc["EndTime"] = t.ended.UTC().Format(time.RFC3339)
		}
	}
	f.mu.Unlock()
	f.log(r, p, path, "task.read", http.StatusOK)
	writeJSON(w, http.StatusOK, doc)
}

func (f *front) newTask(payload map[string]any) *task {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskSeq++
	t := &task{id: strconv.Itoa(f.taskSeq), state: "Running", started: f.a.eng.Now(), payload: payload, messages: []string{"The task with Id '" + strconv.Itoa(f.taskSeq) + "' has started."}}
	f.tasks = append(f.tasks, t)
	return t
}

// taskUpdate changes a task under the front's lock; fn must not take the lock itself.
func (f *front) taskUpdate(t *task, fn func(*task)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(t)
}

func anotherUpdate(w http.ResponseWriter) {
	// bmcweb's answer while an update runs; DPF waits on this text
	writeError(w, http.StatusBadRequest, "Base.1.15.ResourceInUse", "Another update is in progress")
}

// simpleUpdate is UpdateService.SimpleUpdate. A BFB for the DPU's Arm (target DPU_OS, or the
// BlueField-4 OS image) is the BMC's own job: it fetches ImageURI and streams it to the DPU over
// rshim, and its task is that transfer. Any other target is a firmware image the engine stages.
func (f *front) simpleUpdate(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct {
		ImageURI, TransferProtocol, Username, Password string
		Targets                                        []string
	}
	if err := decode(r, &body); err != nil || body.ImageURI == "" {
		f.log(r, p, path, "firmware.update.simple", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyMissing", "ImageURI is required.")
		return
	}
	toOS := false
	for _, t := range body.Targets {
		t = "/" + strings.TrimPrefix(t, "/")
		if strings.HasSuffix(t, "/FirmwareInventory/DPU_OS") || strings.HasSuffix(t, "/FirmwareInventory/BlueField_OS_Image_CPU_0") {
			toOS = true
		}
	}
	if !toOS {
		// a firmware image for the BMC or the NIC: the engine stages it, as for any BMC
		var comps []string
		for _, t := range body.Targets {
			switch t[strings.LastIndex(t, "/")+1:] {
			case "BMC_Firmware":
				comps = append(comps, "bmc")
			case "DPU_NIC":
				comps = append(comps, "nic")
			}
		}
		version := body.ImageURI[strings.LastIndex(body.ImageURI, "/")+1:]
		f.stageFirmware(w, r, path, p, "firmware.update.simple", comps, version)
		return
	}
	proto := strings.ToUpper(body.TransferProtocol)
	uri := body.ImageURI
	if !strings.Contains(uri, "://") {
		switch proto {
		case "HTTP", "HTTPS":
			uri = strings.ToLower(proto) + "://" + uri
		case "":
			uri = "https://" + uri
		default:
			f.log(r, p, path, "firmware.update.simple", http.StatusBadRequest)
			writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterNotSupported", "TransferProtocol "+body.TransferProtocol+" is not supported for a BFB; use HTTP or HTTPS.")
			return
		}
	}
	f.mu.Lock()
	busy := f.busy
	if !busy {
		f.busy = true
	}
	f.mu.Unlock()
	if busy {
		f.log(r, p, path, "firmware.update.simple", http.StatusBadRequest)
		anotherUpdate(w)
		return
	}
	t := f.newTask(map[string]any{"TargetUri": path, "HttpOperation": "POST", "JsonBody": "{\"ImageURI\": \"…\", \"Targets\": [\"DPU_OS\"]}"})
	seq := f.log(r, p, path, "firmware.update.simple", http.StatusAccepted)
	f.commit("dpu.bfb", nil, "transferring", "BFB push to the DPU over rshim (task "+t.id+")", seq)
	go f.pushBFB(t, uri, seq)
	loc := "/redfish/v1/TaskService/Tasks/" + t.id
	w.Header().Set("Location", loc)
	writeJSON(w, http.StatusAccepted, map[string]any{"@odata.id": loc, "@odata.type": "#Task.v1_4_3.Task", "Id": t.id, "TaskState": "Running", "TaskStatus": "OK"})
}

// pushBFB fetches the image and reads it as the DPU would receive it over rshim. When the whole
// stream has gone through, the BMC resets the DPU's Arm into it (the boot stream starts the
// installer). At model fidelity no installer runs; a BlueField at vm fidelity has an Arm
// (arm.go), for which the stream is saved as it arrives, and the reset boots its installer.
func (f *front) pushBFB(t *task, uri string, cause uint64) {
	defer func() {
		f.mu.Lock()
		f.busy = false
		f.mu.Unlock()
	}()
	fail := func(msg string) {
		f.taskUpdate(t, func(t *task) {
			t.state, t.ended = "Exception", f.a.eng.Now()
			t.messages = append(t.messages, msg)
		})
		f.commit("dpu.bfb", "transferring", "failed", msg, cause)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		fail("Invalid ImageURI: " + err.Error())
		return
	}
	resp, err := f.a.fetch.Do(req)
	if err != nil {
		fail("The image could not be transferred from " + redactURI(uri) + ": " + err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("The image server answered %s for %s.", resp.Status, redactURI(uri)))
		return
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		msg := "Image server certificate " + resp.TLS.PeerCertificates[0].Subject.CommonName + ": " + f.chainVerdict(resp.TLS.PeerCertificates)
		f.taskUpdate(t, func(t *task) { t.messages = append(t.messages, msg) })
	}
	total := resp.ContentLength
	last := -1
	body := io.Reader(resp.Body)
	var save *os.File
	var saved *bufio.Writer
	if f.arm != nil {
		part := filepath.Join(f.arm.dir, "stream.part")
		if save, err = os.Create(part); err != nil {
			fail("The DPU could not take the image: " + err.Error())
			return
		}
		saved = bufio.NewWriterSize(save, 1<<20)
		body = io.TeeReader(resp.Body, saved)
		defer func() {
			if save != nil {
				save.Close()
				_ = os.Remove(part)
			}
		}()
	}
	facts, err := readBFB(body, func(n int64) {
		if total <= 0 {
			return
		}
		pct := int(n * 100 / total)
		if pct > 99 {
			pct = 99
		}
		if pct/10 != last {
			last = pct / 10
			f.taskUpdate(t, func(t *task) { t.percent = pct })
		}
	})
	if err != nil {
		fail("The BFB was not accepted: " + err.Error())
		return
	}
	if save != nil {
		part, whole := save.Name(), filepath.Join(f.arm.dir, "stream.bfb")
		err := saved.Flush()
		if cerr := save.Close(); err == nil {
			err = cerr
		}
		save = nil
		if err == nil {
			err = os.Rename(part, whole)
		}
		if err != nil {
			_ = os.Remove(part)
			fail("The DPU could not take the image: " + err.Error())
			return
		}
		f.arm.pushed(whole, facts.summary(), facts.versions)
	}
	f.taskUpdate(t, func(t *task) {
		t.state, t.percent, t.ended = "Completed", 100, f.a.eng.Now()
		t.messages = append(t.messages, "BFB transferred to the DPU: "+facts.summary())
	})
	f.mu.Lock()
	f.bfb, f.armInBFB = facts, true
	f.mu.Unlock()
	f.commit("dpu.bfb", "transferring", "transferred", facts.summary(), cause)
	// the boot stream resets the Arm into the BFB's installer
	_ = f.a.eng.Power(engine.Request{Path: f.dev.Path, Canonical: "power.restart", Protocol: "rshim", Target: "boot stream",
		Source: schema.EventSource{Class: "behaviour", Principal: "bluefield"}})
}

// chainVerdict says whether an image server's chain verifies against the Truststore.
func (f *front) chainVerdict(chain []*x509.Certificate) string {
	f.mu.Lock()
	roots := x509.NewCertPool()
	for _, t := range f.trust {
		roots.AddCert(t.cert)
	}
	f.mu.Unlock()
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter}); err != nil {
		return "does not chain to the Truststore (not required for SimpleUpdate)"
	}
	return "chains to the Truststore"
}

// redactURI drops a query string (DPF's registry paths carry none, but an image URI may carry a
// token).
func redactURI(u string) string {
	if i := strings.Index(u, "?"); i >= 0 && strings.Contains(u[i:], "=") {
		return u[:i] + "?…"
	}
	return u
}

// pushUpdate is the HttpPushUri (and the legacy POST to UpdateService itself): the body is a
// firmware package for the BMC, which the engine stages under the version its image carries.
func (f *front) pushUpdate(w http.ResponseWriter, r *http.Request, path string, p principal) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxImageBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.15.GeneralError", err.Error())
		return
	}
	f.pldmUpdate(w, r, path, p, "firmware.update.push", b)
}

// multipartUpdate is the MultipartHttpPushUri: an UpdateFile part and UpdateParameters.
func (f *front) multipartUpdate(w http.ResponseWriter, r *http.Request, path string, p principal) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		f.log(r, p, path, "firmware.update.multipart", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.MalformedJSON", "A multipart/form-data body is required.")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var file []byte
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "Base.1.15.GeneralError", err.Error())
			return
		}
		if part.FormName() == "UpdateFile" {
			file, err = io.ReadAll(io.LimitReader(part, maxImageBytes+1))
			if err != nil {
				writeError(w, http.StatusBadRequest, "Base.1.15.GeneralError", err.Error())
				return
			}
		}
	}
	if file == nil {
		f.log(r, p, path, "firmware.update.multipart", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterMissing", "UpdateFile is required.")
		return
	}
	f.pldmUpdate(w, r, path, p, "firmware.update.multipart", file)
}

func (f *front) pldmUpdate(w http.ResponseWriter, r *http.Request, path string, p principal, canonical string, b []byte) {
	if len(b) > maxImageBytes {
		f.log(r, p, path, canonical, http.StatusRequestEntityTooLarge)
		writeError(w, http.StatusRequestEntityTooLarge, "Base.1.15.PayloadTooLarge", "The image exceeds MaxImageSizeBytes.")
		return
	}
	pkg, err := parsePLDM(b)
	if err != nil {
		f.log(r, p, path, canonical, http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterValueFormatError", "The image is not a firmware package this BMC accepts: "+err.Error())
		return
	}
	version := pkg.bmcVersion
	if version == "" {
		version = pkg.version
	}
	// an upload still arriving when the BMC goes down is cut off with it: DPF resets the BMC when
	// its task completes, reads the version again before the BMC has gone down, finds it old and
	// pushes again (found by a run, compat/dpf)
	if f.down(w) {
		return
	}
	f.mu.Lock()
	busy := f.busy
	f.mu.Unlock()
	if busy {
		f.log(r, p, path, canonical, http.StatusBadRequest)
		anotherUpdate(w)
		return
	}
	f.stageFirmware(w, r, path, p, canonical, []string{"bmc"}, version)
}

// stageFirmware hands a firmware image to the engine, which logs the request, runs the task and
// stages (or applies) the version per the profile's applyOn.
func (f *front) stageFirmware(w http.ResponseWriter, r *http.Request, path string, p principal, canonical string, comps []string, version string) {
	t, err := f.a.eng.UpdateFirmware(engine.Request{Path: f.dev.Path, Canonical: canonical, Source: src(r, p), Protocol: "redfish", Method: r.Method, Target: path}, comps, version)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Base.1.15.ServiceTemporarilyUnavailable", err.Error())
		return
	}
	loc := "/redfish/v1/TaskService/Tasks/" + t.ID
	w.Header().Set("Location", loc)
	writeJSON(w, http.StatusAccepted, map[string]any{"@odata.id": loc, "@odata.type": "#Task.v1_4_3.Task", "Id": t.ID, "TaskState": "Running", "TaskStatus": "OK",
		"Messages": []any{map[string]any{"MessageId": "TaskEvent.1.0.TaskStarted", "Message": "The task with Id '" + t.ID + "' has started.", "MessageArgs": []any{t.ID}}}})
}
