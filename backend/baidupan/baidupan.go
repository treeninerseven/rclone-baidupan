// Package baidupan provides access to Baidu Netdisk using XPan.
package baidupan

import (
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/baidupan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/pool"
	"github.com/rclone/rclone/lib/readers"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

const (
	rootURL         = "https://pan.baidu.com/rest/2.0/xpan"
	oauthURL        = "https://openapi.baidu.com/oauth/2.0"
	uploadURL       = "https://d.pcs.baidu.com/rest/2.0/pcs/superfile2"
	maxParts        = 1024
	defaultEncoding = encoder.Display | encoder.EncodeWin | encoder.EncodeBackSlash |
		encoder.EncodeLeftSpace | encoder.EncodeRightSpace | encoder.EncodeRightPeriod | encoder.EncodeInvalidUtf8
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "baidupan",
		Description: "Baidu Netdisk (experimental)",
		NewFs:       NewFs,
		Config: func(ctx context.Context, name string, m configmap.Mapper, in fs.ConfigIn) (*fs.ConfigOut, error) {
			return configure(ctx, name, m, in, oauthURL)
		},
		Options: []fs.Option{{
			Name:      "client_id",
			Help:      "AppKey of your own Baidu Netdisk Open Platform application.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:       "client_secret",
			Help:       "SecretKey of your Baidu Netdisk application.",
			Required:   true,
			IsPassword: true,
			Sensitive:  true,
		}, {
			Name:      config.ConfigToken,
			Help:      "OAuth token as a JSON blob. Normally filled in during authorization.",
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "list_chunk",
			Help:     "Number of entries requested per directory page (1-1000).",
			Default:  1000,
			Advanced: true,
		}, {
			Name:     "chunk_size",
			Help:     "Upload chunk size: 4 MiB, 16 MiB or 32 MiB. Sizes above 4 MiB require corresponding Baidu membership permissions. Uploads are limited to 1024 chunks and staged in a temporary file.",
			Default:  fs.SizeSuffix(4 * fs.Mebi),
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Default:  defaultEncoding,
			Advanced: true,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	ClientID     string               `config:"client_id"`
	ClientSecret string               `config:"client_secret"`
	ListChunk    int                  `config:"list_chunk"`
	ChunkSize    fs.SizeSuffix        `config:"chunk_size"`
	Enc          encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a Baidu Netdisk remote.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	srv      *rest.Client
	oauth    *rest.Client
	upload   *rest.Client
	pacer    *fs.Pacer
	m        configmap.Mapper
	tokenMu  sync.Mutex
	token    *oauth2.Token
	mkdirMu  sync.Mutex
}

// Object describes a Baidu Netdisk file.
type Object struct {
	fs      *Fs
	remote  string
	id      int64
	size    int64
	modTime time.Time
}

func readOptions(m configmap.Mapper) (opt Options, err error) {
	opt.ListChunk = 1000
	opt.ChunkSize = 4 * fs.Mebi
	opt.Enc = defaultEncoding
	if err = configstruct.Set(m, &opt); err != nil {
		return opt, err
	}
	if opt.ClientID == "" || opt.ClientSecret == "" {
		return opt, errors.New("baidupan requires your application's client_id and client_secret")
	}
	opt.ClientSecret, err = obscure.Reveal(opt.ClientSecret)
	if err != nil {
		return opt, fmt.Errorf("invalid client_secret: %w", err)
	}
	if opt.ListChunk < 1 || opt.ListChunk > 1000 {
		return opt, errors.New("list_chunk must be between 1 and 1000")
	}
	if !slices.Contains([]fs.SizeSuffix{4 * fs.Mebi, 16 * fs.Mebi, 32 * fs.Mebi}, opt.ChunkSize) {
		return opt, errors.New("chunk_size must be 4 MiB, 16 MiB or 32 MiB")
	}
	return opt, nil
}

func configure(ctx context.Context, name string, m configmap.Mapper, in fs.ConfigIn, endpoint string) (*fs.ConfigOut, error) {
	opt, err := readOptions(m)
	if err != nil {
		return nil, err
	}
	switch in.State {
	case "":
		if token, _ := m.Get(config.ConfigToken); token != "" {
			return fs.ConfigConfirm("reconnect", false, "config_reconnect", "Replace the existing Baidu authorization?")
		}
		return fs.ConfigGoto("authorize")
	case "reconnect":
		if in.Result != "true" {
			return nil, nil
		}
		return fs.ConfigGoto("authorize")
	case "authorize":
		params := url.Values{
			"response_type": {"code"}, "client_id": {opt.ClientID},
			"redirect_uri": {"oob"}, "scope": {"basic,netdisk"}, "display": {"page"},
		}
		return fs.ConfigInput("exchange", "config_code", "Open this URL in a browser and authorize your application:\n\n"+endpoint+"/authorize?"+params.Encode()+"\n\nEnter the authorization code.")
	case "exchange":
		code := strings.TrimSpace(in.Result)
		if code == "" {
			return fs.ConfigError("authorize", "Authorization code cannot be empty")
		}
		srv := rest.NewClient(fshttp.NewClient(ctx)).SetRoot(endpoint).SetErrorHandler(httpError)
		token, err := requestToken(ctx, srv, opt, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"oob"},
		})
		if err != nil {
			return nil, err
		}
		return nil, oauthutil.PutToken(name, m, token, true)
	default:
		return nil, fmt.Errorf("unknown configuration state %q", in.State)
	}
}

// requestToken sends credentials in the query as required by Baidu's OAuth endpoint.
func requestToken(ctx context.Context, srv *rest.Client, opt Options, params url.Values) (*oauth2.Token, error) {
	params.Set("client_id", opt.ClientID)
	params.Set("client_secret", opt.ClientSecret)
	var result api.TokenResponse
	_, err := srv.CallJSON(ctx, &rest.Opts{Method: "GET", Path: "/token", Parameters: params, CheckRedirect: refuseRedirect}, nil, &result)
	if err != nil {
		return nil, fmt.Errorf("baidupan: token request failed: %w", withoutURL(err))
	}
	if result.Error != "" {
		return nil, fmt.Errorf("baidupan: OAuth error %s; authorize again with rclone config reconnect", result.Error)
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 {
		return nil, errors.New("baidupan: invalid OAuth token response")
	}
	return &oauth2.Token{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		TokenType: "Bearer", Expiry: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("unexpected API redirect")
}

// withoutURL removes query credentials from transport errors.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

func httpError(resp *http.Response) error {
	body, err := rest.ReadBody(resp)
	if err == nil {
		var result api.Response
		if json.Unmarshal(body, &result) == nil && (result.Errno != nil || result.ErrorCode != nil) {
			if apiErr := responseError(result); apiErr != nil {
				return apiErr
			}
		}
		var tokenError api.TokenResponse
		if json.Unmarshal(body, &tokenError) == nil && tokenError.Error != "" {
			return fmt.Errorf("baidupan: OAuth error %s; authorize again with rclone config reconnect", tokenError.Error)
		}
	}
	return fmt.Errorf("baidupan: HTTP %d", resp.StatusCode)
}

func responseError(result api.Response) error {
	code := result.Errno
	if code == nil || (*code == 0 && result.ErrorCode != nil) {
		code = result.ErrorCode
	}
	if code == nil {
		return errors.New("baidupan: API response has no status code")
	}
	if *code == 0 {
		return nil
	}
	message := result.Errmsg
	if message == "" {
		message = result.ErrorMsg
	}
	return &api.Error{Code: *code, Message: message}
}

func (f *Fs) accessToken(ctx context.Context, rejected string) (string, error) {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	if f.token.Valid() && (rejected == "" || f.token.AccessToken != rejected) {
		return f.token.AccessToken, nil
	}
	if f.token.RefreshToken == "" {
		return "", fmt.Errorf("baidupan: token expired; run rclone config reconnect %s:", f.name)
	}
	// Another instance may have refreshed the token in the same config mapper.
	if saved, err := oauthutil.GetToken(f.name, f.m); err == nil && saved.Valid() && saved.AccessToken != f.token.AccessToken && saved.AccessToken != rejected {
		f.token = saved
		return saved.AccessToken, nil
	}
	token, err := requestToken(ctx, f.oauth, f.opt, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {f.token.RefreshToken},
	})
	if err != nil {
		return "", err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = f.token.RefreshToken
	}
	if err := oauthutil.PutToken(f.name, f.m, token, false); err != nil {
		return "", err
	}
	f.token = token
	return token.AccessToken, nil
}

func tokenRejected(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && slices.Contains([]int{110, 111, 20016, 20017, 31045}, e.Code)
}

func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	var e *api.Error
	limited := errors.As(err, &e) && e.Code == 31034
	return limited || fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, []int{429, 500, 502, 503, 504}), err
}

