package server

import (
	"fmt"
	"net/http"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Protocol negotiation. Every poll carries the agent's protocol version, and every answer
// the range this server accepts. A server is upgraded before its agents: it accepts every agent from
// minProtocol on. An older agent's polls are refused with 426, so it gets no new config and no commands; it
// keeps backing up on its cached config, and its reports are still taken, so the server still sees its
// backups. The host shows the agent_too_old problem until the agent is upgraded.

// AgentTooOldError refuses a poll from an agent older than the server accepts.
type AgentTooOldError struct {
	Agent int
	Range proto.ProtocolRange
}

func (e *AgentTooOldError) Error() string {
	return fmt.Sprintf("this agent speaks protocol %d, and the server accepts %d to %d: upgrade holdfast-agent", e.Agent, e.Range.Min, e.Range.Max)
}

// protocolRange is what this server accepts.
func (s *Server) protocolRange() proto.ProtocolRange {
	return proto.ProtocolRange{Min: s.minProtocol, Max: proto.Protocol}
}

// checkProtocol refuses an agent older than the server accepts.
func (s *Server) checkProtocol(f proto.Facts) error {
	if p := f.AgentProtocol(); p < s.minProtocol {
		return &AgentTooOldError{Agent: p, Range: s.protocolRange()}
	}
	return nil
}

// agentTooOld says whether an agent's last poll was too old for this server.
func (s *Server) agentTooOld(a *store.Agent) bool {
	return a != nil && a.LastSeenAt != nil && proto.Facts{Protocol: a.Protocol}.AgentProtocol() < s.minProtocol
}

// writeTooOld answers 426 with the range, which the agent logs.
func writeTooOld(w http.ResponseWriter, e *AgentTooOldError) {
	writeJSON(w, http.StatusUpgradeRequired, proto.TooOld{Error: e.Error(), Protocol: e.Range})
}
