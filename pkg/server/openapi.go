package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"

	"gopkg.in/yaml.v3"
)

// openapi.yaml describes every endpoint the daemon can serve (a test holds it to the routes in
// server.go). It is served, without a token, at /openapi.yaml and /openapi.json, with info.version set
// to the running release; it is the same for every daemon, so it says nothing about this host.
//
//go:embed openapi.yaml
var openapiYAML []byte

// openapiDoc is the document with this daemon's version, as YAML and as JSON.
func openapiDoc(version string) (yamlDoc, jsonDoc []byte, err error) {
	var doc map[string]any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		return nil, nil, fmt.Errorf("openapi.yaml: %w", err)
	}
	if info, ok := doc["info"].(map[string]any); ok && version != "" {
		info["version"] = version
	}
	if yamlDoc, err = yaml.Marshal(doc); err != nil {
		return nil, nil, err
	}
	if jsonDoc, err = json.MarshalIndent(doc, "", "  "); err != nil {
		return nil, nil, err
	}
	return yamlDoc, jsonDoc, nil
}

func (s *Server) openapiHandlers() (yamlH, jsonH http.HandlerFunc) {
	y, j, err := openapiDoc(s.opts.Version)
	serve := func(body []byte, ct string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "the API description is broken in this build")
				return
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Access-Control-Allow-Origin", "*") // public and static: a docs viewer may load it
			_, _ = w.Write(body)
		}
	}
	return serve(y, "application/yaml; charset=utf-8"), serve(j, "application/json")
}