func (f *Fs) callAPI(ctx context.Context, endpoint string, params url.Values, result any) error {
	return f.callJSON(ctx, f.srv, rest.Opts{Method: "GET", Path: endpoint, Parameters: params}, nil, result, true)
}

func (f *Fs) postAPI(ctx context.Context, params, form url.Values, result any, retrySafe bool) error {
	encoded := form.Encode()
	return f.callJSON(ctx, f.srv, rest.Opts{
		Method: "POST", Path: "/file", Parameters: params, ContentType: "application/x-www-form-urlencoded",
	}, func() (io.Reader, int64) { return strings.NewReader(encoded), int64(len(encoded)) }, result, retrySafe)
}

// callJSON rebuilds request bodies on retries; writes only retry automatically when idempotent.
func (f *Fs) callJSON(ctx context.Context, srv *rest.Client, opts rest.Opts, body func() (io.Reader, int64), result any, retrySafe bool) error {
	refreshed := false
	return f.pacer.Call(func() (bool, error) {
		token, err := f.accessToken(ctx, "")
		if err != nil {
			return false, err
		}
		attempt := opts
		attempt.Parameters = url.Values{}
		for key, values := range opts.Parameters {
			attempt.Parameters[key] = slices.Clone(values)
		}
		attempt.Parameters.Set("access_token", token)
		attempt.CheckRedirect = refuseRedirect
		if body != nil {
			reader, size := body()
			attempt.Body = reader
			attempt.ContentLength = &size
		}
		var raw json.RawMessage
		resp, err := srv.CallJSON(ctx, &attempt, nil, &raw)
		if err == nil {
			var status api.Response
			if err = json.Unmarshal(raw, &status); err == nil {
				// PCS can omit errno on successful multipart responses.
				if opts.MultipartContentName != "" && status.Errno == nil && status.ErrorCode == nil {
					var part api.UploadPartResponse
					if err = json.Unmarshal(raw, &part); err == nil && part.MD5 == "" {
						err = errors.New("baidupan: upload response has no status or checksum")
					}
				} else {
					err = responseError(status)
				}
			}
		}
		if tokenRejected(err) && !refreshed {
			refreshed = true
			if _, refreshErr := f.accessToken(ctx, token); refreshErr != nil {
				return false, refreshErr
			}
			return true, err
		}
		if err == nil {
			err = json.Unmarshal(raw, result)
		}
		if !retrySafe {
			return false, withoutURL(err)
		}
		return shouldRetry(ctx, resp, withoutURL(err))
	})
}

