package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (s *Service) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.RLock()
			accounts := make([]Account, 0, len(s.accounts))
			for _, account := range s.accounts {
				if account.Enabled {
					accounts = append(accounts, account)
				}
			}
			pools := make(map[string]Pool, len(s.pools))
			for model, pool := range s.pools {
				pools[model] = pool
			}
			s.mu.RUnlock()
			for _, account := range accounts {
				pool := pools[account.PublicModel]
				if pool.ProbeIntervalSecs == 0 {
					continue
				}
				interval := time.Duration(pool.ProbeIntervalSecs) * time.Second
				if interval <= 0 {
					interval = s.probeInterval
				}
				s.mu.RLock()
				last := s.probeLast[account.ID]
				s.mu.RUnlock()
				if (last.IsZero() || now.Sub(last) >= interval) && s.startProbe(ctx, account) {
					s.mu.Lock()
					s.probeLast[account.ID] = now
					s.mu.Unlock()
					break
				}
			}
		}
	}
}

func (s *Service) probeAccount(parent context.Context, account Account) {
	pool, ok := s.pool(account.PublicModel)
	if !ok || !account.Enabled {
		return
	}
	timeout := s.probeTimeout
	if pool.TimeoutMS > 0 && time.Duration(pool.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(pool.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"model":      account.UpstreamModel,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with OK."}},
		"max_tokens": 8,
		"stream":     true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(account.BaseURL), bytes.NewReader(payload))
	if err != nil {
		s.record(requestEvent{At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID, AccountName: account.Name, Probe: true, ErrorType: "invalid_upstream_url", ErrorMessage: safeError(err)})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+account.apiKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return
		}
		s.record(requestEvent{At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID, AccountName: account.Name, Probe: true, ErrorType: classifyError(err), ErrorMessage: safeError(err)})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = boundedRead(resp.Body, 512)
		s.record(requestEvent{At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID, AccountName: account.Name, Probe: true, Status: resp.StatusCode, ErrorType: classifyStatus(resp.StatusCode), ErrorMessage: "probe returned " + resp.Status})
		return
	}
	buf := make([]byte, 4096)
	first := float64(0)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 && first == 0 {
			first = float64(time.Since(started).Milliseconds())
		}
		if readErr == io.EOF {
			s.record(requestEvent{At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID, AccountName: account.Name, Probe: true, Status: resp.StatusCode, TTFTMS: first, TotalMS: float64(time.Since(started).Milliseconds()), Successful: true})
			return
		}
		if readErr != nil {
			if parent.Err() != nil {
				return
			}
			s.record(requestEvent{At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID, AccountName: account.Name, Probe: true, Status: resp.StatusCode, TTFTMS: first, ErrorType: classifyError(readErr), ErrorMessage: safeError(readErr)})
			return
		}
	}
}
