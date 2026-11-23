package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const maxRequestBytes = 8 << 20

func (s *Service) chatCompletions(c *gin.Context) {
	s.beginProxyRequest()
	defer s.endProxyRequest()
	started := time.Now()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		c.JSON(http.StatusRequestEntityTooLarge, openAIError("request_too_large", "request body exceeds 8 MiB"))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(400, openAIError("invalid_request_error", "request body must be a JSON object"))
		return
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		c.JSON(400, openAIError("invalid_request_error", "model is required"))
		return
	}
	pool, ok := s.pool(model)
	if !ok {
		c.JSON(404, openAIError("model_not_found", "model pool is not configured"))
		return
	}
	candidates := s.candidates(model, pool.Strategy)
	if len(candidates) == 0 {
		c.JSON(503, openAIError("no_upstream", "no enabled upstream account is configured for this model"))
		return
	}
	maxAttempts := pool.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = s.defaultAttempts
	}
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	timeout := time.Duration(pool.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = s.defaultTimeout
	}
	requestTimeout := time.Duration(pool.RequestTimeoutMS) * time.Millisecond
	if requestTimeout <= 0 {
		requestTimeout = s.defaultRequestTimeout
	}
	requestCtx, cancelRequest := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancelRequest()
	stream, _ := payload["stream"].(bool)
	attempted := make(map[string]bool)
	var lastStatus int
	var lastBody []byte
	var lastHeaders http.Header
	var lastErr error
	var lastKind string
	for attemptNo := 1; attemptNo <= maxAttempts; attemptNo++ {
		var account Account
		found := false
		for _, candidate := range candidates {
			if !attempted[candidate.ID] {
				account = candidate
				found = true
				break
			}
		}
		if !found {
			break
		}
		attempted[account.ID] = true
		attemptStart := time.Now()
		attemptCtx, cancel := context.WithTimeout(requestCtx, timeout)
		upReqBody := body
		if account.UpstreamModel != model {
			payload["model"] = account.UpstreamModel
			upReqBody, err = json.Marshal(payload)
			if err != nil {
				cancel()
				c.JSON(500, openAIError("gateway_error", "could not map model name"))
				return
			}
			payload["model"] = model
		}
		req, reqErr := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatURL(account.BaseURL), bytes.NewReader(upReqBody))
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			lastKind = "invalid_upstream_url"
			s.record(requestEvent{PublicModel: model, AccountID: account.ID, AccountName: account.Name, ErrorType: lastKind, ErrorMessage: safeError(reqErr), Attempts: attemptNo})
			continue
		}
		copyRequestHeaders(req.Header, c.Request.Header)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+account.apiKey)
		resp, doErr := s.httpClient.Do(req)
		if doErr != nil {
			cancel()
			lastErr = doErr
			lastKind = classifyError(doErr)
			s.record(requestEvent{PublicModel: model, AccountID: account.ID, AccountName: account.Name, Attempts: attemptNo, ErrorType: lastKind, ErrorMessage: safeError(doErr)})
			if requestCtx.Err() != nil {
				writeRequestTimeout(c, requestCtx, http.StatusGatewayTimeout)
				return
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastKind = classifyStatus(resp.StatusCode)
			if !retryableStatus(resp.StatusCode) {
				copyResponseHeaders(c.Writer.Header(), resp.Header)
				c.Status(resp.StatusCode)
				copyErr := copyResponse(c, resp.Body, stream)
				_ = resp.Body.Close()
				cancel()
				ev := requestEvent{At: time.Now().UTC(), PublicModel: model, AccountID: account.ID, AccountName: account.Name, Status: resp.StatusCode, Attempts: attemptNo, ErrorType: lastKind, ErrorMessage: "upstream returned " + formatAttemptStatus(resp.StatusCode)}
				if copyErr != nil {
					ev.ErrorMessage += ": " + safeError(copyErr)
				}
				select {
				case s.requestQueue <- ev:
				default:
				}
				return
			}
			lastStatus = resp.StatusCode
			lastHeaders = resp.Header.Clone()
			lastBody, _ = boundedRead(resp.Body, 8<<20)
			_ = resp.Body.Close()
			cancel()
			s.record(requestEvent{PublicModel: model, AccountID: account.ID, AccountName: account.Name, Status: resp.StatusCode, Attempts: attemptNo, ErrorType: lastKind, ErrorMessage: "upstream returned " + formatAttemptStatus(resp.StatusCode)})
			continue
		}
		first := make([]byte, 16*1024)
		firstN, firstErr := resp.Body.Read(first)
		if firstN == 0 && firstErr != nil && firstErr != io.EOF {
			_ = resp.Body.Close()
			cancel()
			lastErr = firstErr
			lastKind = classifyError(firstErr)
			s.record(requestEvent{PublicModel: model, AccountID: account.ID, AccountName: account.Name, Attempts: attemptNo, ErrorType: lastKind, ErrorMessage: safeError(firstErr)})
			if requestCtx.Err() != nil {
				writeRequestTimeout(c, requestCtx, http.StatusGatewayTimeout)
				return
			}
			continue
		}
		copyResponseHeaders(c.Writer.Header(), resp.Header)
		c.Status(resp.StatusCode)
		ttft := float64(0)
		if firstN > 0 {
			ttft = float64(time.Since(attemptStart).Milliseconds())
			_, _ = c.Writer.Write(first[:firstN])
			if stream {
				c.Writer.Flush()
			}
		}
		copyErr := error(nil)
		if firstErr != io.EOF {
			copyErr = copyResponse(c, resp.Body, stream)
		}
		_ = resp.Body.Close()
		cancel()
		total := float64(time.Since(attemptStart).Milliseconds())
		ev := requestEvent{At: time.Now().UTC(), PublicModel: model, AccountID: account.ID, AccountName: account.Name, Status: resp.StatusCode, TTFTMS: ttft, TotalMS: total, Attempts: attemptNo, Successful: copyErr == nil}
		if copyErr != nil {
			ev.ErrorType = "stream_read_error"
			ev.ErrorMessage = safeError(copyErr)
		}
		s.record(ev)
		return
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && c.Request.Context().Err() == nil {
		writeRequestTimeout(c, requestCtx, http.StatusGatewayTimeout)
		return
	}
	total := time.Since(started)
	if lastStatus != 0 && lastBody != nil {
		copyResponseHeaders(c.Writer.Header(), lastHeaders)
		c.Data(lastStatus, contentType(lastHeaders), lastBody)
		return
	}
	message := "all upstream attempts failed"
	if lastErr != nil {
		message = lastKind + " while contacting upstream"
	}
	s.record(requestEvent{At: time.Now().UTC(), PublicModel: model, Attempts: maxAttempts, TotalMS: float64(total.Milliseconds()), ErrorType: lastKind, ErrorMessage: message})
	c.JSON(http.StatusBadGateway, openAIError("upstream_unavailable", truncate(message, 500)))
}

