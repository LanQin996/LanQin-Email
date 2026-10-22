package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const maxFaviconSize = 256 * 1024

func (a *App) faviconPath() string {
	return filepath.Join(a.cfg.DataDir, "favicon.ico")
}

func (a *App) handleGetFavicon(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(a.faviconPath())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read favicon")
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeContent(w, r, "favicon.ico", info.ModTime(), file)
}

func (a *App) handleUploadFavicon(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFaviconSize+1024)
	if err := r.ParseMultipartForm(maxFaviconSize); err != nil {
		badRequest(w, errors.New("invalid or oversized ICO file"))
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		badRequest(w, errors.New("ICO file is required"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFaviconSize+1))
	if err != nil || len(data) > maxFaviconSize || !validICO(data) {
		badRequest(w, errors.New("file must be a valid ICO under 256 KB"))
		return
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0o755); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create favicon directory")
		return
	}
	tmp := a.faviconPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to store favicon")
		return
	}
	if err := os.Rename(tmp, a.faviconPath()); err != nil {
		_ = os.Remove(tmp)
		respondError(w, http.StatusInternalServerError, "failed to store favicon")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleDeleteFavicon(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(a.faviconPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		respondError(w, http.StatusInternalServerError, "failed to remove favicon")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func validICO(data []byte) bool {
	if len(data) < 22 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
		return false
	}
	count := int(data[4]) | int(data[5])<<8
	if count == 0 || 6+count*16 > len(data) {
		return false
	}
	for index := 0; index < count; index++ {
		entry := data[6+index*16 : 6+(index+1)*16]
		size := int(entry[8]) | int(entry[9])<<8 | int(entry[10])<<16 | int(entry[11])<<24
		offset := int(entry[12]) | int(entry[13])<<8 | int(entry[14])<<16 | int(entry[15])<<24
		if size <= 0 || offset < 6+count*16 || offset > len(data) || size > len(data)-offset {
			return false
		}
	}
	return true
}
