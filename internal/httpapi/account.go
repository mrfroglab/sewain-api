package httpapi

import (
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// exportMe is GET /api/v1/me/export: all data of the signed-in user as one JSON download (UU PDP).
func (s *Server) exportMe(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	data, err := s.svc.Export(r.Context(), v.UserID)
	if err != nil {
		s.log.Error("export", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menyiapkan data.")
		return
	}
	if data == nil {
		writeErr(w, http.StatusNotFound, "Akun tidak ditemukan.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="sewain-data-`+v.UserID+`.json"`)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "export": data})
}

// deleteMe is DELETE /api/v1/me: anonymises the account (see service.DeleteAccount) and signs the browser out.
func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	if _, err := s.svc.DeleteAccount(r.Context(), v.UserID); err != nil {
		if msg, ok := service.IsUserError(err); ok {
			writeErr(w, http.StatusConflict, msg)
			return
		}
		s.log.Error("delete account", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menghapus akun.")
		return
	}
	s.deps.Auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminMetrics is GET /api/v1/admin/metrics (PRD §9.8, §17).
func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.AdminMetrics(r.Context())
	if err != nil {
		s.log.Error("admin metrics", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal memuat metrik.")
		return
	}
	out := map[string]any{"ok": true}
	for k, v := range m {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// importListings is POST /api/v1/admin/listings/import: a CSV (multipart field "file", or the raw body with
// Content-Type text/csv) of listings for Early Lender onboarding (PRD §18.1). ?dryRun=1 (or form field dryRun)
// validates without writing. Any row error writes nothing; the report lists every row.
func (s *Server) importListings(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	dry := isTrue(r.URL.Query().Get("dryRun"))
	var src io.Reader = r.Body
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "multipart/form-data" {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			if middleware.IsBodyTooLarge(err) {
				middleware.RespondTooLarge(w)
				return
			}
			writeErr(w, http.StatusBadRequest, "Unggahan tidak valid.")
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		dry = dry || isTrue(r.FormValue("dryRun"))
		f, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "File CSV wajib (field \"file\").")
			return
		}
		defer f.Close()
		src = f
	}
	rep, _, err := s.svc.ImportListings(r.Context(), v.UserID, src, dry)
	if err != nil {
		if middleware.IsBodyTooLarge(err) {
			middleware.RespondTooLarge(w)
			return
		}
		if msg, ok := service.IsUserError(err); ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg, "columns": service.ImportColumns})
			return
		}
		s.log.Error("import listings", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal mengimpor.")
		return
	}
	status, body := http.StatusOK, map[string]any{"ok": rep.Failed == 0, "dryRun": rep.DryRun, "created": rep.Created, "failed": rep.Failed, "rows": rep.Rows}
	if rep.Failed > 0 {
		status = http.StatusUnprocessableEntity
		body["error"] = "Ada baris yang salah. Tidak ada yang disimpan; perbaiki lalu unggah ulang."
	}
	writeJSON(w, status, body)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "ya", "yes":
		return true
	}
	return false
}
