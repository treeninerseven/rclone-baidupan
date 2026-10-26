package baidupan

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/baidupan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

type mockUpload struct {
	t            *testing.T
	mu           sync.Mutex
	items        map[string]api.Item
	data         map[string][]byte
	hashes       []string
	parts        map[int][]byte
	partCalls    map[int]int
	requestCount int
	commitCount  int
	retryPart    bool
	badChecksum  bool
	badPartList  bool
	badCommit    bool
	rapid        bool
	skipFirst    bool
	manageError  int
	beforePre    func()
}

func newMockUpload(t *testing.T) *mockUpload {
	return &mockUpload{t: t, items: map[string]api.Item{}, data: map[string][]byte{}, partCalls: map[int]int{}}
}

func (m *mockUpload) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestCount++
	q := r.URL.Query()
	if q.Get("access_token") != "access" {
		m.t.Error("missing access token")
	}
	if r.URL.Path == "/upload" {
		if r.ContentLength <= 0 || r.Header.Get("Content-Type") == "" {
			m.t.Error("multipart upload lacks a fixed content length or type")
		}
		part, err := strconv.Atoi(q.Get("partseq"))
		if err != nil || q.Get("uploadid") != "session" {
			m.t.Error("invalid upload part parameters")
		}
		m.partCalls[part]++
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			m.t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		file, _, err := r.FormFile("file")
		if err != nil {
			m.t.Error(err)
			return
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			m.t.Error(err)
		}
		if part < 0 || part >= len(m.hashes) || fmt.Sprintf("%x", md5.Sum(data)) != m.hashes[part] {
			m.t.Error("upload payload or retry payload does not match the declared MD5")
		}
		if m.retryPart && part == 1 && m.partCalls[part] == 1 {
			w.WriteHeader(503)
			return
		}
		m.parts[part] = data
		hash := fmt.Sprintf("%x", md5.Sum(data))
		if m.badChecksum {
			hash = strings.Repeat("0", 32)
		}
		// A successful PCS response need not contain errno.
		writeJSON(m.t, w, map[string]any{"md5": hash})
		return
	}
	if r.URL.Path == "/download" {
		data := m.data[q.Get("path")]
		http.ServeContent(w, r, "file", time.Unix(1700000000, 0), bytes.NewReader(data))
		return
	}
	if r.URL.Path == "/multimedia" {
		for p, item := range m.items {
			if q.Get("fsids") == "["+strconv.FormatInt(item.ID, 10)+"]" {
				item.Dlink = "https://" + r.Host + "/download?path=" + url.QueryEscape(p)
				writeJSON(m.t, w, map[string]any{"errno": 0, "list": []api.Item{item}})
				return
			}
		}
		writeJSON(m.t, w, map[string]any{"errno": -9})
		return
	}
	if r.Method == "GET" && q.Get("method") == "list" {
		dir := q.Get("dir")
		if dir != "/" && m.items[dir].IsDir == 0 {
			writeJSON(m.t, w, map[string]any{"errno": -9})
			return
		}
		items := []api.Item{}
		for p, item := range m.items {
			if path.Dir(p) == dir {
				items = append(items, item)
			}
		}
		slices.SortFunc(items, func(a, b api.Item) int { return strings.Compare(a.Path, b.Path) })
		start, _ := strconv.Atoi(q.Get("start"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		start = min(start, len(items))
		writeJSON(m.t, w, map[string]any{"errno": 0, "list": items[start:min(start+limit, len(items))]})
		return
	}
	if err := r.ParseForm(); err != nil {
		m.t.Error(err)
	}
	form := r.PostForm
	p := form.Get("path")
	switch q.Get("method") {
	case "precreate":
		if m.beforePre != nil {
			m.beforePre()
		}
		if form.Get("rtype") != "3" || form.Get("autoinit") != "1" || form.Get("local_mtime") == "" {
			m.t.Error("upload lacks overwrite or modification-time parameters")
		}
		if err := json.Unmarshal([]byte(form.Get("block_list")), &m.hashes); err != nil {
			m.t.Error(err)
		}
		m.parts = map[int][]byte{}
		parts := make([]int, len(m.hashes))
		for i := range parts {
			parts[i] = i
		}
		if form.Get("size") == "0" {
			parts = []int{}
		}
		if m.badPartList {
			parts = []int{len(m.hashes)}
		}
		if m.skipFirst {
			m.parts[0] = []byte("abcdefgh")
			parts = parts[1:]
		}
		returnType := 1
		if m.rapid {
			returnType = 2
		}
		writeJSON(m.t, w, map[string]any{"errno": 0, "uploadid": "session", "block_list": parts, "return_type": returnType})
	case "create":
		if strings.ContainsAny(path.Base(p), `:*?"<>|\`) {
			writeJSON(m.t, w, map[string]any{"errno": -7})
			return
		}
		if form.Get("isdir") == "1" {
			if item, ok := m.items[p]; ok {
				writeJSON(m.t, w, map[string]any{"errno": -8, "path": item.Path})
				return
			}
			item := api.Item{ID: int64(len(m.items) + 100), Path: p, IsDir: 1}
			m.items[p] = item
			writeJSON(m.t, w, map[string]any{"errno": 0, "fs_id": item.ID, "path": p, "isdir": 1})
			return
		}
		m.commitCount++
		var blocks []string
		_ = json.Unmarshal([]byte(form.Get("block_list")), &blocks)
		if !slices.Equal(blocks, m.hashes) || form.Get("rtype") != "3" {
			m.t.Error("commit did not preserve the complete ordered block list or overwrite mode")
		}
		var data []byte
		for i := range m.hashes {
			data = append(data, m.parts[i]...)
		}
		size, _ := strconv.ParseInt(form.Get("size"), 10, 64)
		mtime, _ := strconv.ParseInt(form.Get("local_mtime"), 10, 64)
		if int64(len(data)) != size {
			m.t.Error("committed content has the wrong size")
		}
		item := api.Item{ID: int64(len(m.items) + 100), Path: p, Size: size, LocalMtime: mtime}
		m.items[p], m.data[p] = item, data
		if m.badCommit {
			p += ".renamed"
		}
		writeJSON(m.t, w, map[string]any{"errno": 0, "fs_id": item.ID, "path": p, "size": size, "isdir": 0})
	case "filemanager":
		var paths []string
		_ = json.Unmarshal([]byte(form.Get("filelist")), &paths)
		if q.Get("opera") != "delete" || form.Get("async") != "0" || len(paths) != 1 {
			m.t.Error("invalid delete request")
		}
		if m.manageError == 0 {
			delete(m.items, paths[0])
			delete(m.data, paths[0])
		}
		writeJSON(m.t, w, map[string]any{"errno": 0, "info": []any{map[string]any{"errno": m.manageError, "path": paths[0]}}})
	default:
		m.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(400)
	}
}

func TestUploadAndOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	mock := newMockUpload(t)
	mock.retryPart = true
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.opt.ChunkSize = 8
	ctx := context.Background()
	remote := "backup/子目录/test.bin"
	for i, data := range [][]byte{[]byte("abcdefghijklmnopqrstuvwxyz1234567"), []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ7654321")} {
		mtime := time.Unix(1700000000+int64(i*10), 0)
		src := object.NewStaticObjectInfo(remote, mtime, int64(len(data)), true, nil, nil)
		obj, err := f.Put(ctx, struct{ io.Reader }{bytes.NewReader(data)}, src)
		if err != nil {
			t.Fatal(err)
		}
		if obj.Size() != int64(len(data)) || !obj.ModTime(ctx).Equal(mtime) {
			t.Fatalf("uploaded metadata differs: %v, %v", obj.Size(), obj.ModTime(ctx))
		}
		body, err := obj.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("download differs after upload: %v", err)
		}
	}
	entries, err := f.List(ctx, "backup/子目录")
	if err != nil || len(entries) != 1 || mock.commitCount != 2 || mock.partCalls[1] != 3 {
		t.Fatalf("overwrite or retry failed: entries=%d commits=%d retries=%d err=%v", len(entries), mock.commitCount, mock.partCalls[1], err)
	}
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 0 {
		t.Fatal("staged upload files were not cleaned up")
	}
}

func TestUploadValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		size int64
	}{
		{"unknown-size", "x", -1}, {"too-large", "x", int64(1024*4*fs.Mebi + 1)},
		{"short-source", "short", 8}, {"long-source", "too-long", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("TMPDIR", tmpDir)
			mock := newMockUpload(t)
			f, err := testFs(t, mock.handler, "", testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			src := object.NewStaticObjectInfo("file", time.Now(), tc.size, true, nil, nil)
			_, err = f.Put(context.Background(), strings.NewReader(tc.data), src)
			if err == nil || mock.requestCount != 0 {
				t.Fatalf("invalid source accepted or modified remote: %v", err)
			}
			files, _ := os.ReadDir(tmpDir)
			if len(files) != 0 {
				t.Fatal("failed upload leaked a temporary file")
			}
		})
	}
}

func TestUploadServerErrors(t *testing.T) {
	for _, mode := range []string{"checksum", "part-list", "renamed"} {
		t.Run(mode, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("TMPDIR", tmpDir)
			mock := newMockUpload(t)
			mock.badChecksum, mock.badPartList, mock.badCommit = mode == "checksum", mode == "part-list", mode == "renamed"
			f, err := testFs(t, mock.handler, "", testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			f.opt.ChunkSize = 8
			src := object.NewStaticObjectInfo("file", time.Now(), 16, true, nil, nil)
			_, err = f.Put(context.Background(), strings.NewReader("abcdefghijklmnop"), src)
			if err == nil {
				t.Fatal("invalid upload response was accepted")
			}
			if mode != "renamed" && mock.commitCount != 0 {
				t.Fatal("upload was committed despite invalid or corrupt parts")
			}
			files, _ := os.ReadDir(tmpDir)
			if len(files) != 0 {
				t.Fatal("failed upload leaked a temporary file")
			}
		})
	}
}

func TestUploadReusesExistingParts(t *testing.T) {
	mock := newMockUpload(t)
	mock.skipFirst = true
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.opt.ChunkSize = 8
	src := object.NewStaticObjectInfo("file", time.Now(), 16, true, nil, nil)
	if _, err := f.Put(context.Background(), strings.NewReader("abcdefghijklmnop"), src); err != nil {
		t.Fatal(err)
	}
	if mock.partCalls[0] != 0 || mock.partCalls[1] != 1 || !bytes.Equal(mock.data["/file"], []byte("abcdefghijklmnop")) {
		t.Fatal("server-side existing parts were not reused correctly")
	}
}

func TestDirectoriesAndDelete(t *testing.T) {
	mock := newMockUpload(t)
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.Mkdir(ctx, "parent/child"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "parent/child"); err != nil {
		t.Fatalf("mkdir is not idempotent: %v", err)
	}
	if err := f.Rmdir(ctx, "parent"); !errors.Is(err, fs.ErrorDirectoryNotEmpty) {
		t.Fatalf("non-empty directory removal was accepted: %v", err)
	}
	if err := f.Rmdir(ctx, ""); !errors.Is(err, fs.ErrorCantPurge) {
		t.Fatalf("account root deletion was accepted: %v", err)
	}
	if err := f.Rmdir(ctx, "parent/child"); err != nil {
		t.Fatal(err)
	}
	mock.items["/parent/file"] = api.Item{ID: 9, Path: "/parent/file"}
	obj, err := f.NewObject(ctx, "parent/file")
	if err != nil {
		t.Fatal(err)
	}
	mock.manageError = -6
	if err := obj.Remove(ctx); err == nil {
		t.Fatal("individual deletion failure was ignored")
	}
	mock.manageError = 0
	if err := obj.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
}

func TestUploadCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := stageUpload(ctx, strings.NewReader("data"), 4, 8)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(tmpDir, "rclone-baidupan-upload-*"))
	if len(files) != 0 {
		t.Fatal("cancelled upload leaked a staged file")
	}
}

type accountedUploadReader struct {
	in    io.Reader
	bytes *atomic.Int64
}

func (r *accountedUploadReader) Read(p []byte) (int, error) {
	n, err := r.in.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

func (r *accountedUploadReader) OldStream() io.Reader { return r.in }

func (r *accountedUploadReader) SetStream(in io.Reader) { r.in = in }

func (r *accountedUploadReader) WrapStream(in io.Reader) io.Reader {
	return &accountedUploadReader{in: in, bytes: r.bytes}
}

func TestUploadAccountsNetworkReads(t *testing.T) {
	var counted atomic.Int64
	mock := newMockUpload(t)
	mock.beforePre = func() {
		if counted.Load() != 0 {
			t.Error("staging disk reads were accounted as network transfers")
		}
	}
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	f.opt.ChunkSize = 8
	src := object.NewStaticObjectInfo("file", time.Now(), 16, true, nil, nil)
	in := &accountedUploadReader{in: strings.NewReader("abcdefghijklmnop"), bytes: &counted}
	if _, err := f.Put(context.Background(), in, src); err != nil {
		t.Fatal(err)
	}
	if counted.Load() != 16 {
		t.Fatalf("network accounting counted %d bytes, expected 16", counted.Load())
	}
}

func TestUploadReservedNames(t *testing.T) {
	mock := newMockUpload(t)
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	remote := `hello? sausage/êé/Hello, 世界/ " ' @ < > & ? + ≠/z.txt`
	src := object.NewStaticObjectInfo(remote, time.Unix(1700000000, 0), 4, true, nil, nil)
	if _, err := f.Put(context.Background(), strings.NewReader("data"), src); err != nil {
		t.Fatal(err)
	}
	entries, err := f.List(context.Background(), path.Dir(remote))
	if err != nil || len(entries) != 1 || entries[0].Remote() != remote {
		t.Fatalf("reserved filename did not roundtrip: %v, %v", entries, err)
	}
}

func TestUploadEmptyFile(t *testing.T) {
	mock := newMockUpload(t)
	f, err := testFs(t, mock.handler, "", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	src := object.NewStaticObjectInfo("empty.txt", time.Unix(1700000000, 0), 0, true, nil, nil)
	obj, err := f.Put(context.Background(), strings.NewReader(""), src)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size() != 0 || len(mock.partCalls) != 0 || mock.commitCount != 1 {
		t.Fatal("empty file was not committed without sending a data part")
	}
	body, err := obj.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || len(data) != 0 {
		t.Fatalf("empty file download failed: %v", err)
	}
}
