package panel

import (
	"net/http"

	"opback/internal/types"
)

type registerRequest struct {
	Name        string `json:"name"`
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Version     string `json:"version"`
	EnrollToken string `json:"enroll_token"`
}

type registerResponse struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	settings := s.store.Settings()
	tokenConfigured := settings.EnrollToken != ""
	if tokenConfigured && !constantTimeEqual(req.EnrollToken, settings.EnrollToken) {
		writeErr(w, http.StatusForbidden, "注册密钥无效")
		return
	}

	name := req.Name
	if name == "" {
		name = req.Hostname
	}

	// The peer address the agent is connecting from. This is observed from the
	// socket, not trusted from headers, so it cannot be spoofed by the caller.
	ip := clientIP(r)

	// Re-registering with a known name reuses the existing record (with a fresh
	// token) so a node that lost its local state does not show up twice. That
	// is only safe when the caller proved possession of the enrollment token;
	// otherwise an unauthenticated client could hijack another node's record —
	// and with it that node's jobs and OpenList credentials. Without an
	// enrollment token we therefore require the caller to present the existing
	// node's own token.
	presented := r.Header.Get("X-Node-Token")
	if tokenConfigured || presented != "" {
		for _, n := range s.store.ListNodes() {
			if n.Name != name {
				continue
			}
			if !tokenConfigured && !constantTimeEqual(n.Token, presented) {
				continue
			}
			if err := s.store.UpdateNode(n.ID, func(node *types.Node) {
				node.Token = newToken()
				node.Hostname = req.Hostname
				node.OS = req.OS
				node.Arch = req.Arch
				node.AgentVersion = req.Version
				node.IP = ip
			}); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			updated, _ := s.store.GetNode(n.ID)
			writeOK(w, registerResponse{NodeID: updated.ID, Token: updated.Token})
			return
		}
	}

	node, err := s.store.CreateNode(name, req.Hostname, req.OS, req.Arch, req.Version)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.SetNodeIP(node.ID, ip)
	writeOK(w, registerResponse{NodeID: node.ID, Token: node.Token})
}

func (s *Server) handleAgentSync(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Node-Token")
	node, err := s.store.NodeByToken(token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "节点令牌无效")
		return
	}

	var req types.SyncRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Persist any run progress reported by the agent, enriching each record
	// with details the agent does not know (names, source/destination).
	for _, rep := range req.Reports {
		job, _ := s.store.GetJob(rep.JobID)
		name := job.Name
		if name == "" {
			name = rep.JobID
		}
		run := rep.ToRun(name)
		run.NodeID = node.ID
		run.NodeName = node.Name
		run.Source = job.Source
		run.Dest = job.Dest
		if job.OpenListID != "" {
			run.OpenListName = s.store.OpenListName(job.OpenListID)
		}
		// Remember whether this run was already finished so a re-reported
		// terminal state does not push a duplicate notification.
		prev, hadPrev := s.store.GetRun(rep.RunID)
		if err := s.store.UpsertRun(run); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if isTerminalRun(run.Status) && (!hadPrev || !isTerminalRun(prev.Status)) {
			s.notifyRun(run)
		}
	}

	_ = s.store.TouchNode(node.ID, req.Hostname, req.OS, req.Arch, req.AgentVersion, clientIP(r))

	settings := s.store.Settings()

	// Hand every configured connection to the agent, keyed by ID, plus the
	// legacy single connection under "" for jobs that do not reference one.
	openlists := map[string]types.AgentConfig{}
	for _, o := range s.store.ListOpenLists() {
		openlists[o.ID] = types.AgentConfig{BaseURL: o.BaseURL, Username: o.Username, Password: o.Password}
	}
	if settings.OpenList.BaseURL != "" {
		if _, ok := openlists[""]; !ok {
			openlists[""] = settings.OpenList
		}
	}

	resp := types.SyncResponse{
		NodeID:       node.ID,
		SyncInterval: settings.SyncInterval,
		OpenLists:    openlists,
		Jobs:         s.store.ListJobs(node.ID),
	}
	resp.Commands = s.drainCommands(node.ID)

	writeOK(w, resp)
}

// handleAgentUninstall is called by an agent just before it removes itself. It
// deletes the node record so the panel reflects the uninstall immediately.
func (s *Server) handleAgentUninstall(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Node-Token")
	node, err := s.store.NodeByToken(token)
	if err != nil {
		// Already gone: treat as success so the agent can proceed.
		writeOK(w, map[string]bool{"ok": true})
		return
	}
	if err := s.store.DeleteNode(node.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// enqueueCommand appends a command for a node to pick up on its next sync.
func (s *Server) enqueueCommand(nodeID string, cmd types.Command) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	// Collapse duplicate "run" commands for the same job.
	for _, c := range s.commands[nodeID] {
		if c.Action == cmd.Action && c.JobID == cmd.JobID {
			return
		}
	}
	s.commands[nodeID] = append(s.commands[nodeID], cmd)
}

// drainCommands returns and clears all pending commands for a node.
func (s *Server) drainCommands(nodeID string) []types.Command {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	cmds := s.commands[nodeID]
	delete(s.commands, nodeID)
	if len(cmds) == 0 {
		return nil
	}
	return cmds
}
