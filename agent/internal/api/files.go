package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"ferrous/agent/internal/docker"
)

// handleFiles: GET /api/v1/servers/{id}/files?path=... · PUT same path.
//
// The data root comes from the container (ferrous.datadir label, else its
// single writable mount), so the endpoint can only ever touch paths the
// container itself has mounted. GET answers a directory listing or file
// content ({"encoding":"utf8"|"base64"}); PUT writes atomically.
//
// Status contract: 200 · 400 bad path/body · 403 unmanaged · 404 unknown
// container or missing path/data dir · 405 · 413 file/body too large ·
// 500 unclassified file IO · 502 engine trouble.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server id"})
		return
	}
	d, ok := s.inspectManaged(w, r, id)
	if !ok {
		return
	}
	root, err := docker.DataRoot(d)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	svc, err := docker.NewFileService(root)
	if err != nil {
		writeJSON(w, fileErrStatus(err), map[string]string{"error": err.Error()})
		return
	}

	switch r.Method {
	case http.MethodGet:
		fi, err := svc.Get(r.URL.Query().Get("path"))
		if err != nil {
			writeJSON(w, fileErrStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, fi)

	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		var req struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large (16 MB)"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `bad request body (want {"path","content","encoding"})`})
			return
		}
		data, err := docker.DecodeContent(req.Content, req.Encoding)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		fi, err := svc.Write(req.Path, data)
		if err != nil {
			writeJSON(w, fileErrStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": fi.Path, "size": fi.Size, "mtime": fi.MTime})

	default: // unreachable: mux routes only GET/PUT here (405 table covers the rest)
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func fileErrStatus(err error) int {
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, docker.ErrEscape), errors.Is(err, docker.ErrBadPath):
		return http.StatusBadRequest
	case errors.Is(err, docker.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, docker.ErrIsDir), errors.Is(err, docker.ErrNotDir):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
