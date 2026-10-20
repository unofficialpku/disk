// Package mockserver provides the shared in-memory AnyShare protocol fixture.
// It is used only by tests and explicit E2E tools, never by application builds.
package mockserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const Library = "gns://library"
const fixtureLibrary = Library

type Object struct {
	Entry       anyshare.Entry
	Data        []byte
	StreamDelay time.Duration // Optional pause between each 64 KiB of a download.
	Thumbnail   []byte        // Optional server-generated raster thumbnail.
}
type fixtureObject = Object
type fixtureAccount struct {
	objects     map[string]fixtureObject
	recycled    map[string]fixtureObject
	links       map[string]any
	permissions []any
	quota       anyshare.Quota
	arrivals    []Object
}
type Request struct {
	Path string
	Body map[string]any
}

type Server struct {
	tokens                     map[string]string
	refreshes                  int
	requests                   []Request
	URL                        string
	mu                         sync.Mutex
	server                     *httptest.Server
	users                      map[string]*fixtureAccount
	uploads                    map[string]fixtureObject
	mutations, calls, failures []string
	next                       int
}
type Token string

func (t Token) AccessToken() string                 { return "fixture-" + string(t) }
func (Token) Refresh(context.Context, string) error { return nil }

func New(names ...string) *Server {
	f := &Server{tokens: make(map[string]string), users: make(map[string]*fixtureAccount), uploads: make(map[string]fixtureObject)}
	for _, name := range names {
		f.addUser(name)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.server.URL
	return f
}
func (f *Server) Close()                 { f.server.Close() }
func (f *Server) AddAccount(name string) { f.mu.Lock(); defer f.mu.Unlock(); f.addUser(name) }
func (f *Server) Account(name string) accounts.Account {
	return accounts.Account{Name: name, Username: "student-" + name, Password: "private-fixture-password", Server: f.URL,
		Session: auth.State{AccessToken: "fixture-" + name, RefreshToken: "fixture-refresh-" + name}}
}
func (f *Server) PutObject(account string, object Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object.Data = append([]byte(nil), object.Data...)
	object.Thumbnail = append([]byte(nil), object.Thumbnail...)
	f.users[account].objects[object.Entry.ID] = object
}
func (f *Server) Objects(account string) []Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := []Object{}
	if user := f.users[account]; user != nil {
		for _, object := range user.objects {
			object.Data = append([]byte(nil), object.Data...)
			object.Thumbnail = append([]byte(nil), object.Thumbnail...)
			result = append(result, object)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Entry.ID < result[j].Entry.ID })
	return result
}
func (f *Server) Mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.mutations...)
}
func (f *Server) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}
func (f *Server) Failures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.failures...)
}

