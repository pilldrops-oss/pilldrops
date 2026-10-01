package httpapi

import "net/http"

// The agent and integrations surfaces are not part of this build: these
// routes register nothing, so the feature packages need not be linked in.

func (s *Server) registerAgentRoutes(func(string, http.Handler)) {}

func (s *Server) registerIntegrationRoutes(func(string, http.Handler)) {}

// Chat is an agent front-end, so it goes with the agent.
func (s *Server) registerChatRoutes(func(string, http.Handler)) {}