func notFound(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && slices.Contains([]int{-9, 31066}, e.Code)
}

// NewFs constructs a filesystem rooted at remote:root.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	f, err := newFs(ctx, name, root, m, rootURL, oauthURL)
	if f == nil {
		return nil, err
	}
	return f, err
}

func newFs(ctx context.Context, name, root string, m configmap.Mapper, apiEndpoint, oauthEndpoint string) (*Fs, error) {
	opt, err := readOptions(m)
	if err != nil {
		return nil, err
	}
	token, err := oauthutil.GetToken(name, m)
	if err != nil {
		return nil, err
	}
	// fshttp enforces the configured User-Agent even when a request sets its own.
	ctx, ci := fs.AddConfig(ctx)
	ci.UserAgent = "pan.baidu.com"
	client := fshttp.NewClient(ctx)
	f := &Fs{
		name: name, root: strings.Trim(path.Clean("/"+root), "/"), opt: opt, m: m, token: token,
		srv:    rest.NewClient(client).SetRoot(apiEndpoint).SetErrorHandler(httpError),
		oauth:  rest.NewClient(client).SetRoot(oauthEndpoint).SetErrorHandler(httpError),
		upload: rest.NewClient(client).SetRoot(uploadURL).SetErrorHandler(httpError),
		pacer:  fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(100*time.Millisecond), pacer.MaxSleep(5*time.Second), pacer.DecayConstant(2))),
	}
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f)
	if f.root != "" {
		fullPath, err := f.fullPath("")
		if err != nil {
			return nil, err
		}
		item, err := f.findItem(ctx, fullPath)
		if err != nil {
			if errors.Is(err, fs.ErrorObjectNotFound) {
				return f, nil
			}
			return nil, err
		}
		if item.IsDir == 0 {
			f.root = strings.Trim(path.Dir("/"+f.root), "/")
			return f, fs.ErrorIsFile
		}
	}
	return f, nil
}

