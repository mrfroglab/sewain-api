package kyc

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/mrfroglab/sewain-api/internal/auth"
)

// Handlers exposes the KYC HTTP endpoints. Mount them on a mux wrapped by auth.Service.Middleware.
type Handlers struct{ S *Service }

// Register mounts the KYC routes. User routes need a session; admin routes need role admin.
func (h Handlers) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/kyc/submissions", auth.RequireUserFunc(h.Submit))
	mux.Handle("GET /api/v1/kyc/status", auth.RequireUserFunc(h.Status))
	mux.Handle("GET /api/v1/admin/kyc", auth.RequireAdminFunc(h.Queue))
	mux.Handle("GET /api/v1/admin/kyc/{id}", auth.RequireAdminFunc(h.GetOne))
	mux.Handle("POST /api/v1/admin/kyc/{id}/decision", auth.RequireAdminFunc(h.Decide))
	mux.Handle("GET /api/v1/admin/kyc/{id}/files/{kind}", auth.RequireAdminFunc(h.File))
	mux.Handle("POST /api/v1/admin/kyc/blacklist", auth.RequireAdminFunc(h.Blacklist))
}

func readPart(fh *multipart.FileHeader) (File, error) {
	if fh.Size > MaxFileBytes {
		return File{}, errFileTooBig
	}
	f, err := fh.Open()
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return File{}, err
	}
	if len(b) > MaxFileBytes {
		return File{}, errFileTooBig
	}
	return File{Data: b, ContentType: fh.Header.Get("Content-Type")}, nil
}

// Submit handles POST /api/v1/kyc/submissions (multipart: ktp, selfie, nik, nameOnKtp).
func (h Handlers) Submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2*MaxFileBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			auth.WriteError(w, errFileTooBig)
			return
		}
		auth.WriteError(w, userErr(http.StatusBadRequest, "Unggahan tidak valid. Kirim ulang formulirnya."))
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := map[string]File{}
	for _, k := range []string{FileKTP, FileSelfie} {
		fhs := r.MultipartForm.File[k]
		if len(fhs) != 1 {
			auth.WriteError(w, errFileMissing)
			return
		}
		f, err := readPart(fhs[0])
		if err != nil {
			auth.WriteError(w, err)
			return
		}
		files[k] = f
	}
	v := auth.ViewerFrom(r.Context())
	sub, err := h.S.Submit(r.Context(), v.UserID, SubmitInput{
		NIK: r.FormValue("nik"), NameOnKTP: r.FormValue("nameOnKtp"), KTP: files[FileKTP], Selfie: files[FileSelfie],
	})
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "submission": sub})
}

// Status handles GET /api/v1/kyc/status: the caller's latest submission (null when none).
func (h Handlers) Status(w http.ResponseWriter, r *http.Request) {
	sub, found, err := h.S.Status(r.Context(), auth.ViewerFrom(r.Context()).UserID)
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	var out any
	if found {
		out = sub
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "submission": out})
}

// Queue handles GET /api/v1/admin/kyc?status=pending&limit=50.
func (h Handlers) Queue(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = StatusPending
	} else if status == "all" {
		status = ""
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.S.Queue(r.Context(), auth.ViewerFrom(r.Context()), status, limit)
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "submissions": list})
}

// GetOne handles GET /api/v1/admin/kyc/{id}.
func (h Handlers) GetOne(w http.ResponseWriter, r *http.Request) {
	sub, err := h.S.Get(r.Context(), auth.ViewerFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "submission": sub})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		auth.WriteError(w, userErr(http.StatusBadRequest, "Format permintaan tidak valid."))
		return false
	}
	return true
}

// Decide handles POST /api/v1/admin/kyc/{id}/decision {"decision":"approve|reject","reason":"…"}.
func (h Handlers) Decide(w http.ResponseWriter, r *http.Request) {
	var in struct{ Decision, Reason string }
	if !decodeJSON(w, r, &in) {
		return
	}
	sub, err := h.S.Decide(r.Context(), auth.ViewerFrom(r.Context()), r.PathValue("id"), in.Decision, in.Reason)
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "submission": sub})
}

// File handles GET /api/v1/admin/kyc/{id}/files/{ktp|selfie}. Every call is audited.
func (h Handlers) File(w http.ResponseWriter, r *http.Request) {
	data, ct, err := h.S.ReadFile(r.Context(), auth.ViewerFrom(r.Context()), r.PathValue("id"), r.PathValue("kind"))
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", ct)
	hd.Set("Content-Length", strconv.Itoa(len(data)))
	hd.Set("Cache-Control", "no-store, private")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Disposition", "inline")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Blacklist handles POST /api/v1/admin/kyc/blacklist {"userId":"…"} or {"nik":"…"}, plus "reason".
func (h Handlers) Blacklist(w http.ResponseWriter, r *http.Request) {
	var in struct{ UserID, NIK, Reason string }
	if !decodeJSON(w, r, &in) {
		return
	}
	v := auth.ViewerFrom(r.Context())
	var err error
	switch {
	case in.UserID != "":
		err = h.S.BlacklistUser(r.Context(), v, in.UserID, in.Reason)
	case in.NIK != "":
		err = h.S.BlacklistNIK(r.Context(), v, in.NIK, in.Reason)
	default:
		err = userErr(http.StatusBadRequest, "Pilih pengguna atau NIK yang mau diblokir.")
	}
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
