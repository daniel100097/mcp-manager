package main

import (
	"fmt"
	"reflect"

	"github.com/daniel100097/mcp-manager/internal/config"
)

// moveMCPGlobal transfers one project's assignment and its exclusions to
// global scope. Other explicitly configured projects keep their assignments.
func moveMCPGlobal(cfg *config.Config, name, projectID string) (bool, error) {
	project := cfg.Projects[projectID]
	index := stringIndex(project.MCPs, name)
	global := stringIndex(cfg.Global.MCPs, name) >= 0
	if index < 0 {
		if global {
			return false, nil
		}
		return false, fmt.Errorf("MCP %q is not active in project %q; use 'enable %s --global' to activate it globally", name, projectID, name)
	}
	if !global {
		cfg.Global.MCPs = append(cfg.Global.MCPs, name)
	}
	if cfg.Global.DisabledAgents == nil {
		cfg.Global.DisabledAgents = map[string][]config.Agent{}
	}
	if !global && len(project.DisabledAgents[name]) > 0 {
		cfg.Global.DisabledAgents[name] = append([]config.Agent(nil), project.DisabledAgents[name]...)
	}
	project.MCPs = append(project.MCPs[:index], project.MCPs[index+1:]...)
	delete(project.DisabledAgents, name)
	cfg.Projects[projectID] = project
	return true, nil
}

func toggleMCPScope(cfg *config.Config, name, projectID string, global, enabled bool, agent config.Agent) (bool, error) {
	scope := cfg.Global
	if !global {
		project := cfg.Projects[projectID]
		scope = config.Scope{MCPs: project.MCPs, DisabledAgents: project.DisabledAgents}
	}
	beforeNames := append([]string{}, scope.MCPs...)
	beforeExclusions := append([]config.Agent{}, scope.DisabledAgents[name]...)
	index := stringIndex(scope.MCPs, name)
	if scope.DisabledAgents == nil {
		scope.DisabledAgents = map[string][]config.Agent{}
	}
	if agent == "" {
		if enabled && index < 0 {
			scope.MCPs = append(scope.MCPs, name)
		} else if !enabled && index >= 0 {
			scope.MCPs = append(scope.MCPs[:index], scope.MCPs[index+1:]...)
			delete(scope.DisabledAgents, name)
		}
	} else if enabled {
		if index < 0 {
			scope.MCPs = append(scope.MCPs, name)
			for _, other := range []config.Agent{config.AgentCodex, config.AgentClaude, config.AgentOpenCode} {
				if other != agent {
					scope.DisabledAgents[name] = append(scope.DisabledAgents[name], other)
				}
			}
		}
		var exclusions []config.Agent
		for _, excluded := range scope.DisabledAgents[name] {
			if excluded != agent {
				exclusions = append(exclusions, excluded)
			}
		}
		delete(scope.DisabledAgents, name)
		if len(exclusions) > 0 {
			scope.DisabledAgents[name] = exclusions
		}
	} else if index >= 0 {
		found := false
		for _, excluded := range scope.DisabledAgents[name] {
			found = found || excluded == agent
		}
		if !found {
			scope.DisabledAgents[name] = append(scope.DisabledAgents[name], agent)
		}
	}
	changed := !reflect.DeepEqual(beforeNames, append([]string{}, scope.MCPs...)) ||
		!reflect.DeepEqual(beforeExclusions, append([]config.Agent{}, scope.DisabledAgents[name]...))
	if global {
		cfg.Global = scope
	} else {
		project := cfg.Projects[projectID]
		project.MCPs, project.DisabledAgents = scope.MCPs, scope.DisabledAgents
		cfg.Projects[projectID] = project
	}
	return changed, nil
}