// Name returns the remote name.
func (f *Fs) Name() string { return f.name }

// Root returns the root path.
func (f *Fs) Root() string { return f.root }

// String describes the filesystem.
func (f *Fs) String() string { return "Baidu Netdisk root '" + f.root + "'" }

// Precision returns the precision of uploaded modification times.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns no hashes until XPan's checksum semantics are verified.
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.None) }

// Features returns the optional capabilities.
func (f *Fs) Features() *fs.Features { return f.features }

func (f *Fs) fullPath(remote string) (string, error) {
	cleaned := path.Clean(remote)
	if path.IsAbs(remote) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("baidupan: path must stay within the remote root")
	}
	return f.opt.Enc.FromStandardPath(path.Join("/", f.root, cleaned)), nil
}

func (f *Fs) listAll(ctx context.Context, directory string, visit func(api.Item) bool) error {
	for start := 0; ; {
		var page api.ListResponse
		err := f.callAPI(ctx, "/file", url.Values{
			"method": {"list"}, "dir": {directory}, "order": {"name"},
			"start": {strconv.Itoa(start)}, "limit": {strconv.Itoa(f.opt.ListChunk)},
		}, &page)
		var apiErr *api.Error
		// The list endpoint also reports missing directories as errno -7.
		if notFound(err) || (errors.As(err, &apiErr) && apiErr.Code == -7) {
			return fs.ErrorDirNotFound
		}
		if err != nil {
			return err
		}
		for _, item := range page.List {
			if visit(item) {
				return nil
			}
		}
		if len(page.List) < f.opt.ListChunk {
			return nil
		}
		start += len(page.List)
	}
}

func (f *Fs) findItem(ctx context.Context, fullPath string) (*api.Item, error) {
	var found *api.Item
	err := f.listAll(ctx, path.Dir(fullPath), func(item api.Item) bool {
		if item.Path == fullPath {
			found = &item
			return true
		}
		return false
	})
	if errors.Is(err, fs.ErrorDirNotFound) || (err == nil && found == nil) {
		return nil, fs.ErrorObjectNotFound
	}
	return found, err
}

func itemTime(item api.Item) time.Time {
	if item.LocalMtime > 0 {
		return time.Unix(item.LocalMtime, 0)
	}
	return time.Unix(item.ServerMtime, 0)
}

func (f *Fs) object(remote string, item api.Item) *Object {
	return &Object{fs: f, remote: remote, id: item.ID, size: item.Size, modTime: itemTime(item)}
}

