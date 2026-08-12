package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	projectRoutesPath       = "/v0/management/bravo/project-routes"
	projectRoutesPublicPath = "/v1/bravo/routes"
)

type projectRoutesDocumentation struct {
	Endpoint             string `json:"endpoint"`
	Method               string `json:"method"`
	CommandTemplate      string `json:"command_template"`
	AuthenticationHeader string `json:"authentication_header"`
}

type projectRouteView struct {
	ID           string                      `json:"id"`
	RequestModel string                      `json:"request_model"`
	DisplayName  string                      `json:"display_name"`
	Description  string                      `json:"description,omitempty"`
	Source       string                      `json:"source"`
	Candidates   []projectRouteCandidateView `json:"candidates"`
}

type projectRouteCandidateView struct {
	Order         int      `json:"order"`
	Role          string   `json:"role"`
	Provider      string   `json:"provider"`
	PhysicalModel string   `json:"physical_model"`
	Effort        string   `json:"effort,omitempty"`
	Priority      int      `json:"priority"`
	Capabilities  []string `json:"capabilities"`
}

type projectRoutePolicyView struct {
	CandidateOrder string `json:"candidate_order"`
	AccountScope   string `json:"account_scope"`
	FallbackUntil  string `json:"fallback_until"`
}

type projectRoutesResponse struct {
	SchemaVersion int                      `json:"schema_version"`
	Object        string                   `json:"object"`
	Project       projectLimitsProjectView `json:"project"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Prefix        string                   `json:"prefix"`
	Policy        projectRoutePolicyView   `json:"policy"`
	Routes        []projectRouteView       `json:"routes"`
}

func projectRoutesDocs() projectRoutesDocumentation {
	return projectRoutesDocumentation{
		Endpoint:             projectRoutesPublicPath,
		Method:               http.MethodGet,
		CommandTemplate:      "curl -sS '<BRAVO_BASE_URL>/v1/bravo/routes' -H 'Authorization: Bearer <PROJECT_KEY>'",
		AuthenticationHeader: "Authorization: Bearer <PROJECT_KEY>",
	}
}

func handleProjectRoutes(req rpcManagementRequest) ([]byte, error) {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path != projectRoutesPath {
		return nil, nil
	}
	if req.Method != http.MethodGet {
		return projectLimitsError(http.StatusMethodNotAllowed, "bravo_project_routes_method_not_allowed", "Маршруты проекта поддерживают только GET.", time.Time{})
	}
	cfg := loadedConfig()
	project, authenticated := matchSmartKey(cfg, requestCredential(req.Headers, req.Query))
	if !authenticated {
		return projectLimitsError(http.StatusUnauthorized, "bravo_smart_key_required", "Для получения маршрутов нужен действующий ключ проекта Bravo.", time.Time{})
	}
	overridden := make(map[string]struct{}, len(cfg.RouteOverrides))
	for _, item := range cfg.RouteOverrides {
		overridden[item.ID] = struct{}{}
	}
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		if smartKeyAllowsModel(project, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	routes := make([]projectRouteView, 0, len(names))
	for _, name := range names {
		model := cfg.Models[name]
		source := "default"
		if _, ok := overridden[name]; ok {
			source = "override"
		}
		routes = append(routes, projectRouteView{
			ID:           name,
			RequestModel: cfg.Prefix + name,
			DisplayName:  model.DisplayName,
			Description:  model.Description,
			Source:       source,
			Candidates:   projectRouteCandidateViews(model.Candidates),
		})
	}
	return projectLimitsJSON(http.StatusOK, projectRoutesResponse{
		SchemaVersion: 1,
		Object:        "bravo.project_routes",
		Project:       projectLimitsProjectView{ID: project.ID, Name: project.Name},
		GeneratedAt:   projectLimitsNow().UTC(),
		Prefix:        cfg.Prefix,
		Policy: projectRoutePolicyView{
			CandidateOrder: "listed_order",
			AccountScope:   "project_allowed_pool",
			FallbackUntil:  "first_response_payload",
		},
		Routes: routes,
	}, 0)
}

func projectRouteCandidateViews(items []candidate) []projectRouteCandidateView {
	out := make([]projectRouteCandidateView, 0, len(items))
	for index, item := range items {
		role := "fallback"
		if index == 0 {
			role = "preferred"
		}
		out = append(out, projectRouteCandidateView{
			Order:         index + 1,
			Role:          role,
			Provider:      normalizeProvider(item.Provider),
			PhysicalModel: item.Model,
			Effort:        item.Effort,
			Priority:      item.Priority,
			Capabilities:  append([]string(nil), item.Capabilities...),
		})
	}
	return out
}
