package baidupan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/baidupan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

func testConfig(t *testing.T) configmap.Simple {
	t.Helper()
	token, err := json.Marshal(&oauth2.Token{
		AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return configmap.Simple{
		"client_id": "app", "client_secret": obscure.MustObscure("secret"), "token": string(token),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func testFs(t *testing.T, handler http.HandlerFunc, root string, m configmap.Simple) (*Fs, error) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	ctx, ci := fs.AddConfig(context.Background())
	ci.LowLevelRetries = 3
	// Local mock servers use self-signed certificates.
	ci.InsecureSkipVerify = true
	f, err := newFs(ctx, "test", root, m, server.URL, server.URL)
	if f != nil {
		f.pacer.SetCalculator(pacer.NewDefault(pacer.MinSleep(time.Millisecond), pacer.MaxSleep(time.Millisecond)))
		f.upload = rest.NewClient(fshttp.NewClient(ctx)).SetRoot(server.URL + "/upload").SetErrorHandler(httpError)
	}
	return f, err
}

func TestListPagination(t *testing.T) {
	m := testConfig(t)
	m["list_chunk"] = "2"
	var starts []string
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/file" || q.Get("method") != "list" || q.Get("dir") != "/" || q.Get("limit") != "2" || q.Get("access_token") != "access" {
			t.Errorf("unexpected list request: %s", r.URL)
		}
		starts = append(starts, q.Get("start"))
		items := []api.Item{{ID: 1, Path: "/中文.txt", Size: 3, LocalMtime: 100}, {ID: 2, Path: "/folder", IsDir: 1, ServerMtime: 200}}
		if q.Get("start") == "2" {
			items = []api.Item{{ID: 3, Path: "/last", Size: 5}}
		}
		writeJSON(t, w, map[string]any{"errno": 0, "list": items})
	}, "", m)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Remote() != "中文.txt" || entries[0].Size() != 3 || entries[0].ModTime(context.Background()).Unix() != 100 {
		t.Fatalf("incorrect entries: %v", entries)
	}
	if dir, ok := entries[1].(fs.Directory); !ok || dir.ID() != "2" || dir.ModTime(context.Background()).Unix() != 200 {
		t.Fatalf("incorrect directory: %v", entries[1])
	}
	if !reflect.DeepEqual(starts, []string{"0", "2"}) {
		t.Fatalf("pagination starts: %v", starts)
	}
}

func TestListRejectsOutsideEntry(t *testing.T) {
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"errno": 0, "list": []api.Item{{Path: "/outside/file"}}})
	}, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := f.List(context.Background(), ""); err == nil || entries != nil {
		t.Fatalf("accepted invalid entry: %v, %v", entries, err)
	}
}

func TestNewObjectAndRoot(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		items := []api.Item{{ID: 4, Path: "/folder", IsDir: 1}, {ID: 5, Path: "/file", Size: 10}}
		if r.URL.Query().Get("dir") == "/folder" {
			items = []api.Item{{ID: 6, Path: "/folder/child", Size: 7}}
		}
		writeJSON(t, w, map[string]any{"errno": 0, "list": items})
	}
	for _, tc := range []struct {
		root, wantRoot string
		wantErr        error
	}{
		{"", "", nil}, {"/folder/", "folder", nil}, {"file", "", fs.ErrorIsFile}, {"missing", "missing", nil},
	} {
		t.Run("root="+tc.root, func(t *testing.T) {
			f, err := testFs(t, handler, tc.root, testConfig(t))
			if !errors.Is(err, tc.wantErr) || f == nil || f.Root() != tc.wantRoot {
				t.Fatalf("root=%v, err=%v", f, err)
			}
		})
	}
	f, err := testFs(t, handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote string
		want   error
	}{
		{"file", nil}, {"folder", fs.ErrorIsDir}, {"missing", fs.ErrorObjectNotFound},
	} {
		obj, err := f.NewObject(context.Background(), tc.remote)
		if !errors.Is(err, tc.want) {
			t.Fatalf("NewObject(%q): %v", tc.remote, err)
		}
		if err == nil && (obj.Size() != 10 || obj.(fs.IDer).ID() != "5") {
			t.Fatalf("incorrect object: %+v", obj)
		}
	}
}