// List returns every file and directory immediately below dir.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fullPath, err := f.fullPath(dir)
	if err != nil {
		return nil, err
	}
	var entryErr error
	err = f.listAll(ctx, fullPath, func(item api.Item) bool {
		// Reject malformed entries rather than exposing paths outside this directory.
		if path.Dir(item.Path) != fullPath || item.Path == fullPath {
			entryErr = fmt.Errorf("baidupan: invalid entry path %q", item.Path)
			return true
		}
		remote := path.Join(dir, f.opt.Enc.ToStandardName(path.Base(item.Path)))
		if item.IsDir != 0 {
			entries = append(entries, fs.NewDir(remote, itemTime(item)).SetID(strconv.FormatInt(item.ID, 10)))
		} else {
			entries = append(entries, f.object(remote, item))
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if entryErr != nil {
		return nil, entryErr
	}
	return entries, nil
}

// NewObject finds a file by its path relative to the root.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fullPath, err := f.fullPath(remote)
	if err != nil {
		return nil, err
	}
	item, err := f.findItem(ctx, fullPath)
	if err != nil {
		return nil, err
	}
	if item.IsDir != 0 {
		return nil, fs.ErrorIsDir
	}
	return f.object(remote, *item), nil
}

// Put uploads a file, replacing an existing file at the same path.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{fs: f, remote: src.Remote()}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}

// Mkdir creates dir and its parents; an existing directory is accepted.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	fullPath, err := f.fullPath(dir)
	if err != nil {
		return err
	}
	f.mkdirMu.Lock()
	defer f.mkdirMu.Unlock()
	return f.mkdirPath(ctx, fullPath)
}

func (f *Fs) mkdirPath(ctx context.Context, fullPath string) error {
	if fullPath == "/" {
		return nil
	}
	item, err := f.findItem(ctx, fullPath)
	if err == nil {
		if item.IsDir == 0 {
			return fs.ErrorIsFile
		}
		return nil
	}
	if !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}
	if err := f.mkdirPath(ctx, path.Dir(fullPath)); err != nil {
		return err
	}
	var created api.CreateResponse
	err = f.postAPI(ctx, url.Values{"method": {"create"}}, url.Values{
		"path": {fullPath}, "size": {"0"}, "isdir": {"1"}, "rtype": {"0"},
	}, &created, false)
	if err != nil {
		// A competing creator or a lost response may have created the directory.
		if item, lookupErr := f.findItem(ctx, fullPath); lookupErr == nil && item.IsDir != 0 {
			return nil
		}
		return err
	}
	if created.Path != fullPath || created.IsDir != 1 {
		return errors.New("baidupan: mkdir returned an unexpected path or entry type")
	}
	return nil
}

// Rmdir deletes an empty directory and refuses the account root.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fullPath, err := f.fullPath(dir)
	if err != nil {
		return err
	}
	if fullPath == "/" {
		return fs.ErrorCantPurge
	}
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	return f.deletePath(ctx, fullPath)
}

func (f *Fs) deletePath(ctx context.Context, fullPath string) error {
	paths, err := json.Marshal([]string{fullPath})
	if err != nil {
		return err
	}
	var result api.ManageResponse
	err = f.postAPI(ctx, url.Values{"method": {"filemanager"}, "opera": {"delete"}}, url.Values{
		"async": {"0"}, "filelist": {string(paths)},
	}, &result, false)
	if notFound(err) {
		return fs.ErrorObjectNotFound
	}
	if err != nil {
		return err
	}
	for _, info := range result.Info {
		if err := responseError(info.Response); err != nil {
			return err
		}
	}
	return nil
}

// Fs returns the object's filesystem.
func (o *Object) Fs() fs.Info { return o.fs }

// Remote returns the object's relative path.
func (o *Object) Remote() string { return o.remote }

// String returns the object's relative path.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Size returns the file size in bytes.
func (o *Object) Size() int64 { return o.size }

// ModTime returns the local modification time, falling back to server time.
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }

// ID returns the Baidu file ID.
func (o *Object) ID() string { return strconv.FormatInt(o.id, 10) }

// Storable reports that this object is a file.
func (o *Object) Storable() bool { return true }

// Hash returns an unsupported-hash error.
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }

// SetModTime rejects metadata changes.
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }

