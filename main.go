package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

const maxBody = 256 * 1024

var errNoIRSignal = errors.New("Remo returned no received IR signal")

type IRSignal struct {
	Freq   int    `json:"freq"`
	Data   []int  `json:"data"`
	Format string `json:"format"`
}

func (s IRSignal) validate() error {
	if s.Freq < 30 || s.Freq > 80 || s.Format != "us" || len(s.Data) == 0 || len(s.Data) > 16000 {
		return errors.New("信号は freq: 30〜80、format: us、data: 1〜16000 個の整数で指定してください")
	}
	hasDuration := false
	for _, n := range s.Data {
		// Remo mini の実機は長い区間などで 0 を含む信号を返す。順序・値を保持する。
		if n < 0 || n > 10000000 {
			return errors.New("信号の各パルスは 0〜10000000 µs で指定してください")
		}
		hasDuration = hasDuration || n > 0
	}
	if !hasDuration {
		return errors.New("信号には 0 より大きいパルスが必要です")
	}
	return nil
}

type Device struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	mu          sync.Mutex
	lastAttempt time.Time
}

type Button struct {
	ID        string   `json:"id"`
	DeviceID  string   `json:"deviceId"`
	Appliance string   `json:"appliance"`
	Name      string   `json:"name"`
	Signal    IRSignal `json:"signal"`
}

type Store struct {
	mu      sync.Mutex
	path    string
	buttons []Button
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "buttons.json"), buttons: []Button{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.persist(s.buttons)
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.buttons); err != nil {
		return nil, fmt.Errorf("保存データを読み込めません: %w", err)
	}
	seen := map[string]bool{}
	for _, button := range s.buttons {
		if button.ID == "" || seen[button.ID] || button.Signal.validate() != nil {
			return nil, errors.New("保存データが不正です。バックアップから復旧してください")
		}
		seen[button.ID] = true
	}
	if s.buttons == nil {
		s.buttons = []Button{}
	}
	return s, nil
}

// 一時ファイルを同期してから置換し、書き込み失敗時にはメモリ上の状態も維持する。
func (s *Store) persist(buttons []Button) error {
	b, err := json.MarshalIndent(buttons, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".buttons-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func parseDevices(raw string) ([]*Device, error) {
	devices := []*Device{}
	if raw == "" {
		return devices, nil
	}
	if err := json.Unmarshal([]byte(raw), &devices); err != nil {
		return nil, err
	}
	if devices == nil {
		devices = []*Device{}
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if d == nil || !idPattern.MatchString(d.ID) || seen[d.ID] || strings.TrimSpace(d.Name) == "" {
			return nil, errors.New("REMO_DEVICES の id/name が不正または重複しています")
		}
		u, err := url.Parse(d.URL)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("Remo の URL は http://IPアドレス[:ポート] で指定してください")
		}
		d.URL = strings.TrimRight(d.URL, "/")
		seen[d.ID] = true
	}
	return devices, nil
}

type App struct {
	store   *Store
	devices []*Device
	client  *http.Client
}