func TestPaths(t *testing.T) {
	var requests atomic.Int32
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.root = "根目录"
	got, err := f.fullPath("中文/报告 #?.txt")
	if err != nil || got != "/根目录/中文/报告 #？.txt" {
		t.Fatalf("path=%q, err=%v", got, err)
	}
	for _, p := range []string{"..", "../escape", "a/../../escape", "/absolute"} {
		if _, err := f.List(context.Background(), p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	f.opt.Enc = encoder.Display | encoder.EncodeInvalidUtf8
	if got, err := f.fullPath("a\xff"); err != nil || !strings.Contains(got, "a") || strings.Contains(got, "\xff") {
		t.Fatalf("invalid UTF-8 was not encoded: %q, %v", got, err)
	}
	ctx := context.Background()
	obj := &Object{fs: f}
	if obj.SetModTime(ctx, time.Now()) != fs.ErrorCantSetModTime || f.Hashes().Contains(hash.MD5) {
		t.Fatal("unverified metadata capabilities advertised")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid path made a request")
	}
}

func TestAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"missing", 200, `{"errno":-9}`, fs.ErrorDirNotFound},
		{"missing-directory-xpan", 200, `{"errno":-7}`, fs.ErrorDirNotFound},
		{"missing-pcs", 200, `{"errno":31066}`, fs.ErrorDirNotFound},
		{"permission", 200, `{"errno":-6,"errmsg":"denied"}`, nil},
		{"http-json", 403, `{"error_code":31023,"error_msg":"blocked"}`, nil},
		{"http-html", 403, `<html>Forbidden</html>`, nil},
		{"missing-status", 200, `{"list":[]}`, nil},
		{"invalid-json", 200, `{`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, "", testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.List(context.Background(), "")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("error=%v, want=%v", err, tc.want)
			}
		})
	}
}

func TestRetryAndCancellation(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				writeJSON(t, w, map[string]any{"errno": 0, "list": []api.Item{}})
			}, "", testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.List(context.Background(), ""); err != nil || calls.Load() != 2 {
				t.Fatalf("calls=%d, err=%v", calls.Load(), err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retry, err := shouldRetry(ctx, nil, context.Canceled)
	if retry || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation retried: %v, %v", retry, err)
	}
}

func TestConfigure(t *testing.T) {
	m := testConfig(t)
	delete(m, "token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/token" || q.Get("grant_type") != "authorization_code" || q.Get("code") != "auth-code" || q.Get("client_id") != "app" || q.Get("client_secret") != "secret" || q.Get("redirect_uri") != "oob" {
			t.Errorf("incorrect token exchange: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
	}))
	defer server.Close()
	ctx := context.Background()
	out, err := configure(ctx, "test", m, fs.ConfigIn{}, server.URL)
	if err != nil || out.State != "authorize" {
		t.Fatalf("initial config: %v, %v", out, err)
	}
	out, err = configure(ctx, "test", m, fs.ConfigIn{State: "authorize"}, server.URL)
	if err != nil || out.State != "exchange" || !strings.Contains(out.Option.Help, "basic%2Cnetdisk") || strings.Contains(out.Option.Help, "secret") {
		t.Fatalf("authorization prompt: %v, %v", out, err)
	}
	out, err = configure(ctx, "test", m, fs.ConfigIn{State: "exchange", Result: " auth-code "}, server.URL)
	if err != nil || out != nil {
		t.Fatalf("exchange: %v, %v", out, err)
	}
	token, err := oauthutilToken(m)
	if err != nil || token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" || !token.Valid() {
		t.Fatalf("token not saved: %v", err)
	}
	if _, ok := m["config_code"]; ok {
		t.Fatal("authorization code was persisted")
	}
	out, err = configure(ctx, "test", m, fs.ConfigIn{}, server.URL)
	if err != nil || out.State != "reconnect" {
		t.Fatalf("reconnect prompt: %v, %v", out, err)
	}
}

