package oai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Test sends an explicit prompt and returns text only after response.completed.
// Empty model selects the first visible model in server order.
func (c *Client) Test(ctx context.Context, model, prompt string) (string, error) {
	if len(model) > 200 || len(prompt) > 64<<10 {
		return "", errors.New("model or prompt exceeds limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	v, e := openVault(c.dir, c.store, false)
	if e != nil {
		return "", e
	}
	defer v.close()
	if e = v.cleanup(ctx); e != nil {
		return "", e
	}
	creds, e := c.access(ctx, v)
	if e != nil {
		return "", e
	}
	if model == "" {
		model, e = c.model(ctx, creds.Access)
		if e != nil {
			return "", e
		}
	}
	body, _ := json.Marshal(struct {
		Model  string              `json:"model"`
		Input  []map[string]string `json:"input"`
		Store  bool                `json:"store"`
		Stream bool                `json:"stream"`
	}{Model: model, Input: []map[string]string{{"role": "user", "content": prompt}}, Stream: true})
	resp, e := c.request(ctx, "POST", c.api+"/responses", "application/json", creds.Access, bytes.NewReader(body))
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, e := bounded(resp.Body, 1<<20)
		if e != nil {
			return "", e
		}
		return "", remoteError(resp.StatusCode, resp.Header.Get("x-request-id"), b)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return "", errors.New("expected an OAI event stream")
	}
	return readStream(resp.Body, resp.Header.Get("x-request-id"))
}
func (c *Client) model(ctx context.Context, token string) (string, error) {
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if e := c.jsonRequest(ctx, "GET", c.api+"/models", "", token, nil, &catalog); e != nil {
		return "", e
	}
	for _, m := range catalog.Models {
		if m.Visibility == "list" && m.Slug != "" && len(m.Slug) <= 200 {
			return m.Slug, nil
		}
	}
	return "", errors.New("no visible models available for this account")
}
func readStream(r io.Reader, requestID string) (string, error) {
	// Bound the entire stream as well as individual lines/events and output.
	limited := &io.LimitedReader{R: r, N: 8<<20 + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var data, text strings.Builder
	consume := func() (bool, error) {
		if data.Len() == 0 {
			return false, nil
		}
		raw := data.String()
		data.Reset()
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Error    json.RawMessage `json:"error"`
			Code     string          `json:"code"`
			Response struct {
				Error json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			return false, errors.New("invalid OAI stream event")
		}
		switch event.Type {
		case "response.output_text.delta":
			if text.Len()+len(event.Delta) > 1<<20 {
				return false, errors.New("OAI reply exceeds limit")
			}
			text.WriteString(event.Delta)
		case "response.completed":
			if text.Len() == 0 {
				return false, errors.New("OAI response completed without text")
			}
			return true, nil
		case "response.failed":
			b, _ := json.Marshal(map[string]json.RawMessage{"error": event.Response.Error})
			return false, remoteError(200, requestID, b)
		case "error":
			return false, remoteError(200, requestID, []byte(raw))
		case "response.incomplete":
			return false, errors.New("OAI response incomplete; request was not retried")
		}
		return false, nil
	}
	for scanner.Scan() {
		if limited.N <= 0 {
			return "", errors.New("OAI stream exceeds limit")
		}
		line := scanner.Text()
		if line == "" {
			done, e := consume()
			if e != nil {
				return "", e
			}
			if done {
				return text.String(), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value) > 256<<10 {
				return "", errors.New("OAI event exceeds limit")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	return "", errors.New("OAI stream interrupted before response.completed")
}