func newApp(store *Store, devices []*Device) *App {
	return &App{store: store, devices: devices, client: &http.Client{
		Timeout: 5 * time.Second,
		// LAN 通信を環境変数の HTTP proxy に送らず、別ホストへの転送も拒否する。
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (a *App) device(id string) *Device {
	for _, d := range a.devices {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	reader := http.MaxBytesReader(w, r.Body, maxBody)
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		fail(w, 400, "JSON の形式またはサイズが不正です")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, 400, "JSON は一つのオブジェクトで指定してください")
		return false
	}
	return true
}

func (a *App) remo(ctx context.Context, device *Device, method string, s *IRSignal) (*IRSignal, error) {
	var body io.Reader
	if s != nil {
		b, _ := json.Marshal(s)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, device.URL+"/messages", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Requested-With", "nature-remo")
	req.Header.Set("Accept", "application/json")
	if s != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if method == http.MethodGet && res.StatusCode == http.StatusNotFound {
		return nil, errNoIRSignal
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Remo HTTP %d", res.StatusCode)
	}
	if method == http.MethodPost {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		return nil, errors.New("Remo の応答サイズが不正です")
	}
	var received IRSignal
	if err := json.Unmarshal(b, &received); err != nil {
		return nil, err
	}
	return &received, received.validate()
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		respond(w, 200, map[string]any{"devices": a.devices, "buttons": a.store.buttons})
	})
	mux.HandleFunc("POST /api/devices/{id}/capture", func(w http.ResponseWriter, r *http.Request) {
		d := a.device(r.PathValue("id"))
		if d == nil {
			fail(w, 404, "Remo が設定されていません")
			return
		}
		if !d.mu.TryLock() {
			fail(w, 409, "この Remo は通信中です")
			return
		}
		defer d.mu.Unlock()
		s, err := a.remo(r.Context(), d, http.MethodGet, nil)
		if errors.Is(err, errNoIRSignal) {
			respond(w, 404, map[string]string{"code": "no_signal", "error": "受信信号を取得できませんでした。実物の赤外線リモコンを Remo に向けて押し、すぐにもう一度取得してください"})
			return
		}
		if err != nil {
			log.Printf("capture device=%s: %v", d.ID, err)
			fail(w, 502, "信号を取得できません。Remo の接続とリモコンの送信を確認してください")
			return
		}
		respond(w, 200, s)
	})
	mux.HandleFunc("POST /api/buttons", func(w http.ResponseWriter, r *http.Request) {
		var b Button
		if !decode(w, r, &b) {
			return
		}
		b.Name, b.Appliance = strings.TrimSpace(b.Name), strings.TrimSpace(b.Appliance)
		if b.ID != "" || a.device(b.DeviceID) == nil || b.Name == "" || len([]rune(b.Name)) > 60 || b.Appliance == "" || len([]rune(b.Appliance)) > 60 {
			fail(w, 400, "Remo、家電名、ボタン名（各60文字以内）を確認してください")
			return
		}
		if err := b.Signal.validate(); err != nil {
			fail(w, 400, err.Error())
			return
		}
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			fail(w, 500, "ID を生成できません")
			return
		}
		b.ID = hex.EncodeToString(id)
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		if len(a.store.buttons) >= 500 {
			fail(w, 409, "登録上限は500ボタンです")
			return
		}
		for _, existing := range a.store.buttons {
			if existing.DeviceID == b.DeviceID && existing.Appliance == b.Appliance && existing.Name == b.Name {
				fail(w, 409, "同じ家電に同名のボタンが登録されています")
				return
			}
		}
		next := append(append([]Button{}, a.store.buttons...), b)
		if err := a.store.persist(next); err != nil {
			log.Printf("save: %v", err)
			fail(w, 500, "保存できません。ストレージを確認してください")
			return
		}
		a.store.buttons = next
		respond(w, 201, b)
	})
	mux.HandleFunc("DELETE /api/buttons/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		next := []Button{}
		found := false
		for _, b := range a.store.buttons {
			if b.ID == r.PathValue("id") {
				found = true
			} else {
				next = append(next, b)
			}
		}
		if !found {
			fail(w, 404, "ボタンが見つかりません")
			return
		}
		if err := a.store.persist(next); err != nil {
			log.Printf("delete: %v", err)
			fail(w, 500, "削除を保存できません")
			return
		}
		a.store.buttons = next
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/buttons/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		var button *Button
		a.store.mu.Lock()
		for _, b := range a.store.buttons {
			if b.ID == r.PathValue("id") {
				copy := b
				button = &copy
				break
			}
		}
		a.store.mu.Unlock()
		if button == nil {
			fail(w, 404, "ボタンが見つかりません")
			return
		}
		d := a.device(button.DeviceID)
		if d == nil {
			fail(w, 409, "保存先の Remo が現在の設定にありません")
			return
		}
		if !d.mu.TryLock() {
			fail(w, 409, "この Remo は通信中です")
			return
		}
		defer d.mu.Unlock()
		if time.Since(d.lastAttempt) < time.Second {
			fail(w, 429, "連続送信を防ぐため、少し待ってください")
			return
		}
		d.lastAttempt = time.Now()
		// タイムアウトでも実機に届いている可能性があるため、自動再送しない。
		if _, err := a.remo(r.Context(), d, http.MethodPost, &button.Signal); err != nil {
			log.Printf("send device=%s button=%s: %v", d.ID, button.ID, err)
			fail(w, 502, "送信結果を確認できません。家電の状態を確認してから再操作してください")
			return
		}
		respond(w, 200, map[string]string{"sentAt": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "API が見つかりません") })
	web, _ := fs.Sub(assets, "web")
	files := http.FileServer(http.FS(web))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// 任意の Web サイトからの家電操作を防ぐ。CORS は許可しない。
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					fail(w, 403, "別のサイトからの操作は許可されていません")
					return
				}
			}
			media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if r.Header.Get("X-Requested-With") != "nature-remo" || media != "application/json" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "操作用ヘッダーが必要です")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func main() {
	devices, err := parseDevices(os.Getenv("REMO_DEVICES"))
	if err != nil {
		log.Fatal(err)
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	store, err := openStore(dir)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: newApp(store, devices).handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("Nature Remo listening on %s (%d devices)", addr, len(devices))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}