func oauthutilToken(m configmap.Simple) (*oauth2.Token, error) {
	var token oauth2.Token
	err := json.Unmarshal([]byte(m[config.ConfigToken]), &token)
	return &token, err
}

func TestRefreshConcurrent(t *testing.T) {
	var calls atomic.Int32
	m := testConfig(t)
	token, err := oauthutilToken(m)
	if err != nil {
		t.Fatal(err)
	}
	token.Expiry = time.Now().Add(-time.Hour)
	b, _ := json.Marshal(token)
	m["token"] = string(b)
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.URL.Path != "/token" || q.Get("grant_type") != "refresh_token" || q.Get("refresh_token") != "refresh" || q.Get("client_secret") != "secret" {
			t.Error("incorrect refresh request")
		}
		// A missing refresh_token must retain the current refresh token.
		writeJSON(t, w, map[string]any{"access_token": "refreshed", "expires_in": 3600})
	}, "", m)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			token, err := f.accessToken(context.Background(), "")
			if err != nil || token != "refreshed" {
				t.Errorf("refresh result: %q, %v", token, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	token, err = oauthutilToken(m)
	if err != nil || token.RefreshToken != "refresh" || token.AccessToken != "refreshed" {
		t.Fatal("refreshed credentials were not persisted correctly")
	}
}

func TestRejectedTokenRefresh(t *testing.T) {
	var refreshes, lists atomic.Int32
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			writeJSON(t, w, map[string]any{"access_token": "replacement", "refresh_token": "rotated", "expires_in": 3600})
			return
		}
		lists.Add(1)
		if r.URL.Query().Get("access_token") == "access" {
			writeJSON(t, w, map[string]any{"errno": 20016})
			return
		}
		writeJSON(t, w, map[string]any{"errno": 0, "list": []api.Item{}})
	}, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(context.Background(), ""); err != nil || refreshes.Load() != 1 || lists.Load() != 2 {
		t.Fatalf("lists=%d, refreshes=%d, err=%v", lists.Load(), refreshes.Load(), err)
	}
}

func TestTokenFailureAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{200, `{"error":"invalid_grant","error_description":"sensitive"}`},
		{400, `{"error":"invalid_grant","error_description":"sensitive"}`},
		{200, `{"access_token":"x"}`},
		{200, `{}`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		srv := rest.NewClient(server.Client()).SetRoot(server.URL).SetErrorHandler(httpError)
		_, err := requestToken(context.Background(), srv, Options{ClientID: "app", ClientSecret: "secret"}, url.Values{})
		server.Close()
		if err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("invalid token response accepted or exposed: %v", err)
		}
		if tc.status == 400 && !strings.Contains(err.Error(), "invalid_grant") {
			t.Fatalf("OAuth error code was lost: %v", err)
		}
	}
	err := withoutURL(&url.Error{Op: "Get", URL: "https://example.com/?access_token=secret", Err: context.DeadlineExceeded})
	if strings.Contains(err.Error(), "secret") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transport error was not redacted: %v", err)
	}
}

func TestAPIRedirectDoesNotForwardCredentials(t *testing.T) {
	var redirects atomic.Int32
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unexpected" {
			redirects.Add(1)
		}
		http.Redirect(w, r, "/unexpected", http.StatusFound)
	}, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.List(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "unexpected API redirect") || strings.Contains(err.Error(), "access_token") || redirects.Load() != 0 {
		t.Fatalf("API redirect was not rejected safely: %v", err)
	}
	_, err = requestToken(context.Background(), f.oauth, f.opt, url.Values{"code": {"private-code"}})
	if err == nil || !strings.Contains(err.Error(), "unexpected API redirect") || strings.Contains(err.Error(), "private-code") || strings.Contains(err.Error(), "secret") || redirects.Load() != 0 {
		t.Fatalf("OAuth redirect was not rejected safely: %v", err)
	}
}