// Update stages and uploads the source, preserving its modification time.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fullPath, err := o.fs.fullPath(o.remote)
	if err != nil {
		return err
	}
	if fullPath == "/" {
		return fs.ErrorIsDir
	}
	if src.Size() < 0 {
		return errors.New("baidupan: upload size must be known")
	}
	chunkSize := int64(o.fs.opt.ChunkSize)
	if (src.Size()-1)/chunkSize+1 > maxParts {
		return fmt.Errorf("baidupan: file exceeds %d upload chunks; increase --baidupan-chunk-size if your account allows", maxParts)
	}
	for _, option := range options {
		if option.Mandatory() {
			return fmt.Errorf("baidupan: unsupported upload option %v", option)
		}
	}
	in, wrap := accounting.UnWrap(in)
	tmp, hashes, err := stageUpload(ctx, in, src.Size(), chunkSize)
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if err := o.fs.Mkdir(ctx, path.Dir(o.remote)); err != nil {
		return err
	}
	if existing, lookupErr := o.fs.findItem(ctx, fullPath); lookupErr == nil && existing.IsDir != 0 {
		return fs.ErrorIsDir
	} else if lookupErr != nil && !errors.Is(lookupErr, fs.ErrorObjectNotFound) {
		return lookupErr
	}
	blocks, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	mtime := src.ModTime(ctx).Unix()
	form := url.Values{
		"path": {fullPath}, "size": {strconv.FormatInt(src.Size(), 10)}, "isdir": {"0"},
		"rtype": {"3"}, "block_list": {string(blocks)}, "local_mtime": {strconv.FormatInt(mtime, 10)},
	}
	preForm := url.Values{}
	for key, values := range form {
		preForm[key] = slices.Clone(values)
	}
	preForm.Set("autoinit", "1")
	var pre api.PrecreateResponse
	if err := o.fs.postAPI(ctx, url.Values{"method": {"precreate"}}, preForm, &pre, false); err != nil {
		return err
	}
	if pre.ReturnType != 1 && pre.ReturnType != 2 {
		return errors.New("baidupan: unexpected precreate return type")
	}
	if pre.ReturnType == 1 {
		if pre.UploadID == "" {
			return errors.New("baidupan: precreate returned no upload ID")
		}
		seen := make(map[int]bool)
		for _, part := range pre.BlockList {
			if part < 0 || part >= len(hashes) || seen[part] {
				return errors.New("baidupan: precreate returned invalid upload part numbers")
			}
			seen[part] = true
		}
		for _, part := range pre.BlockList {
			offset := int64(part) * chunkSize
			size := min(chunkSize, src.Size()-offset)
			var uploaded api.UploadPartResponse
			err := o.fs.callJSON(ctx, o.fs.upload, rest.Opts{
				Method: "POST", Parameters: url.Values{
					"method": {"upload"}, "type": {"tmpfile"}, "path": {fullPath},
					"uploadid": {pre.UploadID}, "partseq": {strconv.Itoa(part)},
				}, MultipartContentName: "file", MultipartFileName: "file",
			}, func() (io.Reader, int64) {
				return wrap(io.NewSectionReader(tmp, offset, size)), size
			}, &uploaded, true)
			if err != nil {
				return err
			}
			if !strings.EqualFold(uploaded.MD5, hashes[part]) {
				return fmt.Errorf("baidupan: checksum mismatch for upload part %d", part)
			}
		}
		form.Set("uploadid", pre.UploadID)
		var created api.CreateResponse
		if err := o.fs.postAPI(ctx, url.Values{"method": {"create"}}, form, &created, false); err != nil {
			return err
		}
		if created.Path != fullPath || created.Size != src.Size() || created.IsDir != 0 {
			return errors.New("baidupan: uploaded file path, size or type does not match the source")
		}
	}
	// A fresh listing also verifies rapid uploads and avoids create's server-time fields.
	item, err := o.fs.findItem(ctx, fullPath)
	if err != nil {
		return err
	}
	if item.IsDir != 0 || item.Size != src.Size() {
		return errors.New("baidupan: uploaded file metadata does not match the source")
	}
	*o = *o.fs.object(o.remote, *item)
	return nil
}