func writeRequestTimeout(c *gin.Context, requestCtx context.Context, status int) {
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && c.Request.Context().Err() == nil && !c.Writer.Written() {
		c.JSON(status, openAIError("request_timeout", "request exceeded the model pool overall timeout"))
	}
}

func (s *Service) candidates(model, strategy string) []Account {
	s.mu.RLock()
	all := make([]Account, 0)
	now := time.Now()
	for _, a := range s.accounts {
		if a.PublicModel == model && a.Enabled {
			all = append(all, a)
		}
	}
	s.mu.RUnlock()
	if len(all) == 0 {
		return nil
	}
	ready := make([]Account, 0, len(all))
	cooling := make([]Account, 0)
	for _, a := range all {
		if a.CooldownUntil.After(now) {
			cooling = append(cooling, a)
		} else {
			ready = append(ready, a)
		}
	}
	if len(ready) == 0 {
		sort.Slice(cooling, func(i, j int) bool { return cooling[i].CooldownUntil.Before(cooling[j].CooldownUntil) })
		ready = cooling
	}
	if strategy == "fastest" {
		sort.SliceStable(ready, func(i, j int) bool {
			a, b := ready[i], ready[j]
			if a.Samples == 0 && b.Samples == 0 {
				return a.Priority < b.Priority
			}
			if a.Samples == 0 {
				return false
			}
			if b.Samples == 0 {
				return true
			}
			sa, sb := latencyScore(a), latencyScore(b)
			if mathAbs(sa-sb) < 1 {
				return a.Priority < b.Priority
			}
			return sa < sb
		})
	} else {
		sort.SliceStable(ready, func(i, j int) bool { return ready[i].Priority < ready[j].Priority })
	}
	return ready
}

func latencyScore(a Account) float64 {
	if a.Samples == 0 {
		return 1e15 + float64(a.Priority)
	}
	// TTFT matters to streaming clients; complete response time dominates total perceived wait.
	return .35*a.TTFTEWMA + .65*a.TotalEWMA
}

func copyResponse(c *gin.Context, body io.Reader, flush bool) error {
	buf := make([]byte, 16*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				return werr
			}
			if flush {
				c.Writer.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func copyRequestHeaders(dst, src http.Header) {
	hop := map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Authorization": true, "Host": true, "Content-Length": true}
	for k, values := range src {
		if hop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	if src == nil {
		return
	}
	hop := map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true}
	for k, values := range src {
		if hop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func chatURL(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/chat/completions") {
		return b
	}
	return b + "/chat/completions"
}
func contentType(h http.Header) string {
	v := h.Get("Content-Type")
	if v == "" {
		return "application/json"
	}
	return v
}
func openAIError(kind, message string) gin.H {
	return gin.H{"error": gin.H{"type": kind, "message": message}}
}
func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "connection_error"
}
func classifyStatus(status int) string {
	switch status {
	case 401, 403:
		return "auth_error"
	case 408:
		return "timeout"
	case 429:
		return "rate_limit"
	default:
		if status >= 500 {
			return "upstream_5xx"
		}
		return "upstream_4xx"
	}
}
func retryableStatus(status int) bool {
	return status == 401 || status == 403 || status == 408 || status == 429 || status >= 500
}
func mathAbs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(time.Now()) {
		return time.Until(t)
	}
	return 0
}
func formatAttemptStatus(status int) string { return fmt.Sprintf("HTTP %d", status) }
