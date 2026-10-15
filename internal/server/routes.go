package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/deployer"
	"github.com/krwu/compose-deploy-agent/internal/store"
)

// mainRouter builds the /v1/* mux served over mTLS on :8866, and over the
// local unix socket for `deploy-agent update`. /v1/version is also
// reachable here for callers that already hold a valid client
// certificate, even though its primary entry point is publicRouter.
func (s *Server) mainRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/deploy", s.handleDeploy)
	mux.HandleFunc("GET /v1/deploys/{id}", s.handleGetDeploy)
	mux.HandleFunc("GET /v1/status/{service}", s.handleGetStatus)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	return limitBody(mux)
}

// publicRouter builds the mux served over the plain-HTTP sub-stream of
// :8866 (see protoDemux): intentionally just /v1/version, so a pipeline
// can discover the running agent version - and thus which client binary
// to download - before it has any mTLS credential. Every other /v1/*
// route must stay unreachable without a client certificate; do not add
// routes here.
func (s *Server) publicRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	return limitBody(mux)
}

// healthzRouter builds the plain-HTTP mux served on 127.0.0.1:8867.
func (s *Server) healthzRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.HealthzResponse{
		Status:  "ok",
		Version: s.version,
		UptimeS: int64(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintln(w, s.version); err != nil {
		slog.Error("write version response failed", "error", err)
	}
}

func (server *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeDeployRequest(w, r)
	if !ok {
		return
	}
	if !server.authorizeService(w, r, req.Service) {
		return
	}
	if !server.consumeOIDCToken(w, r) {
		return
	}

	var deployID string
	var err error
	if req.Digest != "" {
		deployID, _, err = server.deployer.StartDeployDigest(r.Context(), req.Service, req.Digest)
	} else {
		deployID, _, err = server.deployer.StartDeploy(r.Context(), req.Service, req.Tag)
	}
	if err != nil {
		server.releaseOIDCToken(r)
		server.writeStartDeployError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, api.DeployAccepted{
		DeployID: deployID,
		Service:  req.Service,
		Tag:      req.Tag,
		Digest:   req.Digest,
		State:    api.StateDeploying,
	})
}

// decodeDeployRequest parses a POST /v1/deploy body strictly: unknown
// fields are rejected (the request can never smuggle in options such as a
// post_deploy user or command), the body must hold exactly one JSON value
// (only whitespace may follow it), and exactly one of tag and digest must
// be set. On failure it writes the error response and returns false.
func decodeDeployRequest(w http.ResponseWriter, r *http.Request) (api.DeployRequest, bool) {
	var req api.DeployRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err != nil {
		writeBodyError(w, err)
		return req, false
	}
	_, err = dec.Token()
	if !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected data after JSON value")
		}
		writeBodyError(w, err)
		return req, false
	}
	if req.Service == "" || (req.Tag == "") == (req.Digest == "") {
		writeError(w, http.StatusBadRequest, api.ErrInvalidParam,
			"service and exactly one of tag or digest are required")
		return req, false
	}
	return req, true
}

// writeBodyError maps a request body read/parse error to 413 (size limit)
// or 400.
func writeBodyError(w http.ResponseWriter, err error) {
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, api.ErrBodyTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, api.ErrInvalidParam, fmt.Sprintf("invalid JSON body: %v", err))
}

func (s *Server) writeStartDeployError(w http.ResponseWriter, err error) {
	var notFound deployer.ErrServiceNotFound
	var invalidTag deployer.ErrInvalidTag
	var conflict deployer.ErrAlreadyDeploying

	switch {
	case errors.As(err, &notFound):
		writeError(w, http.StatusNotFound, api.ErrServiceNotFound, notFound.Error())
	case errors.As(err, &invalidTag):
		writeError(w, http.StatusBadRequest, api.ErrInvalidParam, invalidTag.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, api.ErrAlreadyDeploying, conflict.Error())
	default:
		slog.Error("start deploy failed", "error", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal, "internal error")
	}
}

func (s *Server) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := s.deployer.GetDeploy(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, api.ErrServiceNotFound, "deploy not found")
			return
		}
		slog.Error("get deploy failed", "error", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal, "internal error")
		return
	}
	if !s.authorizeService(w, r, status.Service) {
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	if !s.authorizeService(w, r, service) {
		return
	}
	status, err := s.deployer.GetServiceStatus(r.Context(), service)
	if err != nil {
		var notFound deployer.ErrServiceNotFound
		if errors.As(err, &notFound) {
			writeError(w, http.StatusNotFound, api.ErrServiceNotFound, notFound.Error())
			return
		}
		slog.Error("get status failed", "error", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, status)
}