func (f *Server) addUser(name string) {
	if f.users[name] != nil {
		return
	}
	user := &fixtureAccount{objects: make(map[string]fixtureObject), recycled: make(map[string]fixtureObject), links: make(map[string]any), permissions: []any{}, quota: anyshare.Quota{Allocated: 10 << 30, Used: 2 << 30}}
	f.users[name] = user
	f.tokens["fixture-"+name] = name
	stamp := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	for _, entry := range []anyshare.Entry{
		{ID: fixtureLibrary, Name: "个人文档库", Dir: true, Size: -1, Type: "user_doc_lib"},
		{ID: fixtureLibrary + "/folder", Name: "课程资料", Dir: true, Size: -1},
		{ID: fixtureLibrary + "/backup", Name: "归档", Dir: true, Size: -1},
		{ID: fixtureLibrary + "/report", Name: "研究报告.txt", Size: 1536},
		{ID: fixtureLibrary + "/archive", Name: "资料.zip", Size: 1536},
		{ID: fixtureLibrary + "/folder/notes", Name: "读书笔记.md", Size: 1536},
	} {
		if name == "work" && entry.ID == fixtureLibrary+"/report" {
			entry.Name = "工作报告.txt"
		}
		entry.Rev, entry.Modified, entry.Created = "current", stamp, stamp
		object := fixtureObject{Entry: entry}
		if !entry.Dir {
			object.Data = bytes.Repeat([]byte("fixture!"), 192)
		}
		user.objects[entry.ID] = object
	}
	user.recycled[fixtureLibrary+"/bin/old"] = fixtureObject{Entry: anyshare.Entry{ID: fixtureLibrary + "/bin/old", Name: "已删除的笔记.txt", Size: 8, Rev: "old", Modified: stamp}, Data: []byte("restored")}
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	unlock := sync.OnceFunc(f.mu.Unlock)
	defer unlock()
	w.Header().Set("Content-Type", "application/json")
	f.calls = append(f.calls, r.URL.Path)
	if r.URL.Path == "/anyshare/oauth2/login/refreshToken" {
		cookie, err := r.Cookie("client.oauth2_refresh_token")
		if err != nil || !strings.HasPrefix(cookie.Value, "fixture-refresh-") {
			http.Error(w, "invalid test session", 401)
			return
		}
		name := strings.TrimPrefix(cookie.Value, "fixture-refresh-")
		if f.users[name] == nil {
			http.Error(w, "unknown test account", 401)
			return
		}
		f.refreshes++
		token := fmt.Sprintf("fixture-session-%d", f.refreshes)
		f.tokens[token] = name
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: token, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: cookie.Value, Path: "/"})
		_, _ = w.Write([]byte("{}"))
		return
	}
	if r.URL.Path == "/anyshare/oauth2/login" {
		http.Error(w, "test password login disabled", 503)
		return
	}
	account := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if strings.HasPrefix(r.URL.Path, "/objects") || r.URL.Path == "/package" || r.URL.Path == "/thumbnails" {
		account = r.URL.Query().Get("account")
	}
	u := f.users[account]
	if u == nil {
		http.Error(w, "invalid fixture account", 401)
		return
	}
	var body map[string]any
	if strings.HasPrefix(r.URL.Path, "/api/") && r.Method == "POST" {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.requests = append(f.requests, Request{Path: r.URL.Path, Body: body})
	get := func(key string) string { v, _ := body[key].(string); return v }
	respond := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	id := get("docid")
	if r.URL.Path == "/api/open-doc/v1/file-thumbnail" {
		for id, object := range u.objects {
			if id[strings.LastIndexByte(id, '/')+1:] != r.URL.Query().Get("id") {
				continue
			}
			if !anyshare.ThumbnailSupported(object.Entry.Name) || len(object.Thumbnail) == 0 {
				http.NotFound(w, r)
				return
			}
			respond(map[string]string{"url": f.URL + "/thumbnails?" + url.Values{"account": {account}, "id": {id}}.Encode()})
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/thumbnails" {
		object, ok := u.objects[r.URL.Query().Get("id")]
		if !ok || len(object.Thumbnail) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(object.Thumbnail))
		_, _ = w.Write(object.Thumbnail)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/objects") {
		id = r.URL.Query().Get("id")
		if r.Method == http.MethodPost {
			object := f.uploads[account+id]
			reader, err := r.MultipartReader()
			if err != nil {
				http.Error(w, "invalid multipart body", 400)
				return
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					http.Error(w, "invalid multipart body", 400)
					return
				}
				if part.FormName() == "file" {
					object.Data, _ = io.ReadAll(io.LimitReader(part, 32<<20))
				}
				part.Close()
			}
			f.uploads[account+id] = object
			w.WriteHeader(200)
			return
		}
		object, ok := u.objects[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		// Objects own their immutable bytes. Do not hold the fixture lock while
		// streaming, so other API calls and cancellation can proceed normally.
		unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		if object.StreamDelay > 0 {
			w = &delayedResponseWriter{ResponseWriter: w, ctx: r.Context(), delay: object.StreamDelay}
		}
		http.ServeContent(w, r, object.Entry.Name, object.Entry.Modified, bytes.NewReader(object.Data))
		return
	}
	if r.URL.Path == "/package" {
		var buffer bytes.Buffer
		z := zip.NewWriter(&buffer)
		file, _ := z.Create("课程资料/读书笔记.md")
		_, _ = file.Write(u.objects[fixtureLibrary+"/folder/notes"].Data)
		_ = z.Close()
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(buffer.Bytes())
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/efast/v1/folders/") {
		parent := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/efast/v1/folders/"), "/sub_objects")
		dirs, files := []anyshare.Entry{}, []anyshare.Entry{}
		for _, object := range u.objects {
			if anyshare.ParentID(object.Entry.ID) == parent {
				if object.Entry.Dir {
					dirs = append(dirs, object.Entry)
				} else {
					files = append(files, object.Entry)
				}
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		respond(map[string]any{"dirs": dirs, "files": files})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/efast/v1/quota/doc-lib/") {
		respond(u.quota)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/doc-share/v1/sharing-configuration-scope/") {
		respond(map[string]any{"validity_period": map[string]int{"default": 7, "maximum": -1}, "perm": map[string]any{"allow": []string{"display", "preview", "download", "create", "modify", "delete", "cache"}, "default": []string{"display", "preview", "download"}}, "access_times": map[string]int{"default": -1, "maximum": -1}, "must_set_password": false, "verify_mobile": map[string]bool{"enable": true, "force": false}})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/shared-link/v1/document/anonymous/") && r.Method == http.MethodDelete {
		delete(u.links, strings.TrimPrefix(r.URL.Path, "/api/shared-link/v1/document/anonymous/"))
		f.mutations = append(f.mutations, "remove_link")
		w.WriteHeader(204)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/shared-link/v1/document/file/") || strings.HasPrefix(r.URL.Path, "/api/shared-link/v1/document/folder/") {
		links := []any{}
		for _, raw := range u.links {
			link := raw.(map[string]any)
			if link["type"] == r.URL.Query().Get("type") {
				links = append(links, link)
			}
		}
		respond(links)
		return
	}
	switch r.URL.Path {
	case "/api/efast/v1/entry-doc-lib", "/api/efast/v1/owned-doc-lib":
		respond([]anyshare.Entry{u.objects[fixtureLibrary].Entry})
	case "/api/eacp/v1/perm1/checkall":
		respond(map[string]any{"allow": []string{"display", "preview", "download", "create", "modify", "delete", "cache"}})
	case "/api/efast/v1/file/attribute", "/api/efast/v1/dir/attribute":
		object, ok := u.objects[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		respond(object.Entry)
	case "/api/efast/v1/dir/create":
		f.next++
		newID := id + fmt.Sprintf("/created-%d", f.next)
		entry := anyshare.Entry{ID: newID, Name: get("name"), Dir: true, Size: -1, Rev: "new"}
		u.objects[newID] = fixtureObject{Entry: entry}
		f.mutations = append(f.mutations, "mkdir")
		respond(entry)
	case "/api/efast/v1/file/rename", "/api/efast/v1/dir/rename":
		object := u.objects[id]
		object.Entry.Name = get("name")
		u.objects[id] = object
		f.mutations = append(f.mutations, "rename")
		respond(object.Entry)
	case "/api/efast/v1/file/move", "/api/efast/v1/dir/move", "/api/efast/v1/file/copy", "/api/efast/v1/dir/copy":
		object := u.objects[id]
		f.next++
		object.Entry.ID = get("destparent") + fmt.Sprintf("/copy-%d", f.next)
		u.objects[object.Entry.ID] = object
		operation := "copy"
		if strings.HasSuffix(r.URL.Path, "/move") {
			delete(u.objects, id)
			operation = "move"
		}
		f.mutations = append(f.mutations, operation)
		respond(map[string]any{"docid": object.Entry.ID, "name": object.Entry.Name, "rev": "new"})
	case "/api/efast/v1/file/delete", "/api/efast/v1/dir/delete":
		object := u.objects[id]
		delete(u.objects, id)
		object.Entry.ID = strings.Replace(id, fixtureLibrary+"/", fixtureLibrary+"/bin/", 1)
		u.recycled[object.Entry.ID] = object
		f.mutations = append(f.mutations, account+":delete")
		w.WriteHeader(204)
	case "/api/ecosearch/v1/file-search":
		rows := []any{}
		for _, object := range u.objects {
			if strings.Contains(object.Entry.Name, get("keyword")) {
				rows = append(rows, map[string]any{"doc_id": object.Entry.ID, "basename": object.Entry.Name, "extension": "", "size": object.Entry.Size, "parent_path": "/个人文档库", "modified_at": object.Entry.Modified.UnixMicro()})
			}
		}
		respond(map[string]any{"files": rows, "hits": len(rows), "next": len(rows)})
	case "/api/efast/v1/file/osdownload":
		respond(map[string]any{"authrequest": []string{"GET", f.URL + "/objects?account=" + url.QueryEscape(account) + "&id=" + url.QueryEscape(id)}})
	case "/api/efast/v1/file/osoption":
		respond(map[string]any{"partminsize": 5 << 20, "partmaxsize": 5 << 30, "partmaxnum": 10000})
	case "/api/efast/v1/file/predupload":
		respond(map[string]any{"match": false})
	case "/api/efast/v1/file/osbeginupload":
		f.next++
		newID := id + fmt.Sprintf("/upload-%d", f.next)
		size, _ := body["length"].(float64)
		entry := anyshare.Entry{ID: newID, Name: get("name"), Size: int64(size), Rev: "uploaded"}
		f.uploads[account+newID] = fixtureObject{Entry: entry}
		respond(map[string]any{"docid": newID, "name": entry.Name, "rev": entry.Rev, "authrequest": []string{"POST", f.URL + "/objects?account=" + url.QueryEscape(account) + "&id=" + url.QueryEscape(newID)}})
	case "/api/efast/v1/file/osendupload":
		u.objects[id] = f.uploads[account+id]
		delete(f.uploads, account+id)
		f.mutations = append(f.mutations, "upload")
		w.WriteHeader(204)
	case "/api/open-doc/v1/file-download":
		respond(map[string]any{"progress": 100, "package_address": "/package?account=" + url.QueryEscape(account), "items": []any{map[string]string{"status": "completed"}}})
	case "/api/open-doc/v1/file-unarchiver":
		parent := get("target_path")
		f.next++
		entry := anyshare.Entry{ID: parent + fmt.Sprintf("/extracted-%d", f.next), Name: get("name"), Size: -1, Dir: true, Rev: "new"}
		u.objects[entry.ID] = fixtureObject{Entry: entry}
		f.mutations = append(f.mutations, "extract")
		respond(map[string]any{"progress": 100, "status": "complete", "unarchive_path": entry.ID})
	case "/api/efast/v1/recycle/list":
		ids := []string{}
		for id := range u.recycled {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		start, _ := body["start"].(float64)
		limit, _ := body["limit"].(float64)
		if start < 0 || limit < 1 {
			http.Error(w, "invalid pagination", 400)
			return
		}
		dirs, files := []any{}, []any{}
		for _, id := range ids[min(int(start), len(ids)):min(int(start+limit), len(ids))] {
			object := u.recycled[id]
			row := map[string]any{"docid": object.Entry.ID, "name": object.Entry.Name, "size": object.Entry.Size, "path": "/" + u.objects[fixtureLibrary].Entry.Name, "modified": time.Now().UnixMicro()}
			if object.Entry.Dir {
				dirs = append(dirs, row)
			} else {
				files = append(files, row)
			}
		}
		respond(map[string]any{"dirs": dirs, "files": files})
	case "/api/efast/v1/recycle/restore":
		object := u.recycled[id]
		delete(u.recycled, id)
		object.Entry.ID = strings.Replace(id, "/bin/", "/", 1)
		u.objects[object.Entry.ID] = object
		f.mutations = append(f.mutations, "restore")
		respond(object.Entry)
	case "/api/efast/v1/recycle/delete":
		delete(u.recycled, id)
		for _, object := range u.arrivals {
			u.recycled[object.Entry.ID] = object
		}
		u.arrivals = nil
		f.mutations = append(f.mutations, "purge")
		w.WriteHeader(204)
	case "/api/efast/v1/file/revisions":
		respond([]any{map[string]any{"rev": "current", "name": "研究报告.txt", "editor": "测试用户", "size": 1536, "modified": time.Now().UnixMicro()}, map[string]any{"rev": "old", "name": "研究报告.txt", "editor": "测试用户", "size": 1024, "modified": time.Now().Add(-24 * time.Hour).UnixMicro()}})
	case "/api/efast/v1/file/restorerevision":
		f.mutations = append(f.mutations, "revision:"+get("rev"))
		w.WriteHeader(204)
	case "/api/eacp/v1/department/search":
		respond(map[string]any{"userinfos": []any{map[string]string{"userid": "recipient", "name": "李同学", "account": "student-li", "deppath": "计算机学院"}}, "depinfos": []any{map[string]string{"depid": "department", "name": "课程组", "path": "课程组"}}})
	case "/api/eacp/v1/perm2/get":
		respond(map[string]any{"inherit": true, "perminfos": u.permissions})
	case "/api/eacp/v1/perm2/set":
		u.permissions, _ = body["perminfos"].([]any)
		f.mutations = append(f.mutations, "revoke")
		respond(map[string]any{"result": 0})
	case "/api/doc-share/v1/doc-perm":
		u.permissions = []any{map[string]any{"accessorid": "recipient", "accessortype": "user", "accessorname": "李同学", "allow": []string{"display", "download"}, "deny": []string{}, "endtime": -1, "inheritdocid": ""}}
		f.mutations = append(f.mutations, "grant")
		w.WriteHeader(204)
	case "/api/shared-link/v1/document/realname", "/api/shared-link/v1/document/anonymous":
		kind := "realname"
		if strings.HasSuffix(r.URL.Path, "/anonymous") {
			kind = "anonymous"
		}
		linkID := kind + "-link"
		link := map[string]any{"id": linkID, "type": kind, "title": "研究报告.txt", "limited_times": -1, "item": map[string]any{"allow": []string{"display", "download"}}}
		if kind == "anonymous" {
			for key, value := range body {
				link[key] = value
			}
		}
		u.links[linkID] = link
		f.mutations = append(f.mutations, kind)
		respond(map[string]string{"id": linkID})
	default:
		f.failures = append(f.failures, fmt.Sprintf("unhandled fixture endpoint: %s %s", r.Method, r.URL.Path))
		http.NotFound(w, r)
	}
}

func (f *Server) ReplaceObjects(account string, objects []Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[account].objects = make(map[string]Object)
	for _, object := range objects {
		object.Data = append([]byte(nil), object.Data...)
		f.users[account].objects[object.Entry.ID] = object
	}
}
func (f *Server) SetQuota(account string, quota anyshare.Quota) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[account].quota = quota
}
func (f *Server) ReplaceRecycled(account string, objects []Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[account].recycled = make(map[string]Object)
	for _, object := range objects {
		f.users[account].recycled[object.Entry.ID] = object
	}
}
func (f *Server) Recycled(account string) []Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := []Object{}
	for _, object := range f.users[account].recycled {
		result = append(result, object)
	}
	return result
}

// ArriveAfterDelete simulates a concurrent deletion after the reviewed snapshot.
func (f *Server) ArriveAfterDelete(account string, objects ...Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[account].arrivals = append([]Object(nil), objects...)
}
func (f *Server) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := json.Marshal(f.requests)
	var result []Request
	_ = json.Unmarshal(data, &result)
	return result
}
