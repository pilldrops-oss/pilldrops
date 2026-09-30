//go:build core

package httpapi

import "net/http"

// Core build: the optional feature areas register nothing. A build tag rather
// than a config flag, so the feature packages are not linked in at all.

func (s *Server) registerAgentRoutes(func(string, http.Handler)) {}

func (s *Server) registerIntegrationRoutes(func(string, http.Handler)) {}

// Chat is an agent front-end, so it goes with the agent.
func (s *Server) registerChatRoutes(func(string, http.Handler)) {}
