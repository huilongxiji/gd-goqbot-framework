// Command goqbot-relay 是最轻量的信令/地址登记服务。
// 只保存 peer_id → IP:port + 更新时间，不存密钥、消息或其它敏感信息。
// 用于 P2P 打洞失败时查询对端当前地址。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type peer struct {
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	UpdatedAt time.Time `json:"updated_at"`
}

type store struct {
	mu    sync.RWMutex
	peers map[string]peer
}

func main() {
	addr := flag.String("listen", "0.0.0.0:8787", "HTTP listen address")
	flag.Parse()

	s := &store{peers: make(map[string]peer)}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/peers/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/peers/")
		id = strings.Trim(id, "/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "bad peer id", 400)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				IP   string `json:"ip"`
				Port int    `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", 400)
				return
			}
			if body.IP == "" {
				// 从连接窥探公网侧地址
				host, _, err := net.SplitHostPort(r.RemoteAddr)
				if err == nil {
					body.IP = host
				}
			}
			if body.Port == 0 {
				http.Error(w, "port required", 400)
				return
			}
			s.mu.Lock()
			s.peers[id] = peer{IP: body.IP, Port: body.Port, UpdatedAt: time.Now().UTC()}
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case http.MethodGet:
			s.mu.RLock()
			p, ok := s.peers[id]
			s.mu.RUnlock()
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	log.Printf("goqbot-relay listening on %s (stores peer_id -> ip:port only)", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