func TestRefreshFailure(t *testing.T) {
	var calls atomic.Int32
	f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, map[string]any{"error": "invalid_grant"})
	}, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.token.Expiry = time.Now().Add(-time.Hour)
	_, err = f.accessToken(context.Background(), "")
	if err == nil || calls.Load() != 1 {
		t.Fatalf("failed refresh was retried: calls=%d, err=%v", calls.Load(), err)
	}
	f.token.RefreshToken = ""
	_, err = f.accessToken(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "config reconnect") || calls.Load() != 1 {
		t.Fatalf("missing refresh token did not request reconnect: %v", err)
	}
}

func TestOpen(t *testing.T) {
	for _, tc := range []struct {
		name, mode, want string
		option           fs.OpenOption
		wantErr          bool
	}{
		{"full", "", "0123456789", nil, false},
		{"range", "", "2345", &fs.RangeOption{Start: 2, End: 5}, false},
		{"suffix", "", "789", &fs.RangeOption{Start: -1, End: 3}, false},
		{"seek", "", "6789", &fs.SeekOption{Offset: 6}, false},
		{"http-link-upgrade", "http-link", "0123456789", nil, false},
		{"ignored-range", "ignore", "", &fs.RangeOption{Start: 2, End: 5}, true},
		{"wrong-range", "wrong-range", "", &fs.RangeOption{Start: 2, End: 5}, true},
		{"wrong-length", "wrong-length", "", nil, true},
		{"http-error", "http-error", "", nil, true},
		{"missing-link", "missing-link", "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := testFs(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("access_token") != "access" {
					t.Error("missing API or download token")
				}
				if r.URL.Path == "/multimedia" {
					if q.Get("fsids") != "[45]" || q.Get("dlink") != "1" {
						t.Error("incorrect metadata parameters")
					}
					link := "https://" + r.Host + "/download?signature=keep"
					if tc.mode == "http-link" {
						link = "http://" + r.Host + "/download?signature=keep"
					}
					if tc.mode == "missing-link" {
						link = ""
					}
					writeJSON(t, w, map[string]any{"errno": 0, "list": []api.Item{{ID: 45, Dlink: link}}})
					return
				}
				if r.URL.Path != "/download" || q.Get("signature") != "keep" || r.Header.Get("User-Agent") != "pan.baidu.com" || r.Header.Get("Authorization") != "" {
					t.Error("incorrect download request")
				}
				if tc.mode == "http-error" {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, "forbidden")
					return
				}
				data := "0123456789"
				if tc.mode == "wrong-length" {
					data = "short"
				}
				if header := r.Header.Get("Range"); header != "" && tc.mode != "ignore" {
					rangeOpt, err := fs.ParseRangeOption(header)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					start, limit := rangeOpt.Decode(10)
					end := int64(9)
					if limit >= 0 {
						end = min(start+limit-1, end)
					}
					contentRange := fmt.Sprintf("bytes %d-%d/10", start, end)
					if tc.mode == "wrong-range" {
						contentRange = "bytes 0-3/10"
					}
					w.Header().Set("Content-Range", contentRange)
					data = data[start : end+1]
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.WriteHeader(206)
				}
				_, _ = io.WriteString(w, data)
			}, "", testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			obj := &Object{fs: f, remote: "file", id: 45, size: 10}
			var options []fs.OpenOption
			if tc.option != nil {
				options = append(options, tc.option)
			}
			body, err := obj.Open(context.Background(), options...)
			if tc.wantErr {
				if err == nil {
					_ = body.Close()
					t.Fatal("expected download failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			closeErr := body.Close()
			if err != nil || closeErr != nil || string(data) != tc.want {
				t.Fatalf("download=%q, err=%v, close=%v", data, err, closeErr)
			}
		})
	}
}
