// Package api contains Baidu Netdisk API response types.
package api

import (
	"encoding/json"
	"fmt"
)

// Response contains the status shared by XPan responses.
type Response struct {
	Errno     *int            `json:"errno"`
	ErrorCode *int            `json:"error_code"`
	Errmsg    string          `json:"errmsg"`
	ErrorMsg  string          `json:"error_msg"`
	RequestID json.RawMessage `json:"request_id"`
}

// Error describes an unsuccessful API request.
type Error struct {
	Code    int
	Message string
}

// Error returns the API error code and message.
func (e *Error) Error() string {
	return fmt.Sprintf("baidupan: API error %d: %s", e.Code, e.Message)
}

// Item describes a file or directory returned by XPan.
type Item struct {
	ID             int64  `json:"fs_id"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Size           int64  `json:"size"`
	IsDir          int    `json:"isdir"`
	LocalMtime     int64  `json:"local_mtime"`
	ServerMtime    int64  `json:"server_mtime"`
	Dlink          string `json:"dlink"`
}

// ListResponse contains one page of directory entries or file metadata.
type ListResponse struct {
	Response
	List []Item `json:"list"`
}

// TokenResponse contains a Baidu OAuth token or an OAuth error.
type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// PrecreateResponse identifies an upload and the parts the server needs.
type PrecreateResponse struct {
	Response
	UploadID   string `json:"uploadid"`
	BlockList  []int  `json:"block_list"`
	ReturnType int    `json:"return_type"`
}

// UploadPartResponse contains the checksum of a received part.
type UploadPartResponse struct {
	Response
	MD5 string `json:"md5"`
}

// CreateResponse describes a created file or directory.
type CreateResponse struct {
	Response
	Item
}

// ManageResponse contains the individual results of a synchronous file operation.
type ManageResponse struct {
	Response
	Info []struct {
		Response
		Path string `json:"path"`
	} `json:"info"`
}
