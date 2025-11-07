package router

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codingeasygo/util/xmap"
)

// ChannelStatus 描述一个通道(slave)的配置与状态
type ChannelStatus struct {
	Name      string   `json:"name"`
	Config    xmap.M   `json:"config,omitempty"`
	Active    []xmap.M `json:"active,omitempty"`
	Connected int      `json:"connected"`
}

// AdminServer 提供管理后台 API
type AdminServer struct {
	service  *Service
	server   *http.Server
	listener net.Listener
	token    string
	username string
	password string
}

//go:embed admin_static/index.html
var adminStatic embed.FS

// NewAdminServer 创建管理后台实例
func NewAdminServer(service *Service) *AdminServer {
	return &AdminServer{service: service}
}

// Start 启动管理后台监听
func (a *AdminServer) Start(cfg AdminConfig) error {
	if len(cfg.Listen) < 1 {
		return nil
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/api/slaves", a.wrapAuth(a.handleSlaves))
	mux.HandleFunc("/admin/api/slaves/", a.wrapAuth(a.handleSlave))
	mux.HandleFunc("/admin/api/state", a.wrapAuth(a.handleState))
	mux.HandleFunc("/admin", a.wrapAuth(a.handleIndex))
	mux.HandleFunc("/admin/", a.wrapAuth(a.handleIndex))
	a.token = cfg.Token
	a.username = cfg.Username
	a.password = cfg.Password
	a.server = &http.Server{Handler: mux}
	a.listener = ln
	go func() {
		if err := a.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ErrorLog("Admin server exit with %v", err)
		}
	}()
	return nil
}

// Stop 关闭管理后台
func (a *AdminServer) Stop() {
	if a.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.server.Shutdown(ctx)
	if a.listener != nil {
		_ = a.listener.Close()
		a.listener = nil
	}
	a.server = nil
}

func (a *AdminServer) wrapAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorize(w, r) {
			return
		}
		next(w, r)
	}
}

func (a *AdminServer) authorize(w http.ResponseWriter, r *http.Request) bool {
	if len(a.token) > 0 && r.Header.Get("X-Admin-Token") == a.token {
		return true
	}
	if len(a.username) > 0 {
		user, pass, ok := r.BasicAuth()
		if ok && user == a.username && pass == a.password {
			return true
		}
		w.Header().Set("WWW-Authenticate", "Basic realm=\"bsck-admin\"")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if len(a.token) > 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (a *AdminServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := adminStatic.ReadFile("admin_static/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (a *AdminServer) handleState(w http.ResponseWriter, r *http.Request) {
	state := xmap.M{"channels": xmap.M{}, "table": []interface{}{}, "name": a.service.Name}
	if a.service != nil && a.service.Node != nil && a.service.Node.Router != nil {
		state = a.service.Node.Router.State()
	}
	respondJSON(w, http.StatusOK, state)
}

func (a *AdminServer) handleSlaves(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		statuses, err := a.service.ChannelStatusList()
		if err != nil {
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"channels": statuses})
	case http.MethodPost:
		name, cfg, err := parseChannelPayload(r)
		if err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		if err = a.service.UpsertChannel(name, cfg); err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		status, err := a.service.ChannelStatus(name)
		if err != nil {
			respondJSON(w, http.StatusCreated, map[string]string{"name": name})
			return
		}
		respondJSON(w, http.StatusCreated, status)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *AdminServer) handleSlave(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/api/slaves/")
	name = strings.TrimSuffix(name, "/")
	if len(name) < 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		status, err := a.service.ChannelStatus(name)
		if err != nil {
			respondError(w, http.StatusNotFound, err)
			return
		}
		respondJSON(w, http.StatusOK, status)
	case http.MethodPut, http.MethodPatch:
		_, cfg, err := parseChannelPayload(r)
		if err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		if err = a.service.UpsertChannel(name, cfg); err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		status, err := a.service.ChannelStatus(name)
		if err != nil {
			respondJSON(w, http.StatusOK, map[string]string{"name": name})
			return
		}
		respondJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := a.service.RemoveChannel(name); err != nil {
			respondError(w, http.StatusNotFound, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func parseChannelPayload(r *http.Request) (name string, cfg xmap.M, err error) {
	defer r.Body.Close()
	payload := map[string]interface{}{}
	if err = json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return
	}
	rawName, _ := payload["name"].(string)
	if inner, ok := payload["config"].(map[string]interface{}); ok {
		cfg = xmap.Wrap(inner)
	} else {
		cfg = xmap.Wrap(payload)
		_ = cfg.Delete("name")
	}
	name = strings.TrimSpace(rawName)
	if len(name) < 1 {
		if v, ok := cfg["name"].(string); ok {
			name = strings.TrimSpace(v)
			_ = cfg.Delete("name")
		}
	}
	if len(name) < 1 {
		err = errors.New("channel name is required")
		return
	}
	return
}

func respondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, err error) {
	respondJSON(w, status, map[string]string{"error": err.Error()})
}
