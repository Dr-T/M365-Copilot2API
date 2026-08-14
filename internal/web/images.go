package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/chathub"
	"net/http"
	"strings"
	"time"
)

type imageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	Model          string `json:"model"`
	AccountID      string `json:"accountId"`
	User           string `json:"user"`
}

func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var b imageGenerationRequest
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Prompt) == "" {
		http.Error(w, `{"error":{"message":"prompt is required","type":"invalid_request_error"}}`, 400)
		return
	}
	if b.N <= 0 {
		b.N = 1
	}
	if b.N > 10 {
		writeOpenAIError(w, 400, "invalid_request_error", "n must be between 1 and 10")
		return
	}
	if b.ResponseFormat != "" && !strings.EqualFold(b.ResponseFormat, "url") && !strings.EqualFold(b.ResponseFormat, "b64_json") {
		http.Error(w, `{"error":{"message":"response_format must be url or b64_json","type":"invalid_request_error"}}`, 400)
		return
	}
	acc, err := s.resolveAccount(firstNonEmpty(b.AccountID, b.User))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "account missing oid/tid — re-login with PKCE")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ImageTimeoutSeconds)*time.Second)
	defer cancel()
	size := b.Size
	if size == "" {
		size = "1024x1024"
	}
	prompt := fmt.Sprintf("Generate an image with the Flux model. Size: %s. Description: %s. Return the image URL directly.", size, b.Prompt)
	res, err := s.chat.Chat(ctx, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{Text: prompt, Tone: "magic"})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	log.Printf("[image-gen] conversation=%s images=%d text_len=%d events=%d raw_len=%d", res.ConversationID, len(res.Images), len(res.Text), len(res.Events), len(res.RawResult))
	if len(res.Images) == 0 {
		// Fallback: try to find image URLs in the raw result
		if urls := extractImageURLs(res.RawResult); len(urls) > 0 {
			res.Images = urls
		}
	}
	if len(res.Images) == 0 {
		// Fallback: try to find image URLs in the response text
		if urls := extractImageURLs(res.Text); len(urls) > 0 {
			res.Images = urls
		}
	}
	if len(res.Images) == 0 {
		// Debug: log the response to understand what the model returned
		textPreview := res.Text
		if len(textPreview) > 500 {
			textPreview = textPreview[:500]
		}
		rawPreview := ""
		if len(res.RawResult) > 0 {
			rawPreview = res.RawResult
			if len(rawPreview) > 500 {
				rawPreview = rawPreview[:500]
			}
		}
		debug := map[string]any{
			"text":        textPreview,
			"raw_len":     len(res.RawResult),
			"events":      len(res.Events),
			"images":      res.Images,
			"raw_preview": rawPreview,
		}
		b, _ := json.Marshal(debug)
		log.Printf("[image-gen-debug] %s", string(b))
		http.Error(w, `{"error":{"message":"upstream returned no image resource","type":"upstream_error"}}`, 502)
		return
	}
	images := res.Images
	if len(images) > b.N {
		images = images[:b.N]
	}
	data := make([]map[string]string, 0, len(images))
	for _, u := range images {
		if strings.EqualFold(b.ResponseFormat, "b64_json") {
			if strings.HasPrefix(u, "data:image/") {
				data = append(data, map[string]string{"b64_json": strings.SplitN(u, ",", 2)[1], "revised_prompt": b.Prompt})
			} else {
				b64, _, err := downloadImageAsBase64(u)
				if err != nil {
					log.Printf("[image-gen] download failed for b64_json: %v", err)
					data = append(data, map[string]string{"url": u, "revised_prompt": b.Prompt})
				} else {
					data = append(data, map[string]string{"b64_json": b64, "revised_prompt": b.Prompt})
				}
			}
		} else {
			data = append(data, map[string]string{"url": u, "revised_prompt": b.Prompt})
		}
	}
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		AccountEmail: acc.Email,
		Model:        firstNonEmpty(b.Model, "flux"),
		Endpoint:     "/v1/images/generations",
		InputTokens:  EstimateTokens(prompt),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	jsonOut(w, map[string]any{"created": time.Now().Unix(), "data": data, "m365": map[string]any{"conversationId": res.ConversationID, "sessionId": res.SessionID, "images": images}})
}

// extractImageURLs finds image URLs in a raw JSON string by searching for URL patterns.
func extractImageURLs(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				lk := strings.ToLower(k)
				if s, ok := e.(string); ok && (lk == "url" || lk == "imageurl" || lk == "thumbnailurl" || lk == "downloadurl" || lk == "src" || lk == "value" || lk == "data") {
					if strings.HasPrefix(s, "https://") && !seen[s] {
						if strings.Contains(strings.ToLower(s), "image") || strings.HasSuffix(strings.ToLower(s), ".png") || strings.HasSuffix(strings.ToLower(s), ".jpg") || strings.HasSuffix(strings.ToLower(s), ".jpeg") || strings.HasSuffix(strings.ToLower(s), ".webp") || strings.HasSuffix(strings.ToLower(s), ".gif") {
							seen[s] = true
							out = append(out, s)
						}
					}
				} else {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return out
}

func downloadImageAsBase64(url string) (b64, contentType string, err error) {
	return downloadImageAsBase64WithToken(url, "")
}

func downloadImageAsBase64WithToken(url, token string) (b64, contentType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("download returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	enc := base64.StdEncoding.EncodeToString(body)
	return enc, ct, nil
}

func downloadImageAsDataURI(url string) (string, error) {
	b64, ct, err := downloadImageAsBase64(url)
	if err != nil {
		return url, nil
	}
	return "data:" + ct + ";base64," + b64, nil
}

func downloadImageAsDataURIWithToken(url, token string) (string, error) {
	b64, ct, err := downloadImageAsBase64WithToken(url, token)
	if err != nil {
		log.Printf("[image-download] failed url=%s token_len=%d err=%v", url[:80], len(token), err)
		return url, nil
	}
	log.Printf("[image-download] ok url=%s ct=%s size=%d", url[:80], ct, len(b64))
	return "data:" + ct + ";base64," + b64, nil
}
