package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testSignal = IRSignal{Freq: 38, Format: "us", Data: []int{2500, 1200, 600, 600}}

func setup(t *testing.T, remote http.HandlerFunc) (*App, string, *httptest.Server) {
	t.Helper()
	remo := httptest.NewServer(remote)
	t.Cleanup(remo.Close)
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(s, []*Device{{ID: "living", Name: "リビング", URL: remo.URL}})
	server := httptest.NewServer(a.handler())
	t.Cleanup(server.Close)
	return a, dir, server
}

func call(t *testing.T, server *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "nature-remo")
	req.Header.Set("Origin", server.URL)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	result, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, result
}

func saveButton(t *testing.T, server *httptest.Server) Button {
	t.Helper()
	b := Button{DeviceID: "living", Appliance: "照明", Name: "点灯", Signal: testSignal}
	code, result := call(t, server, "POST", "/api/buttons", b)
	if code != 201 {
		t.Fatalf("save: %d %s", code, result)
	}
	if err := json.Unmarshal(result, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCaptureSaveRestartSendDelete(t *testing.T) {
	var sends atomic.Int32
	a, dir, server := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" || r.Header.Get("X-Requested-With") == "" || r.Header.Get("Expect") != "" {
			t.Errorf("Remo protocol mismatch: %s %v", r.URL, r.Header)
		}
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(testSignal)
			return
		}
		sends.Add(1)
		var actual IRSignal
		json.NewDecoder(r.Body).Decode(&actual)
		if fmt.Sprint(actual) != fmt.Sprint(testSignal) || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect signal payload")
		}
		w.WriteHeader(200)
	})
	code, data := call(t, server, "POST", "/api/devices/living/capture", nil)
	if code != 200 || !bytes.Contains(data, []byte(`"freq":38`)) {
		t.Fatalf("capture: %d %s", code, data)
	}
	b := saveButton(t, server)
	restored, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.buttons) != 1 || restored.buttons[0].ID != b.ID {
		t.Fatal("button not persisted")
	}
	restarted := httptest.NewServer(newApp(restored, a.devices).handler())
	defer restarted.Close()
	code, _ = call(t, restarted, "POST", "/api/buttons/"+b.ID+"/send", nil)
	if code != 200 || sends.Load() != 1 {
		t.Fatalf("send: %d, calls: %d", code, sends.Load())
	}
	code, _ = call(t, restarted, "POST", "/api/buttons/"+b.ID+"/send", nil)
	if code != 429 || sends.Load() != 1 {
		t.Fatal("rapid duplicate send was not blocked")
	}
	code, _ = call(t, restarted, "DELETE", "/api/buttons/"+b.ID, nil)
	if code != 200 {
		t.Fatal("delete failed")
	}
	restored, err = openStore(dir)
	if err != nil || len(restored.buttons) != 0 {
		t.Fatal("delete not persisted")
	}
	code, _ = call(t, restarted, "POST", "/api/buttons/"+b.ID+"/send", nil)
	if code != 404 {
		t.Fatal("deleted button still callable")
	}
}

func TestCrossSiteAndUnvalidatedSignalsNeverReachRemo(t *testing.T) {
	var calls atomic.Int32
	_, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	b := saveButton(t, server)
	for _, tc := range []struct{ origin, requested, contentType, site string }{
		{"https://evil.example", "nature-remo", "application/json", ""},
		{"null", "nature-remo", "application/json", ""},
		{server.URL, "", "application/json", ""},
		{server.URL, "nature-remo", "text/plain", ""},
		{server.URL, "nature-remo", "application/json", "cross-site"},
	} {
		req, _ := http.NewRequest("POST", server.URL+"/api/buttons/"+b.ID+"/send", strings.NewReader("{}"))
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-Requested-With", tc.requested)
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set("Sec-Fetch-Site", tc.site)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("CSRF allowed: %+v status=%d", tc, res.StatusCode)
		}
	}
	for _, signal := range []IRSignal{{Freq: 29, Format: "us", Data: []int{1}}, {Freq: 38, Format: "ms", Data: []int{1}}, {Freq: 38, Format: "us"}, {Freq: 38, Format: "us", Data: []int{-1}}} {
		code, _ := call(t, server, "POST", "/api/buttons", Button{DeviceID: "living", Appliance: "照明", Name: "不正", Signal: signal})
		if code != 400 {
			t.Fatalf("invalid signal accepted: %v", signal)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unexpected physical action")
	}
}

func TestMissingReceivedSignalDoesNotMaskSendFailure(t *testing.T) {
	_, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	code, body := call(t, server, "POST", "/api/devices/living/capture", nil)
	if code != 404 || !bytes.Contains(body, []byte("実物の赤外線リモコン")) || !bytes.Contains(body, []byte(`"code":"no_signal"`)) {
		t.Fatalf("capture must explain how to receive a signal: %d %s", code, body)
	}
	b := saveButton(t, server)
	code, body = call(t, server, "POST", "/api/buttons/"+b.ID+"/send", nil)
	if code != 502 || !bytes.Contains(body, []byte("送信結果を確認できません")) {
		t.Fatalf("send failure must remain an uncertain result: %d %s", code, body)
	}
}

