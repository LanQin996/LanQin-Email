package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

const googleTranslateEndpoint = "https://translate.googleapis.com/translate_a/single"

type translateMailMessageRequest struct {
	TargetLanguage string `json:"targetLanguage"`
}

type translateMailMessageResponse struct {
	TranslatedText string `json:"translatedText"`
	SourceLanguage string `json:"sourceLanguage,omitempty"`
	TargetLanguage string `json:"targetLanguage"`
	Truncated      bool   `json:"truncated"`
}

func (a *App) handleTranslateMailMessage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	msg, err := a.loadMessageForRequest(r, chi.URLParam(r, "id"), true)
	if err != nil {
		respondError(w, http.StatusNotFound, "message not found")
		return
	}
	text := strings.TrimSpace(msg.BodyText)
	if text == "" {
		text = strings.TrimSpace(msg.Snippet)
	}
	if text == "" {
		respondError(w, http.StatusBadRequest, "message has no translatable text")
		return
	}
	maxChars := a.cfg.MailTranslateMaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	text, truncated := truncateRunes(text, maxChars)
	translated, source, err := googleFreeTranslate(r.Context(), text, target)
	if err != nil {
		a.log.Warn("mail translation failed", "message_id", msg.ID, "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedText: translated, SourceLanguage: source, TargetLanguage: target, Truncated: truncated})
}

func (a *App) handleTranslateExternalIMAPMessage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.MailTranslateEnabled {
		respondError(w, http.StatusForbidden, "mail translation is disabled")
		return
	}
	var req translateMailMessageRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	target := normalizeTranslateTarget(req.TargetLanguage)
	if target == "" {
		respondError(w, http.StatusBadRequest, "unsupported target language")
		return
	}
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	stored, _, err := a.parseMaildirMessage(raw, account.Username)
	text := ""
	if err == nil {
		text = strings.TrimSpace(stored.BodyText)
		if text == "" {
			text = strings.TrimSpace(stored.Snippet)
		}
	}
	if text == "" {
		text = strings.TrimSpace(remote.Snippet)
	}
	if text == "" {
		respondError(w, http.StatusBadRequest, "message has no translatable text")
		return
	}
	maxChars := a.cfg.MailTranslateMaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	text, truncated := truncateRunes(text, maxChars)
	translated, source, err := googleFreeTranslate(r.Context(), text, target)
	if err != nil {
		a.log.Warn("external mail translation failed", "account_id", account.ID, "remote_id", chi.URLParam(r, "remoteId"), "target", target, "error", err)
		respondError(w, http.StatusBadGateway, "translation failed")
		return
	}
	respondJSON(w, http.StatusOK, translateMailMessageResponse{TranslatedText: translated, SourceLanguage: source, TargetLanguage: target, Truncated: truncated})
}

func normalizeTranslateTarget(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "zh", "zh-cn", "zh-hans", "zh_cn":
		return "zh-CN"
	case "zh-tw", "zh-hant", "zh_hk", "zh-hk", "zh-mo":
		return "zh-TW"
	case "en", "en-us", "en-gb":
		return "en"
	default:
		return ""
	}
}

func truncateRunes(value string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value, false
	}
	out := make([]rune, 0, max)
	for i, r := range value {
		if len(out) >= max {
			return string(out), i < len(value)
		}
		out = append(out, r)
	}
	return string(out), false
}

func googleFreeTranslate(ctx context.Context, text, target string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	params := url.Values{}
	params.Set("client", "gtx")
	params.Set("sl", "auto")
	params.Set("tl", target)
	params.Set("dt", "t")
	params.Set("q", text)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleTranslateEndpoint+"?"+params.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
		return "", "", fmt.Errorf("google translate status %d", res.StatusCode)
	}
	var raw any
	if err := json.NewDecoder(io.LimitReader(res.Body, 4*1024*1024)).Decode(&raw); err != nil {
		return "", "", err
	}
	translated, source := parseGoogleTranslateResponse(raw)
	translated = strings.TrimSpace(translated)
	if translated == "" {
		return "", source, errors.New("empty translation")
	}
	return translated, source, nil
}

func parseGoogleTranslateResponse(raw any) (string, string) {
	root, _ := raw.([]any)
	var b strings.Builder
	if len(root) > 0 {
		if sentences, ok := root[0].([]any); ok {
			for _, item := range sentences {
				parts, ok := item.([]any)
				if !ok || len(parts) == 0 {
					continue
				}
				if s, ok := parts[0].(string); ok {
					b.WriteString(s)
				}
			}
		}
	}
	source := ""
	if len(root) > 2 {
		if s, ok := root[2].(string); ok {
			source = s
		}
	}
	return b.String(), source
}