// stageUpload spools the source to disk and computes the required part checksums.
func stageUpload(ctx context.Context, in io.Reader, size, chunkSize int64) (tmp *os.File, hashes []string, err error) {
	tmp, err = os.CreateTemp("", "rclone-baidupan-upload-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	bufferPool := pool.Global()
	buf := bufferPool.Get()
	defer bufferPool.Put(buf)
	reader := readers.NewContextReader(ctx, in)
	for offset := int64(0); offset < size; offset += chunkSize {
		h := md5.New()
		length := min(chunkSize, size-offset)
		n, copyErr := io.CopyBuffer(io.MultiWriter(tmp, h), io.LimitReader(reader, length), buf)
		if copyErr != nil {
			return tmp, nil, copyErr
		}
		if n != length {
			return tmp, nil, io.ErrUnexpectedEOF
		}
		hashes = append(hashes, fmt.Sprintf("%x", h.Sum(nil)))
	}
	if size == 0 {
		hashes = []string{fmt.Sprintf("%x", md5.Sum(nil))}
	}
	var extra [1]byte
	if n, readErr := io.ReadFull(reader, extra[:]); n != 0 {
		return tmp, nil, errors.New("baidupan: source is larger than its declared size")
	} else if readErr != io.EOF {
		return tmp, nil, readErr
	}
	return tmp, hashes, nil
}

// Remove deletes this file using the synchronous file manager operation.
func (o *Object) Remove(ctx context.Context) error {
	fullPath, err := o.fs.fullPath(o.remote)
	if err != nil {
		return err
	}
	if fullPath == "/" {
		return fs.ErrorIsDir
	}
	return o.fs.deletePath(ctx, fullPath)
}

// Open obtains a fresh download link and opens the requested byte range.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.size == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	options = slices.Clone(options)
	fs.FixRangeOption(options, o.size)
	var result api.ListResponse
	err := o.fs.callAPI(ctx, "/multimedia", url.Values{
		"method": {"filemetas"}, "fsids": {"[" + o.ID() + "]"}, "dlink": {"1"},
	}, &result)
	if notFound(err) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(result.List) != 1 || result.List[0].ID != o.id || result.List[0].Dlink == "" {
		return nil, errors.New("baidupan: metadata response has no matching download link")
	}
	dlink, err := url.Parse(result.List[0].Dlink)
	if err != nil || dlink.Host == "" || (dlink.Scheme != "https" && dlink.Scheme != "http") || dlink.User != nil {
		return nil, errors.New("baidupan: invalid download link")
	}
	// Older API responses can contain HTTP links; tokens must travel over TLS.
	dlink.Scheme = "https"
	token, err := o.fs.accessToken(ctx, "")
	if err != nil {
		return nil, err
	}
	params := dlink.Query()
	params.Set("access_token", token)
	dlink.RawQuery = params.Encode()
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		var callErr error
		resp, callErr = o.fs.srv.Call(ctx, &rest.Opts{
			Method: "GET", RootURL: dlink.String(), Options: options,
			ExtraHeaders:  map[string]string{"User-Agent": "pan.baidu.com", "Accept-Encoding": "identity"},
			CheckRedirect: rest.RefuseHTTPSDowngradeRedirectFn,
		})
		return shouldRetry(ctx, resp, withoutURL(callErr))
	})
	if err != nil {
		return nil, err
	}
	if err := o.checkDownload(resp, options); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

func (o *Object) checkDownload(resp *http.Response, options []fs.OpenOption) error {
	var requested *fs.RangeOption
	for _, option := range options {
		if key, value := option.Header(); strings.EqualFold(key, "Range") {
			var err error
			requested, err = fs.ParseRangeOption(value)
			if err != nil {
				return err
			}
		}
	}
	if requested == nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("baidupan: expected HTTP 200, got %d", resp.StatusCode)
		}
		if resp.ContentLength >= 0 && resp.ContentLength != o.size {
			return errors.New("baidupan: download size does not match file metadata")
		}
		return nil
	}
	start, limit := requested.Decode(o.size)
	end := o.size - 1
	if limit >= 0 {
		end = min(start+limit-1, end)
	}
	want := fmt.Sprintf("bytes %d-%d/%d", start, end, o.size)
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want {
		return fmt.Errorf("baidupan: server did not honor the requested range (expected %s)", want)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
		return errors.New("baidupan: range response length is incorrect")
	}
	return nil
}

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
	_ fs.IDer   = (*Object)(nil)
)