func TestCapturedZeroIntervalsArePreserved(t *testing.T) {
	want := IRSignal{Freq: 38, Format: "us", Data: []int{500, 65535, 0, 65535, 0, 500}}
	_, dir, server := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(want)
			return
		}
		var got IRSignal
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("signal changed during send: %v, error: %v", got, err)
		}
	})
	code, body := call(t, server, "POST", "/api/devices/living/capture", nil)
	if code != 200 {
		t.Fatalf("capture: %d %s", code, body)
	}
	var received IRSignal
	if err := json.Unmarshal(body, &received); err != nil {
		t.Fatal(err)
	}
	code, body = call(t, server, "POST", "/api/buttons", Button{DeviceID: "living", Appliance: "テスト家電", Name: "テスト操作", Signal: received})
	if code != 201 {
		t.Fatalf("save: %d %s", code, body)
	}
	stored, err := openStore(dir)
	if err != nil || len(stored.buttons) != 1 {
		t.Fatalf("restore: %v", err)
	}
	code, body = call(t, server, "POST", "/api/buttons/"+stored.buttons[0].ID+"/send", nil)
	if code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	if (IRSignal{Freq: 38, Format: "us", Data: []int{0, 0}}).validate() == nil {
		t.Fatal("all-zero signal must be rejected")
	}
}

func TestRemoteFailureIsNotRetriedAndRedirectIsRejected(t *testing.T) {
	for _, mode := range []string{"error", "timeout", "redirect", "invalid", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			var calls, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer target.Close()
			a, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "error":
					w.WriteHeader(500)
				case "timeout":
					time.Sleep(50 * time.Millisecond)
				case "redirect":
					http.Redirect(w, r, target.URL, 307)
				case "invalid":
					io.WriteString(w, `{"freq":38,"format":"ms","data":[1]}`)
				case "oversized":
					io.WriteString(w, strings.Repeat(" ", maxBody+1))
				}
			})
			a.client.Timeout = 20 * time.Millisecond
			b := saveButton(t, server)
			path := "/api/buttons/" + b.ID + "/send"
			if mode == "invalid" || mode == "oversized" {
				path = "/api/devices/living/capture"
			}
			code, _ := call(t, server, "POST", path, nil)
			if code != 502 || calls.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("status=%d calls=%d redirects=%d", code, calls.Load(), redirected.Load())
			}
		})
	}
}

func TestConcurrentDeviceOperationsAreRejected(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	_, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-release })
	b := saveButton(t, server)
	done := make(chan struct{})
	go func() { defer close(done); call(t, server, "POST", "/api/buttons/"+b.ID+"/send", nil) }()
	<-started
	code, _ := call(t, server, "POST", "/api/devices/living/capture", nil)
	close(release)
	<-done
	if code != 409 {
		t.Fatalf("concurrent device operation accepted: %d", code)
	}
}

func TestSaveFailurePreservesDataAndCorruptionFailsStartup(t *testing.T) {
	a, dir, server := setup(t, func(w http.ResponseWriter, r *http.Request) {})
	b := saveButton(t, server)
	a.store.path = filepath.Join(dir, "missing", "buttons.json")
	code, _ := call(t, server, "DELETE", "/api/buttons/"+b.ID, nil)
	if code != 500 || len(a.store.buttons) != 1 {
		t.Fatal("failed write changed memory")
	}
	a.store.path = filepath.Join(dir, "buttons.json")
	if err := os.WriteFile(a.store.path, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir); err == nil {
		t.Fatal("corruption silently discarded")
	}
}

func TestInputLimitsDuplicateAndUnknownDevice(t *testing.T) {
	_, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) {})
	b := saveButton(t, server)
	b.ID = ""
	code, _ := call(t, server, "POST", "/api/buttons", b)
	if code != 409 {
		t.Fatal("duplicate accepted")
	}
	b.DeviceID = "missing"
	code, _ = call(t, server, "POST", "/api/buttons", b)
	if code != 400 {
		t.Fatal("unknown device accepted")
	}
	code, _ = call(t, server, "POST", "/api/devices/missing/capture", nil)
	if code != 404 {
		t.Fatal("unknown capture device accepted")
	}
	code, _ = call(t, server, "POST", "/api/buttons", map[string]string{"name": strings.Repeat("x", maxBody+1)})
	if code != 400 {
		t.Fatal("oversized request accepted")
	}
}

func TestDeviceConfig(t *testing.T) {
	for _, raw := range []string{`[null]`, `[{"id":"a","name":"A","url":"https://remo.local"}]`, `[{"id":"a","name":"A","url":"http://user:pass@remo.local"}]`, `[{"id":"a","name":"A","url":"http://remo.local/messages"}]`, `[{"id":"a","name":"A","url":"http://remo.local"},{"id":"a","name":"B","url":"http://remo2.local"}]`} {
		if _, err := parseDevices(raw); err == nil {
			t.Fatalf("bad config accepted: %s", raw)
		}
	}
	devices, err := parseDevices(`[{"id":"living","name":"リビング","url":"http://192.168.1.20/"}]`)
	if err != nil || devices[0].URL != "http://192.168.1.20" {
		t.Fatal("valid config rejected")
	}
}

func TestStaticAssetsAndHealthDoNotContactRemo(t *testing.T) {
	_, _, server := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("health or static request contacted Remo") })
	for _, path := range []string{"/", "/app.js", "/style.css", "/healthz", "/api/state"} {
		res, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("static response: %s %d", path, res.StatusCode)
		}
	}
}
