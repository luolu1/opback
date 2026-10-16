package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"opback/internal/openlist"
	"opback/internal/types"
)

// ---- OpenList connections ------------------------------------------------

// openListView masks the password while telling the UI whether one is stored.
type openListView struct {
	types.OpenList
	PasswordSet bool `json:"password_set"`
}

func openListViewOf(o types.OpenList) openListView {
	v := openListView{OpenList: o, PasswordSet: o.Password != ""}
	v.Password = ""
	return v
}

func (s *Server) handleListOpenLists(w http.ResponseWriter, r *http.Request) {
	ols := s.store.ListOpenLists()
	out := make([]openListView, 0, len(ols))
	for _, o := range ols {
		out = append(out, openListViewOf(o))
	}
	writeOK(w, out)
}

func (s *Server) handleGetOpenList(w http.ResponseWriter, r *http.Request) {
	o, err := s.store.GetOpenList(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "OpenList 连接不存在")
		return
	}
	writeOK(w, openListViewOf(o))
}

type openListRequest struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (req openListRequest) normalized() (types.OpenList, error) {
	url := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if url == "" {
		return types.OpenList{}, errString("OpenList 地址不能为空")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = url
	}
	return types.OpenList{
		Name:     name,
		BaseURL:  url,
		Username: strings.TrimSpace(req.Username),
		Password: req.Password,
	}, nil
}

func (s *Server) handleCreateOpenList(w http.ResponseWriter, r *http.Request) {
	var req openListRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ol, err := req.normalized()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.store.CreateOpenList(ol)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, openListViewOf(created))
}

func (s *Server) handleUpdateOpenList(w http.ResponseWriter, r *http.Request) {
	var req openListRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ol, err := req.normalized()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateOpenList(r.PathValue("id"), func(o *types.OpenList) {
		o.Name = ol.Name
		o.BaseURL = ol.BaseURL
		o.Username = ol.Username
		if req.Password != "" {
			o.Password = req.Password
		}
		// Credentials may have changed, so the last test result is stale.
		o.Status = "unknown"
		o.Message = ""
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "OpenList 连接不存在")
		return
	}
	writeOK(w, openListViewOf(updated))
}

func (s *Server) handleDeleteOpenList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if n := s.store.CountJobsByOpenList(id); n > 0 && r.URL.Query().Get("force") != "1" {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("仍有 %d 个备份任务在使用该连接，请先改用其它连接，或使用强制删除", n))
		return
	}
	if err := s.store.DeleteOpenList(id); err != nil {
		writeErr(w, http.StatusNotFound, "OpenList 连接不存在")
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// openListClientByID resolves a connection into a client. An empty ID falls
// back to the legacy single connection or, failing that, the only connection.
func (s *Server) openListClientByID(id string) (*openlist.Client, error) {
	if id != "" {
		o, err := s.store.GetOpenList(id)
		if err != nil {
			return nil, errString("OpenList 连接不存在")
		}
		return openlist.New(o.BaseURL, o.Username, o.Password), nil
	}
	if set := s.store.Settings(); set.OpenList.BaseURL != "" {
		return openlist.New(set.OpenList.BaseURL, set.OpenList.Username, set.OpenList.Password), nil
	}
	if list := s.store.ListOpenLists(); len(list) > 0 {
		o := list[0]
		return openlist.New(o.BaseURL, o.Username, o.Password), nil
	}
	return nil, errString("尚未配置任何 OpenList 连接")
}

func (s *Server) handleTestOpenList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, err := s.store.GetOpenList(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "OpenList 连接不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if err := openlist.New(o.BaseURL, o.Username, o.Password).Test(ctx); err != nil {
		_ = s.store.SetOpenListStatus(id, "error", err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.store.SetOpenListStatus(id, "ok", "连接正常")
	updated, _ := s.store.GetOpenList(id)
	writeOK(w, openListViewOf(updated))
}

func (s *Server) handleBrowseOpenList(w http.ResponseWriter, r *http.Request) {
	c, err := s.openListClientByID(r.URL.Query().Get("openlist_id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := c.List(ctx, dir, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// The UI only needs directories to pick a destination.
	dirs := make([]openlist.FsObject, 0, len(res.Content))
	for _, e := range res.Content {
		if e.IsDir {
			dirs = append(dirs, e)
		}
	}
	writeOK(w, map[string]any{"path": dir, "dirs": dirs})
}
