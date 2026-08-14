package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	HTTPBase    string `json:"http_base"`
	SessionFile string `json:"session_file"`
	CallID      string `json:"call_id"`
	OutputFile  string `json:"output_file"`
	AuditFile   string `json:"audit_file"`
}

type session struct {
	AccessToken string `json:"access_token"`
}

type productEvent struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func main() {
	if len(os.Args) != 2 {
		fatal(errors.New("usage: accept-existing-events CONFIG_JSON"))
	}
	var cfg config
	var auth session
	if err := readJSON(os.Args[1], &cfg); err != nil {
		fatal(err)
	}
	if err := readJSON(cfg.SessionFile, &auth); err != nil {
		fatal(err)
	}
	if strings.TrimSpace(cfg.HTTPBase) == "" || strings.TrimSpace(auth.AccessToken) == "" ||
		strings.TrimSpace(cfg.CallID) == "" || cfg.OutputFile == "" || cfg.AuditFile == "" {
		fatal(errors.New("invalid global SSE acceptance config"))
	}
	out, err := os.OpenFile(cfg.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	first, scanner, err := openEvents(ctx, cfg.HTTPBase, auth.AccessToken, 0)
	if err != nil {
		fatal(err)
	}
	created, err := action(ctx, cfg.HTTPBase, auth.AccessToken, "calls.create", map[string]any{
		"call_id": cfg.CallID, "media_type": "voice",
	})
	if err != nil {
		first.Body.Close()
		fatal(err)
	}
	if stringValue(created["call_id"]) != cfg.CallID || stringValue(created["state"]) != "ringing" {
		first.Body.Close()
		fatal(errors.New("calls.create returned an unexpected call"))
	}
	createdEvent, err := readMatchingCall(scanner, cfg.CallID, "ringing", out)
	first.Body.Close()
	if err != nil {
		fatal(err)
	}
	ended, err := action(ctx, cfg.HTTPBase, auth.AccessToken, "calls.event", map[string]any{
		"call_id": cfg.CallID, "event": "ended", "reason": "acceptance_complete",
	})
	if err != nil {
		fatal(err)
	}
	if stringValue(ended["call_id"]) != cfg.CallID || stringValue(ended["state"]) != "ended" {
		fatal(errors.New("calls.event did not terminate the acceptance call"))
	}
	second, scanner, err := openEvents(ctx, cfg.HTTPBase, auth.AccessToken, createdEvent.Seq)
	if err != nil {
		fatal(err)
	}
	endedEvent, err := readMatchingCall(scanner, cfg.CallID, "ended", out)
	second.Body.Close()
	if err != nil {
		fatal(err)
	}
	if endedEvent.Seq <= createdEvent.Seq {
		fatal(errors.New("global SSE resume did not advance the cursor"))
	}
	loaded, err := action(ctx, cfg.HTTPBase, auth.AccessToken, "calls.get", map[string]any{"call_id": cfg.CallID})
	if err != nil {
		fatal(err)
	}
	if stringValue(loaded["call_id"]) != cfg.CallID || stringValue(loaded["state"]) != "ended" {
		fatal(errors.New("calls.get did not read back the terminal call"))
	}
	audit := map[string]any{
		"product_action_post_count": 3,
		"calls_create_post_count":   1,
		"calls_event_post_count":    1,
		"calls_get_post_count":      1,
		"event_watch_get_count":     2,
		"call_id":                   cfg.CallID,
		"created_seq":               createdEvent.Seq,
		"ended_seq":                 endedEvent.Seq,
		"terminal_state":            "ended",
	}
	if err := writeJSON(cfg.AuditFile, audit); err != nil {
		fatal(err)
	}
}

func openEvents(ctx context.Context, base, token string, after int64) (*http.Response, *bufio.Scanner, error) {
	endpoint := strings.TrimRight(base, "/") + "/_p2p/events?after_seq=" + strconv.FormatInt(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		defer response.Body.Close()
		return nil, nil, fmt.Errorf("global events returned HTTP %d", response.StatusCode)
	}
	return response, bufio.NewScanner(response.Body), nil
}

func readMatchingCall(scanner *bufio.Scanner, callID, state string, out io.Writer) (productEvent, error) {
	for {
		id, eventName, data, ok, err := readFrame(scanner)
		if err != nil {
			return productEvent{}, err
		}
		if !ok {
			return productEvent{}, errors.New("global SSE closed before the expected call.changed event")
		}
		var event productEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return productEvent{}, err
		}
		if event.Seq != id || event.Type != eventName {
			return productEvent{}, errors.New("global SSE metadata does not match the data envelope")
		}
		if event.Type != "call.changed" {
			continue
		}
		call, _ := event.Payload["call"].(map[string]any)
		if stringValue(call["call_id"]) != callID || stringValue(call["state"]) != state {
			continue
		}
		if err := json.NewEncoder(out).Encode(event); err != nil {
			return productEvent{}, err
		}
		return event, nil
	}
}

func readFrame(scanner *bufio.Scanner) (int64, string, []byte, bool, error) {
	var id int64
	var event string
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) == 0 {
				continue
			}
			return id, event, data, true, nil
		}
		key, value, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, ":") {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			id, _ = strconv.ParseInt(value, 10, 64)
		case "event":
			event = value
		case "data":
			data = append(data, value...)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, "", nil, false, err
	}
	return 0, "", nil, false, nil
}

func action(ctx context.Context, base, token, name string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"action": name, "params": params})
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(base, "/") + "/_p2p/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned HTTP %d", name, response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func writeJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "global SSE acceptance:", err)
	os.Exit(1)
}
